package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"llm-router/internal/model"
	"llm-router/internal/pool"
	"llm-router/internal/util"

	"github.com/rs/zerolog"
)

// MetricsMonitor collects Prometheus metrics from backends periodically.
type MetricsMonitor struct {
	pool     *pool.BackendPool
	client   *http.Client
	interval time.Duration
	logger   zerolog.Logger
}

// NewMetricsMonitor creates a new MetricsMonitor.
func NewMetricsMonitor(
	p *pool.BackendPool,
	interval time.Duration,
	logger zerolog.Logger,
) *MetricsMonitor {
	return &MetricsMonitor{
		pool: p,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		interval: interval,
		logger:   logger.With().Str("component", "metrics_monitor").Logger(),
	}
}

// Run starts the periodic metrics collection loop. Blocks until ctx is cancelled.
func (m *MetricsMonitor) Run(ctx context.Context) {
	m.logger.Info().Dur("interval", m.interval).Msg("starting metrics monitor")

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info().Msg("metrics monitor stopped")
			return
		case <-ticker.C:
			m.collectAll(ctx)
		}
	}
}

// GetMetrics returns the latest metrics for a backend by ID.
func (m *MetricsMonitor) GetMetrics(backendID string) *model.LoadMetrics {
	b := m.pool.FindBackend(backendID)
	if b == nil {
		return nil
	}
	return b.GetLoadMetrics()
}

func (m *MetricsMonitor) collectAll(ctx context.Context) {
	backends := m.pool.AllBackends()
	if len(backends) == 0 {
		return
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 20)

	var collected, skippedUnhealthy, skippedNoMetrics int

	for _, b := range backends {
		if !b.IsHealthy() {
			skippedUnhealthy++
			continue
		}
		// Skip non-engine backends (e.g. openai proxies) that don't expose /metrics
		if b.SourceType == "openai" {
			skippedNoMetrics++
			continue
		}
		// Skip backends whose /metrics endpoint is known to be unavailable,
		// unless enough time has passed to retry.
		if b.IsMetricsUnavailable() && !b.ShouldRetryMetrics() {
			skippedNoMetrics++
			continue
		}
		collected++
		wg.Add(1)
		go func(backend *model.Backend) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			m.collectOne(ctx, backend)
		}(b)
	}

	wg.Wait()

	m.logger.Debug().
		Int("total", len(backends)).
		Int("collected", collected).
		Int("skipped_unhealthy", skippedUnhealthy).
		Int("skipped_no_metrics", skippedNoMetrics).
		Msg("metrics collection round complete")
}

func (m *MetricsMonitor) collectOne(ctx context.Context, b *model.Backend) {
	metricsURL := util.MetricsURL(b.APIBase)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metricsURL, nil)
	if err != nil {
		b.SetLoadMetrics(&model.LoadMetrics{Error: err.Error(), CollectedAt: time.Now()})
		return
	}

	resp, err := m.client.Do(req)
	if err != nil {
		b.SetLoadMetrics(&model.LoadMetrics{Error: err.Error(), CollectedAt: time.Now()})
		// On timeout/connection errors, mark unavailable for 1 minute to avoid
		// flooding logs and wasting resources on unreachable backends.
		b.MarkMetricsUnavailable(1 * time.Minute)
		m.logger.Debug().
			Str("backend", b.ID).
			Str("url", metricsURL).
			Err(err).
			Msg("failed to fetch /metrics, backing off for 1m")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// /metrics endpoint not available on this backend; stop polling it
		// for 10 minutes to avoid flooding logs with 404s.
		b.MarkMetricsUnavailable(10 * time.Minute)
		b.SetLoadMetrics(&model.LoadMetrics{
			Error:       "/metrics endpoint not found (404)",
			CollectedAt: time.Now(),
		})
		m.logger.Debug().
			Str("backend", b.ID).
			Str("url", metricsURL).
			Str("source_type", b.SourceType).
			Str("api_base", b.APIBase).
			Msg("/metrics returned 404, marking unavailable for 10m")
		return
	}

	if resp.StatusCode != http.StatusOK {
		b.SetLoadMetrics(&model.LoadMetrics{
			Error:       fmt.Sprintf("/metrics returned HTTP %d", resp.StatusCode),
			CollectedAt: time.Now(),
		})
		m.logger.Debug().
			Str("backend", b.ID).
			Str("url", metricsURL).
			Int("status", resp.StatusCode).
			Msg("/metrics returned unexpected status")
		return
	}

	// Success – clear any previous unavailable mark
	if b.IsMetricsUnavailable() {
		b.ResetMetricsUnavailable()
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.SetLoadMetrics(&model.LoadMetrics{Error: err.Error(), CollectedAt: time.Now()})
		return
	}

	metrics, engineType := util.ParsePrometheusMetrics(string(body))
	metrics.CollectedAt = time.Now()
	b.SetLoadMetrics(metrics)

	if engineType != model.EngineUnknown {
		b.SetEngineType(engineType)
	}
}
