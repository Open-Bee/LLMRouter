package model

import (
	"fmt"
	"sync/atomic"
	"time"
)

// Backend represents a single LLM backend with both configuration and runtime state.
type Backend struct {
	// Configuration fields (immutable after creation, except weight/rpm_limit via hot-reload)
	ID             string
	ModelName      string
	APIBase        string
	APIKey         string
	SupportsVision bool
	Weight         float64
	SourceType     string
	RPMLimit       *int // nil means unlimited

	// Runtime state (atomic operations)
	healthy            atomic.Bool
	activeConnections  atomic.Int64
	pendingRequests    atomic.Int64 // requests queued in HTTP transport, not yet sent
	lastHealthCheck    atomic.Value // time.Time
	consecutiveFailures atomic.Int64
	engineType         atomic.Value // EngineType

	// Load metrics (atomic)
	loadMetrics atomic.Value // *LoadMetrics

	// Metrics endpoint availability
	metricsUnavailable atomic.Bool  // true if /metrics returned 404
	metricsRetryAfter  atomic.Value // time.Time – next time to retry /metrics
}

// NewBackend creates a Backend from a ServiceEndpoint configuration.
func NewBackend(ep ServiceEndpoint) *Backend {
	b := &Backend{
		ID:             fmt.Sprintf("%s@%s", ep.ModelName, ep.LiteLLMParams.APIBase),
		ModelName:      ep.ModelName,
		APIBase:        ep.LiteLLMParams.APIBase,
		APIKey:         ep.LiteLLMParams.APIKey,
		SupportsVision: ep.LiteLLMParams.SupportsVision,
		Weight:         ep.LiteLLMParams.Weight,
		SourceType:     ep.LiteLLMParams.SourceType,
		RPMLimit:       ep.LiteLLMParams.RPMLimit,
	}
	if b.Weight == 0 {
		b.Weight = 1.0
	}
	b.healthy.Store(true)
	b.lastHealthCheck.Store(time.Time{})
	b.engineType.Store(EngineUnknown)
	b.loadMetrics.Store((*LoadMetrics)(nil))
	b.metricsRetryAfter.Store(time.Time{})
	return b
}

// IsHealthy returns whether this backend is considered healthy.
func (b *Backend) IsHealthy() bool {
	return b.healthy.Load()
}

// SetHealthy sets the backend health status.
func (b *Backend) SetHealthy(healthy bool) {
	b.healthy.Store(healthy)
}

// ActiveConnections returns the current number of active connections.
func (b *Backend) ActiveConnections() int64 {
	return b.activeConnections.Load()
}

// IncrConnections atomically increments the active connection count.
func (b *Backend) IncrConnections() {
	b.activeConnections.Add(1)
}

// DecrConnections atomically decrements the active connection count.
func (b *Backend) DecrConnections() {
	b.activeConnections.Add(-1)
}

// PendingRequests returns the number of requests waiting in HTTP transport queue.
func (b *Backend) PendingRequests() int64 {
	return b.pendingRequests.Load()
}

// IncrPending atomically increments the pending request count.
func (b *Backend) IncrPending() {
	b.pendingRequests.Add(1)
}

// DecrPending atomically decrements the pending request count.
func (b *Backend) DecrPending() {
	b.pendingRequests.Add(-1)
}

// ConsecutiveFailures returns the number of consecutive failures.
func (b *Backend) ConsecutiveFailures() int64 {
	return b.consecutiveFailures.Load()
}

// IncrFailures atomically increments and returns the new consecutive failure count.
func (b *Backend) IncrFailures() int64 {
	return b.consecutiveFailures.Add(1)
}

// ResetFailures resets the consecutive failure counter to zero.
func (b *Backend) ResetFailures() {
	b.consecutiveFailures.Store(0)
}

// LastHealthCheck returns the time of the last health check.
func (b *Backend) LastHealthCheck() time.Time {
	v := b.lastHealthCheck.Load()
	if v == nil {
		return time.Time{}
	}
	return v.(time.Time)
}

// SetLastHealthCheck sets the time of the last health check.
func (b *Backend) SetLastHealthCheck(t time.Time) {
	b.lastHealthCheck.Store(t)
}

// GetEngineType returns the detected engine type of this backend.
func (b *Backend) GetEngineType() EngineType {
	v := b.engineType.Load()
	if v == nil {
		return EngineUnknown
	}
	return v.(EngineType)
}

// SetEngineType sets the engine type for this backend.
func (b *Backend) SetEngineType(et EngineType) {
	b.engineType.Store(et)
}

// GetLoadMetrics returns the latest load metrics for this backend.
func (b *Backend) GetLoadMetrics() *LoadMetrics {
	v := b.loadMetrics.Load()
	if v == nil {
		return nil
	}
	return v.(*LoadMetrics)
}

// SetLoadMetrics updates the load metrics for this backend.
func (b *Backend) SetLoadMetrics(m *LoadMetrics) {
	b.loadMetrics.Store(m)
}

// IsMetricsUnavailable returns true if /metrics endpoint is known to be unsupported.
func (b *Backend) IsMetricsUnavailable() bool {
	return b.metricsUnavailable.Load()
}

// MarkMetricsUnavailable marks /metrics as unavailable and sets a retry time.
func (b *Backend) MarkMetricsUnavailable(retryAfter time.Duration) {
	b.metricsUnavailable.Store(true)
	b.metricsRetryAfter.Store(time.Now().Add(retryAfter))
}

// ShouldRetryMetrics returns true if enough time has passed to retry /metrics.
func (b *Backend) ShouldRetryMetrics() bool {
	v := b.metricsRetryAfter.Load()
	if v == nil {
		return true
	}
	return time.Now().After(v.(time.Time))
}

// ResetMetricsUnavailable clears the metrics unavailable flag.
func (b *Backend) ResetMetricsUnavailable() {
	b.metricsUnavailable.Store(false)
}
