package ratelimit

import (
	"sync"
	"time"
)

const windowDuration = 60 * time.Second

// RateLimiter implements per-backend sliding window RPM rate limiting.
// It tracks sent requests to enforce strict RPM limits.
type RateLimiter struct {
	mu      sync.Mutex
	windows map[string]*slidingWindow // backend ID -> window
}

type slidingWindow struct {
	timestamps []time.Time
}

// NewRateLimiter creates a new RateLimiter.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		windows: make(map[string]*slidingWindow),
	}
}

// Allow checks if a request can be sent to the given backend.
// rpmLimit is the RPM limit; if nil, always returns true.
// If allowed, it atomically records the request timestamp.
func (r *RateLimiter) Allow(backendID string, rpmLimit *int) bool {
	if rpmLimit == nil {
		return true
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	w := r.getOrCreate(backendID)
	now := time.Now()
	w.evict(now)

	if len(w.timestamps) >= *rpmLimit {
		return false
	}

	w.timestamps = append(w.timestamps, now)
	return true
}

// CurrentRPM returns the current number of requests in the sliding window for a backend.
func (r *RateLimiter) CurrentRPM(backendID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.windows[backendID]
	if !ok {
		return 0
	}
	w.evict(time.Now())
	return len(w.timestamps)
}

// Record explicitly records a request timestamp for the given backend.
// Used when Allow is not called but a request was sent.
func (r *RateLimiter) Record(backendID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w := r.getOrCreate(backendID)
	w.timestamps = append(w.timestamps, time.Now())
}

// IsLimited returns true if the backend has reached its RPM limit.
func (r *RateLimiter) IsLimited(backendID string, rpmLimit *int) bool {
	if rpmLimit == nil {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.windows[backendID]
	if !ok {
		return false
	}
	w.evict(time.Now())
	return len(w.timestamps) >= *rpmLimit
}

// RemoveBackend removes the rate limit state for a backend.
func (r *RateLimiter) RemoveBackend(backendID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.windows, backendID)
}

func (r *RateLimiter) getOrCreate(backendID string) *slidingWindow {
	w, ok := r.windows[backendID]
	if !ok {
		w = &slidingWindow{}
		r.windows[backendID] = w
	}
	return w
}

// evict removes timestamps outside the sliding window.
func (w *slidingWindow) evict(now time.Time) {
	cutoff := now.Add(-windowDuration)
	i := 0
	for i < len(w.timestamps) && w.timestamps[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		w.timestamps = w.timestamps[i:]
	}
}
