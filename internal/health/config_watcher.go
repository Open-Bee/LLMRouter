package health

import (
	"context"
	"net/http"
	"os"
	"sync"
	"time"

	"llm-router/internal/config"
	"llm-router/internal/model"
	"llm-router/internal/pool"
	"llm-router/internal/util"

	"github.com/rs/zerolog"
)

// ConfigWatcher monitors backend configuration files for changes and
// triggers hot-reload of the BackendPool.
//
// On config change it:
//  1. Builds new backend objects for newly discovered endpoints
//  2. Health-checks all new backends in parallel
//  3. Atomically swaps backends of that source type (preserving runtime
//     state for backends that already existed), so there is never a moment
//     with zero available backends.
type ConfigWatcher struct {
	pool      *pool.BackendPool
	configDir string
	enabled   []string
	interval  time.Duration
	logger    zerolog.Logger

	healthClient  *http.Client
	healthTimeout time.Duration
	maxConcurrency int

	mu           sync.Mutex
	lastModTimes map[string]time.Time
	suppressUntil map[string]time.Time // suppress changes written by scanner
}

// NewConfigWatcher creates a new ConfigWatcher.
func NewConfigWatcher(
	p *pool.BackendPool,
	configDir string,
	enabledSources []string,
	interval time.Duration,
	logger zerolog.Logger,
) *ConfigWatcher {
	healthTimeout := 10 * time.Second
	return &ConfigWatcher{
		pool:           p,
		configDir:      configDir,
		enabled:        enabledSources,
		interval:       interval,
		logger:         logger.With().Str("component", "config_watcher").Logger(),
		healthClient:   &http.Client{Timeout: healthTimeout},
		healthTimeout:  healthTimeout,
		maxConcurrency: 20,
		lastModTimes:   make(map[string]time.Time),
		suppressUntil:  make(map[string]time.Time),
	}
}

// SuppressFile temporarily suppresses change detection for a file.
// Used by the scanner after writing config files to avoid triggering a reload loop.
func (w *ConfigWatcher) SuppressFile(filePath string, duration time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.suppressUntil[filePath] = time.Now().Add(duration)
}

// Run starts the config watch loop. Blocks until ctx is cancelled.
func (w *ConfigWatcher) Run(ctx context.Context) {
	w.logger.Info().Str("dir", w.configDir).Msg("starting config watcher")

	// Initialize mod times
	w.initModTimes()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info().Msg("config watcher stopped")
			return
		case <-ticker.C:
			w.checkChanges()
		}
	}
}

func (w *ConfigWatcher) initModTimes() {
	configs, err := config.LoadBackendConfigs(w.configDir, w.enabled)
	if err != nil {
		w.logger.Error().Err(err).Msg("failed to load initial configs")
		return
	}

	for filePath := range configs {
		info, err := os.Stat(filePath)
		if err == nil {
			w.lastModTimes[filePath] = info.ModTime()
		}
	}
}

func (w *ConfigWatcher) checkChanges() {
	configs, err := config.LoadBackendConfigs(w.configDir, w.enabled)
	if err != nil {
		w.logger.Error().Err(err).Msg("failed to load configs for change detection")
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	now := time.Now()
	for filePath, cfg := range configs {
		// Check suppression
		if suppress, ok := w.suppressUntil[filePath]; ok && now.Before(suppress) {
			continue
		}

		info, err := os.Stat(filePath)
		if err != nil {
			continue
		}

		lastMod, known := w.lastModTimes[filePath]
		if known && !info.ModTime().After(lastMod) {
			continue
		}

		w.lastModTimes[filePath] = info.ModTime()
		w.logger.Info().Str("file", filePath).Msg("config file changed, reloading")
		w.applyChanges(cfg)
	}
}

func (w *ConfigWatcher) applyChanges(cfg *model.BackendConfig) {
	// Determine the source type from the config entries
	sourceType := ""
	if len(cfg.ModelList) > 0 {
		sourceType = cfg.ModelList[0].LiteLLMParams.SourceType
	}
	if sourceType == "" {
		return
	}

	// Build desired backend set from config
	desiredByID := make(map[string]model.ServiceEndpoint)
	for _, ep := range cfg.ModelList {
		b := model.NewBackend(ep)
		desiredByID[b.ID] = ep
	}

	// Get current backends of this source type, indexed by ID
	currentBackends := w.pool.GetBackendsBySourceType(sourceType)
	currentByID := make(map[string]*model.Backend)
	for _, b := range currentBackends {
		currentByID[b.ID] = b
	}

	// Separate into: reuse (existing, update mutable fields) vs brand-new (need health check)
	var reused []*model.Backend
	var brandNew []*model.Backend

	for id, ep := range desiredByID {
		if existing, ok := currentByID[id]; ok {
			// Reuse existing backend — preserve runtime state (connections, health, metrics)
			weight := ep.LiteLLMParams.Weight
			if weight == 0 {
				weight = 1.0
			}
			existing.Weight = weight
			existing.RPMLimit = ep.LiteLLMParams.RPMLimit
			reused = append(reused, existing)
		} else {
			// Brand-new backend — create with healthy=false, needs health check
			b := model.NewBackend(ep)
			b.SetHealthy(false)
			brandNew = append(brandNew, b)
		}
	}

	removedCount := 0
	for id := range currentByID {
		if _, ok := desiredByID[id]; !ok {
			removedCount++
		}
	}

	// Initial health-check for brand-new backends
	if len(brandNew) > 0 {
		w.logger.Info().Int("count", len(brandNew)).Str("source", sourceType).Msg("health-checking new backends")
		w.healthCheckBackends(brandNew)

		healthyNew := 0
		for _, b := range brandNew {
			if b.IsHealthy() {
				healthyNew++
			}
		}
		w.logger.Info().
			Int("healthy", healthyNew).
			Int("total", len(brandNew)).
			Str("source", sourceType).
			Msg("new backends health-checked")
	}

	// Build the final list: reused + brand-new (including unhealthy ones so they
	// can be retried by the periodic health checker)
	finalBackends := make([]*model.Backend, 0, len(reused)+len(brandNew))
	finalBackends = append(finalBackends, reused...)
	finalBackends = append(finalBackends, brandNew...)

	// Atomic swap — old backends of this source type are replaced in one shot.
	// Requests in flight on old backends continue on their existing *Backend
	// pointers (the pool snapshot is immutable); new requests pick up the new snapshot.
	w.pool.ReplaceBySourceType(sourceType, finalBackends)

	w.logger.Info().
		Int("reused", len(reused)).
		Int("added", len(brandNew)).
		Int("removed", removedCount).
		Int("total", len(finalBackends)).
		Str("source", sourceType).
		Msg("backends replaced atomically")

	// Background retry: if any new backends failed the initial health check,
	// keep retrying at short intervals so they start receiving traffic as soon
	// as they become ready, rather than waiting for the next 30s periodic check.
	var unhealthyNew []*model.Backend
	for _, b := range brandNew {
		if !b.IsHealthy() {
			unhealthyNew = append(unhealthyNew, b)
		}
	}
	if len(unhealthyNew) > 0 {
		go w.retryHealthCheck(unhealthyNew, sourceType)
	}
}

// retryHealthCheck keeps probing unhealthy new backends at short intervals
// (every 10s, up to ~2 minutes) so they start receiving traffic as soon as
// they become ready. This runs in a background goroutine after the pool swap.
func (w *ConfigWatcher) retryHealthCheck(backends []*model.Backend, sourceType string) {
	const maxRetries = 24
	const retryInterval = 5 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		time.Sleep(retryInterval)

		var unhealthy []*model.Backend
		for _, b := range backends {
			if !b.IsHealthy() {
				unhealthy = append(unhealthy, b)
			}
		}
		if len(unhealthy) == 0 {
			w.logger.Info().Str("source", sourceType).Msg("all new backends are now healthy")
			return
		}

		w.logger.Info().
			Int("attempt", attempt).
			Int("remaining", len(unhealthy)).
			Str("source", sourceType).
			Msg("retrying health check for unhealthy new backends")
		w.healthCheckBackends(unhealthy)
	}

	// Log any that never became healthy
	var still []*model.Backend
	for _, b := range backends {
		if !b.IsHealthy() {
			still = append(still, b)
		}
	}
	if len(still) > 0 {
		ids := make([]string, len(still))
		for i, b := range still {
			ids[i] = b.ID
		}
		w.logger.Warn().
			Strs("backends", ids).
			Str("source", sourceType).
			Msg("some new backends still unhealthy after all retries; periodic health checker will continue")
	}
}

// healthCheckBackends runs health checks on the given backends in parallel.
func (w *ConfigWatcher) healthCheckBackends(backends []*model.Backend) {
	sem := make(chan struct{}, w.maxConcurrency)
	var wg sync.WaitGroup

	for _, b := range backends {
		wg.Add(1)
		go func(backend *model.Backend) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), w.healthTimeout)
			defer cancel()

			url := util.ModelsURL(backend.APIBase)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				w.logger.Debug().Err(err).Str("backend", backend.ID).Msg("health check request creation failed")
				return
			}
			if backend.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+backend.APIKey)
			}

			resp, err := w.healthClient.Do(req)
			if err != nil {
				w.logger.Debug().Err(err).Str("backend", backend.ID).Msg("health check failed")
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				backend.SetHealthy(true)
				backend.ResetFailures()
				backend.SetLastHealthCheck(time.Now())
			} else {
				w.logger.Debug().
					Str("backend", backend.ID).
					Int("status", resp.StatusCode).
					Msg("health check returned non-200")
			}
		}(b)
	}

	wg.Wait()
}
