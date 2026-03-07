package util

import (
	"sync"
	"sync/atomic"
	"time"
)

// RequestStats tracks request statistics globally and per-backend.
type RequestStats struct {
	mu           sync.RWMutex
	total        atomic.Int64
	success      atomic.Int64
	failures     atomic.Int64
	backendStats map[string]*BackendStats
	startTime    time.Time
}

// BackendStats holds per-backend statistics.
type BackendStats struct {
	Total        atomic.Int64
	Success      atomic.Int64
	Failures     atomic.Int64
	TotalLatency atomic.Int64 // in milliseconds
	ErrorCounts  sync.Map     // error type -> *atomic.Int64
}

// NewRequestStats creates a new RequestStats tracker.
func NewRequestStats() *RequestStats {
	return &RequestStats{
		backendStats: make(map[string]*BackendStats),
		startTime:    time.Now(),
	}
}

// RecordSuccess records a successful request for the given backend.
func (s *RequestStats) RecordSuccess(backendID string, latency time.Duration) {
	s.total.Add(1)
	s.success.Add(1)

	bs := s.getOrCreate(backendID)
	bs.Total.Add(1)
	bs.Success.Add(1)
	bs.TotalLatency.Add(latency.Milliseconds())
}

// RecordFailure records a failed request for the given backend.
func (s *RequestStats) RecordFailure(backendID string, errorType string) {
	s.total.Add(1)
	s.failures.Add(1)

	bs := s.getOrCreate(backendID)
	bs.Total.Add(1)
	bs.Failures.Add(1)

	v, _ := bs.ErrorCounts.LoadOrStore(errorType, &atomic.Int64{})
	v.(*atomic.Int64).Add(1)
}

// Total returns total request count.
func (s *RequestStats) Total() int64 {
	return s.total.Load()
}

// Success returns successful request count.
func (s *RequestStats) Success() int64 {
	return s.success.Load()
}

// Failures returns failed request count.
func (s *RequestStats) Failures() int64 {
	return s.failures.Load()
}

// Uptime returns how long stats have been tracking.
func (s *RequestStats) Uptime() time.Duration {
	return time.Since(s.startTime)
}

// GetBackendStats returns stats for a specific backend.
func (s *RequestStats) GetBackendStats(backendID string) *BackendStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.backendStats[backendID]
}

// AllBackendStats returns a copy of the backend stats map.
func (s *RequestStats) AllBackendStats() map[string]*BackendStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]*BackendStats, len(s.backendStats))
	for k, v := range s.backendStats {
		result[k] = v
	}
	return result
}

// AvgLatency returns the average latency in ms for a backend, or 0 if no data.
func (bs *BackendStats) AvgLatency() float64 {
	total := bs.Total.Load()
	if total == 0 {
		return 0
	}
	return float64(bs.TotalLatency.Load()) / float64(total)
}

func (s *RequestStats) getOrCreate(backendID string) *BackendStats {
	s.mu.RLock()
	bs, ok := s.backendStats[backendID]
	s.mu.RUnlock()
	if ok {
		return bs
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	bs, ok = s.backendStats[backendID]
	if ok {
		return bs
	}
	bs = &BackendStats{}
	s.backendStats[backendID] = bs
	return bs
}
