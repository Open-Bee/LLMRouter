package monitor

import (
	"sync"
	"time"
)

const completionWindow = 60 * time.Second

// CompletionRPM tracks per-backend completed request rates for monitoring purposes.
// This does NOT participate in rate limiting decisions.
type CompletionRPM struct {
	mu      sync.Mutex
	windows map[string]*completionWindow_
}

type completionWindow_ struct {
	timestamps []time.Time
}

// NewCompletionRPM creates a new CompletionRPM observer.
func NewCompletionRPM() *CompletionRPM {
	return &CompletionRPM{
		windows: make(map[string]*completionWindow_),
	}
}

// Record records a completed request for the given backend.
func (c *CompletionRPM) Record(backendID string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	w, ok := c.windows[backendID]
	if !ok {
		w = &completionWindow_{}
		c.windows[backendID] = w
	}
	w.timestamps = append(w.timestamps, time.Now())
}

// CurrentRPM returns the current completion RPM for a backend.
func (c *CompletionRPM) CurrentRPM(backendID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	w, ok := c.windows[backendID]
	if !ok {
		return 0
	}
	w.evict(time.Now())
	return len(w.timestamps)
}

// AllRPMs returns the current completion RPM for all tracked backends.
func (c *CompletionRPM) AllRPMs() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	result := make(map[string]int, len(c.windows))
	for id, w := range c.windows {
		w.evict(now)
		result[id] = len(w.timestamps)
	}
	return result
}

func (w *completionWindow_) evict(now time.Time) {
	cutoff := now.Add(-completionWindow)
	i := 0
	for i < len(w.timestamps) && w.timestamps[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		w.timestamps = w.timestamps[i:]
	}
}
