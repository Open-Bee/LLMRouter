package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"llm-router/internal/config"
	"llm-router/internal/handler"
	"llm-router/internal/health"
	"llm-router/internal/model"
	"llm-router/internal/monitor"
	"llm-router/internal/notify"
	"llm-router/internal/pool"
	"llm-router/internal/ratelimit"
	"llm-router/internal/router"
	"llm-router/internal/routing"
	"llm-router/internal/util"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"
)

func main() {
	// Parse command line arguments
	configPath := flag.String("config", "config.yaml", "Path to the main configuration file")
	logLevel := flag.String("log-level", "", "Log level override (debug, info, warn, error)")
	enabledSources := flag.String("enabled-sources", "", "Comma-separated enabled source types (overrides config.yaml, e.g. openai)")
	flag.Parse()

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Setup logger
	level := cfg.Log.Level
	if *logLevel != "" {
		level = *logLevel
	}
	logRing := monitor.NewLogRing(500)
	logger, logCleanup := setupLogger(level, cfg.Log, logRing)
	defer logCleanup()

	// CLI overrides
	if *enabledSources != "" {
		var sources []string
		for _, s := range strings.Split(*enabledSources, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				sources = append(sources, s)
			}
		}
		cfg.Backends.EnabledSources = sources
	}

	logger.Info().Str("config", *configPath).Msg("starting LLM Router")

	// Create context with cancellation for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Step 1: Create BackendPool
	backendPool := pool.NewBackendPool()

	// Step 2: Load backend configs and populate pool
	loadBackends(cfg, backendPool, logger)

	// Step 3: Initialize components
	rateLimiter := ratelimit.NewRateLimiter()
	stats := util.NewRequestStats()

	// Metrics monitor
	metricsMonitor := monitor.NewMetricsMonitor(
		backendPool,
		time.Duration(cfg.Monitor.CollectInterval)*time.Second,
		logger,
	)
	monitorMgr := monitor.NewManager(backendPool, metricsMonitor, logger)

	// Completion RPM observer
	completionRPM := monitorMgr.CompletionRPM

	// Routing strategy
	strategy := routing.NewStrategy(cfg.Routing.Strategy, monitorMgr)
	logger.Info().Str("strategy", strategy.Name()).Msg("routing strategy selected")

	// HTTP client with connection pooling
	httpClient := createHTTPClient(cfg)

	// Router manager
	routerMgr := router.NewManager(
		backendPool,
		strategy,
		rateLimiter,
		completionRPM,
		stats,
		httpClient,
		logger,
		router.WithMaxRetries(cfg.Routing.NumRetries),
		router.WithBackoff(cfg.Retry.BaseDelay, cfg.Retry.MaxDelay, cfg.Retry.JitterFactor),
		router.WithFailureThreshold(cfg.Retry.FailureThreshold),
	)

	// Health checker
	healthChecker := health.NewChecker(
		backendPool,
		time.Duration(cfg.HealthCheck.Interval)*time.Second,
		time.Duration(cfg.HealthCheck.Timeout)*time.Second,
		cfg.HealthCheck.FailureThreshold,
		cfg.HealthCheck.MaxConcurrency,
		logger,
	)

	// Config watcher
	configWatcher := health.NewConfigWatcher(
		backendPool,
		cfg.Backends.ConfigDir,
		cfg.Backends.EnabledSources,
		time.Duration(cfg.HealthCheck.Interval)*time.Second,
		logger,
	)

	// Webhook notifier
	var notifier *notify.Notifier
	if cfg.Webhook.Enabled && cfg.Webhook.URL != "" {
		notifier = notify.NewNotifier(
			backendPool,
			cfg.Webhook.URL,
			cfg.Webhook.CooldownSeconds,
			cfg.Webhook.BatchInterval,
			cfg.Webhook.ReportIntervalActive,
			cfg.Webhook.ReportIntervalIdle,
			logger,
		)
		// Register as observer for health changes
		healthChecker.OnStatusChange(func(change health.StatusChange) {
			notifier.HandleStatusChange(change.Backend.ID, change.Healthy, change.Backend)
		})
	}

	// Web dashboard (always enabled)
	webPort := cfg.Dashboard.WebPort
	if webPort <= 0 {
		webPort = 80
	}
	webDashboard := monitor.NewWebDashboard(
		backendPool,
		rateLimiter,
		completionRPM,
		stats,
		fmt.Sprintf("0.0.0.0:%d", webPort),
		logger,
	)

	// Step 4: Start background goroutines
	go healthChecker.Run(ctx)
	go configWatcher.Run(ctx)
	go monitorMgr.Run(ctx)

	if notifier != nil {
		go notifier.RunPeriodicReport(ctx)
	}

	// Step 5: Run initial health check
	logger.Info().Msg("running initial health check...")
	totalBefore := len(backendPool.AllBackends())
	unhealthyBackends := healthChecker.InitialCheckAll(ctx)
	healthy := totalBefore - len(unhealthyBackends)
	logger.Info().Int("total", totalBefore).Int("healthy", healthy).Msg("initial health check complete")

	// Step 5.1: Optionally prune unhealthy backends from pool and config files
	if cfg.HealthCheck.PruneUnhealthyOnStart && len(unhealthyBackends) > 0 {
		pruneUnhealthyBackends(cfg, backendPool, unhealthyBackends, configWatcher, logger)
	}

	// Send startup report
	if notifier != nil {
		notifier.SendStartupReport(ctx)
	}

	// Step 6: Setup HTTP routes
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	chatHandler := handler.NewChatHandler(routerMgr, logger)
	modelsHandler := handler.NewModelsHandler(backendPool, logger)
	healthHandler := handler.NewHealthHandler()

	r.Post("/v1/chat/completions", chatHandler.ServeHTTP)
	r.Get("/v1/models", modelsHandler.ServeHTTP)
	r.Get("/health", healthHandler.ServeHTTP)

	// Step 7: Start HTTP server
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	go func() {
		logger.Info().Str("addr", addr).Msg("HTTP server listening")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal().Err(err).Msg("HTTP server error")
		}
	}()

	// Step 8: Start web dashboard
	go webDashboard.Run(ctx)

	// Step 9: Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	logger.Info().Str("signal", sig.String()).Msg("shutting down...")

	// Graceful shutdown
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error().Err(err).Msg("HTTP server shutdown error")
	}

	logger.Info().Msg("LLM Router stopped")
}

// loadBackends loads all backend configuration files and populates the pool.
// Returns the raw config map (filePath -> BackendConfig) for later use (e.g. pruning unhealthy backends).
func loadBackends(cfg *config.Config, p *pool.BackendPool, logger zerolog.Logger) map[string]*model.BackendConfig {
	configs, err := config.LoadBackendConfigs(cfg.Backends.ConfigDir, cfg.Backends.EnabledSources)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to load backend configs")
	}

	var allBackends []*model.Backend
	for filePath, backendCfg := range configs {
		for _, ep := range backendCfg.ModelList {
			b := model.NewBackend(ep)
			allBackends = append(allBackends, b)
		}
		logger.Info().
			Str("file", filePath).
			Int("backends", len(backendCfg.ModelList)).
			Msg("loaded backend config")
	}

	p.AddBackends(allBackends)
	logger.Info().Int("total", len(allBackends)).Msg("all backends loaded")
	return configs
}

// pruneUnhealthyBackends removes unhealthy backends from the pool and rewrites
// the corresponding config files so that bad nodes don't reappear on next startup.
func pruneUnhealthyBackends(
	cfg *config.Config,
	p *pool.BackendPool,
	unhealthy []*model.Backend,
	configWatcher *health.ConfigWatcher,
	logger zerolog.Logger,
) {
	// Build set of unhealthy backend IDs
	unhealthyIDs := make(map[string]bool, len(unhealthy))
	// Also index by APIBase for matching against config file endpoints
	unhealthyAPIs := make(map[string]bool, len(unhealthy))
	for _, b := range unhealthy {
		unhealthyIDs[b.ID] = true
		unhealthyAPIs[b.APIBase] = true
		logger.Warn().Str("backend", b.ID).Msg("pruning unhealthy backend")
	}

	// Remove from pool
	p.RemoveBackends(unhealthyIDs)

	// Rewrite config files: load each file, filter out unhealthy endpoints, write back
	configs, err := config.LoadBackendConfigs(cfg.Backends.ConfigDir, cfg.Backends.EnabledSources)
	if err != nil {
		logger.Error().Err(err).Msg("failed to load backend configs for pruning")
		return
	}

	for filePath, backendCfg := range configs {
		var kept []model.ServiceEndpoint
		pruned := 0
		for _, ep := range backendCfg.ModelList {
			if unhealthyAPIs[ep.LiteLLMParams.APIBase] {
				pruned++
				continue
			}
			kept = append(kept, ep)
		}
		if pruned == 0 {
			continue
		}

		// Suppress config watcher to avoid reload loop from our own write
		configWatcher.SuppressFile(filePath, 30*time.Second)

		backendCfg.ModelList = kept
		if err := config.WriteBackendConfigFile(filePath, backendCfg); err != nil {
			logger.Error().Err(err).Str("file", filePath).Msg("failed to write pruned config")
		} else {
			logger.Info().
				Str("file", filePath).
				Int("pruned", pruned).
				Int("remaining", len(kept)).
				Msg("pruned unhealthy backends from config file")
		}
	}

	logger.Info().Int("pruned", len(unhealthy)).Msg("startup pruning complete")
}

// createHTTPClient creates an HTTP client with connection pooling optimized for proxying.
func createHTTPClient(cfg *config.Config) *http.Client {
	// When MaxConnsPerHost is 0 (unlimited), Go's http.Transport treats
	// MaxIdleConnsPerHost=0 as DefaultMaxIdleConnsPerHost (which is only 2).
	// For high-concurrency LLM proxying we need many idle connections per host.
	maxIdlePerHost := cfg.HTTPClient.MaxConnsPerHost
	if maxIdlePerHost <= 0 {
		maxIdlePerHost = cfg.HTTPClient.MaxIdleConns // match the global idle pool size
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(cfg.HTTPClient.ConnectTimeout) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        cfg.HTTPClient.MaxIdleConns,
		MaxConnsPerHost:     cfg.HTTPClient.MaxConnsPerHost,
		MaxIdleConnsPerHost: maxIdlePerHost,
		IdleConnTimeout:     time.Duration(cfg.HTTPClient.IdleConnTimeout) * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableCompression:  true,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   time.Duration(cfg.HTTPClient.RequestTimeout) * time.Second,
	}
}

// setupLogger configures zerolog with dual output:
//   - Console (stderr): human-readable at the configured level
//   - File (logs/ dir): JSON at debug level for detailed debugging
//
// Returns the logger and a cleanup function to close the log file.
func setupLogger(level string, logCfg config.LogConfig, extraWriters ...io.Writer) (zerolog.Logger, func()) {
	var lvl zerolog.Level
	switch level {
	case "debug":
		lvl = zerolog.DebugLevel
	case "warn":
		lvl = zerolog.WarnLevel
	case "error":
		lvl = zerolog.ErrorLevel
	default:
		lvl = zerolog.InfoLevel
	}

	// Console writer (stderr) at the configured level
	consoleWriter := zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: "15:04:05",
	}

	// If no log dir configured, console-only
	if logCfg.Dir == "" {
		logger := zerolog.New(consoleWriter).
			Level(lvl).
			With().
			Timestamp().
			Logger()
		return logger, func() {}
	}

	// Create logs directory
	if err := os.MkdirAll(logCfg.Dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to create log dir %s: %v\n", logCfg.Dir, err)
		logger := zerolog.New(consoleWriter).
			Level(lvl).
			With().
			Timestamp().
			Logger()
		return logger, func() {}
	}

	// Open log file with date+hour-minute name
	logFileName := fmt.Sprintf("router_%s.log", time.Now().Format("2006-01-02_1504"))
	logFilePath := filepath.Join(logCfg.Dir, logFileName)
	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to open log file %s: %v\n", logFilePath, err)
		logger := zerolog.New(consoleWriter).
			Level(lvl).
			With().
			Timestamp().
			Logger()
		return logger, func() {}
	}

	// File writer: JSON format at info level (skip per-request debug noise)
	// Console writer: human-readable at configured level
	// Use a level-filtered writer so console only sees >= configured level,
	// while file gets everything >= info.
	fileWriter := newLevelWriter(logFile, zerolog.InfoLevel)
	consoleFiltered := newLevelWriter(consoleWriter, lvl)

	ioWriters := []io.Writer{consoleFiltered, fileWriter}
	for _, ew := range extraWriters {
		ioWriters = append(ioWriters, newLevelWriter(ew, lvl))
	}
	multi := zerolog.MultiLevelWriter(ioWriters...)

	logger := zerolog.New(multi).
		Level(zerolog.DebugLevel). // global level = debug; per-writer filtering handles the rest
		With().
		Timestamp().
		Caller(). // include caller info in file logs for easier debugging
		Logger()

	logger.Info().
		Str("log_file", logFilePath).
		Str("console_level", level).
		Str("file_level", "info").
		Msg("logging initialized")

	// Clean old log files on startup
	go cleanOldLogs(logCfg.Dir, logCfg.MaxBackups, logCfg.MaxAgeDays)

	cleanup := func() {
		logFile.Close()
	}

	return logger, cleanup
}

// cleanOldLogs removes old log files exceeding max_backups or max_age_days.
func cleanOldLogs(dir string, maxBackups, maxAgeDays int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	type logEntry struct {
		name    string
		modTime time.Time
	}

	var logs []logEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 4 || name[len(name)-4:] != ".log" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		logs = append(logs, logEntry{name: name, modTime: info.ModTime()})
	}

	// Sort newest first
	for i := 0; i < len(logs); i++ {
		for j := i + 1; j < len(logs); j++ {
			if logs[j].modTime.After(logs[i].modTime) {
				logs[i], logs[j] = logs[j], logs[i]
			}
		}
	}

	cutoff := time.Now().AddDate(0, 0, -maxAgeDays)

	for i, l := range logs {
		// Remove if exceeds max backups or is too old
		if i >= maxBackups || l.modTime.Before(cutoff) {
			os.Remove(filepath.Join(dir, l.name))
		}
	}
}

// levelWriter wraps an io.Writer and only writes messages at or above a minimum level.
type levelWriter struct {
	w        io.Writer
	minLevel zerolog.Level
}

func newLevelWriter(w io.Writer, minLevel zerolog.Level) *levelWriter {
	return &levelWriter{w: w, minLevel: minLevel}
}

func (lw *levelWriter) Write(p []byte) (n int, err error) {
	return lw.w.Write(p)
}

// WriteLevel implements zerolog.LevelWriter, filtering by minimum level.
func (lw *levelWriter) WriteLevel(level zerolog.Level, p []byte) (n int, err error) {
	if level >= lw.minLevel {
		return lw.w.Write(p)
	}
	return len(p), nil
}

