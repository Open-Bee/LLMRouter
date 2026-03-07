package monitor

import (
	"context"

	"llm-router/internal/model"
	"llm-router/internal/pool"

	"github.com/rs/zerolog"
)

// Manager coordinates all monitoring subsystems.
type Manager struct {
	MetricsMonitor *MetricsMonitor
	CompletionRPM  *CompletionRPM
	pool           *pool.BackendPool
	logger         zerolog.Logger
}

// NewManager creates a new monitoring Manager.
func NewManager(p *pool.BackendPool, metricsMonitor *MetricsMonitor, logger zerolog.Logger) *Manager {
	return &Manager{
		MetricsMonitor: metricsMonitor,
		CompletionRPM:  NewCompletionRPM(),
		pool:           p,
		logger:         logger.With().Str("component", "monitor_manager").Logger(),
	}
}

// GetMetrics implements routing.LoadMetricsProvider.
func (m *Manager) GetMetrics(backendID string) *model.LoadMetrics {
	return m.MetricsMonitor.GetMetrics(backendID)
}

// Run starts all monitoring subsystems. Blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	m.MetricsMonitor.Run(ctx)
}
