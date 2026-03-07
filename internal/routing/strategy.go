package routing

import (
	"math/rand"

	"llm-router/internal/model"
)

// Strategy is the interface for backend selection strategies.
type Strategy interface {
	// Select picks one backend from the candidate list.
	Select(candidates []*model.Backend) *model.Backend
	// Name returns the strategy name.
	Name() string
}

// LoadMetricsProvider provides load metrics for backends. Used by LeastWaitingStrategy.
type LoadMetricsProvider interface {
	GetMetrics(backendID string) *model.LoadMetrics
}

// --- SimpleShuffleStrategy ---

// SimpleShuffleStrategy selects a backend using weighted random selection.
type SimpleShuffleStrategy struct{}

// NewSimpleShuffle creates a new SimpleShuffleStrategy.
func NewSimpleShuffle() *SimpleShuffleStrategy {
	return &SimpleShuffleStrategy{}
}

// Select picks a backend based on weighted random selection.
func (s *SimpleShuffleStrategy) Select(candidates []*model.Backend) *model.Backend {
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	var totalWeight float64
	for _, b := range candidates {
		totalWeight += b.Weight
	}

	// Fallback: if all weights are zero, pick uniformly at random
	if totalWeight <= 0 {
		return candidates[rand.Intn(len(candidates))]
	}

	r := rand.Float64() * totalWeight
	var cumulative float64
	for _, b := range candidates {
		cumulative += b.Weight
		if r <= cumulative {
			return b
		}
	}
	return candidates[len(candidates)-1]
}

// Name returns the strategy name.
func (s *SimpleShuffleStrategy) Name() string {
	return "simple-shuffle"
}

// --- LeastConnectionsStrategy (Power-of-Two-Choices) ---

// LeastConnectionsStrategy selects the backend with fewer active connections
// using the Power-of-Two-Choices algorithm.
type LeastConnectionsStrategy struct{}

// NewLeastConnections creates a new LeastConnectionsStrategy.
func NewLeastConnections() *LeastConnectionsStrategy {
	return &LeastConnectionsStrategy{}
}

// Select picks a backend using Power-of-Two-Choices on active connections.
func (s *LeastConnectionsStrategy) Select(candidates []*model.Backend) *model.Backend {
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	i := rand.Intn(len(candidates))
	j := rand.Intn(len(candidates) - 1)
	if j >= i {
		j++
	}

	a, b := candidates[i], candidates[j]
	if a.ActiveConnections() <= b.ActiveConnections() {
		return a
	}
	return b
}

// Name returns the strategy name.
func (s *LeastConnectionsStrategy) Name() string {
	return "least-connections"
}

// --- LeastWaitingStrategy (Prometheus metrics + P2C) ---

// LeastWaitingStrategy selects the backend with the least combined load
// (waiting_from_metrics + active_connections) using Power-of-Two-Choices.
type LeastWaitingStrategy struct {
	metricsProvider LoadMetricsProvider
}

// NewLeastWaiting creates a new LeastWaitingStrategy with the given metrics provider.
func NewLeastWaiting(provider LoadMetricsProvider) *LeastWaitingStrategy {
	return &LeastWaitingStrategy{metricsProvider: provider}
}

// Select picks a backend with the least combined load using P2C.
func (s *LeastWaitingStrategy) Select(candidates []*model.Backend) *model.Backend {
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 {
		return candidates[0]
	}

	// Separate backends with and without metrics
	withMetrics := make([]*model.Backend, 0, len(candidates))
	withoutMetrics := make([]*model.Backend, 0)

	for _, b := range candidates {
		m := b.GetLoadMetrics()
		if m != nil && m.Error == "" {
			withMetrics = append(withMetrics, b)
		} else {
			withoutMetrics = append(withoutMetrics, b)
		}
	}

	// Prefer backends with real metrics
	pool := withMetrics
	if len(pool) < 2 {
		pool = candidates
	}

	i := rand.Intn(len(pool))
	j := rand.Intn(len(pool) - 1)
	if j >= i {
		j++
	}

	a, b := pool[i], pool[j]
	loadA := s.combinedLoad(a)
	loadB := s.combinedLoad(b)

	if loadA <= loadB {
		return a
	}
	return b
}

func (s *LeastWaitingStrategy) combinedLoad(b *model.Backend) int64 {
	load := b.ActiveConnections()
	if m := b.GetLoadMetrics(); m != nil && m.Error == "" {
		load += int64(m.WaitingRequests)
	}
	return load
}

// Name returns the strategy name.
func (s *LeastWaitingStrategy) Name() string {
	return "least-waiting"
}

// NewStrategy is a factory function that creates a Strategy by name.
func NewStrategy(name string, metricsProvider LoadMetricsProvider) Strategy {
	switch name {
	case "least-connections":
		return NewLeastConnections()
	case "least-waiting":
		return NewLeastWaiting(metricsProvider)
	default:
		return NewSimpleShuffle()
	}
}
