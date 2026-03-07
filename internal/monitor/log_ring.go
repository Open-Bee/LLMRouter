package monitor

import (
	"strings"
	"sync"
)

// LogRing is a thread-safe ring buffer for log lines.
// It implements io.Writer so it can be used as a zerolog output.
type LogRing struct {
	mu    sync.Mutex
	lines []string
	max   int
}

// NewLogRing creates a LogRing with the given max capacity.
func NewLogRing(max int) *LogRing {
	return &LogRing{
		lines: make([]string, 0, max),
		max:   max,
	}
}

// Write implements io.Writer. Each call appends one log line.
func (r *LogRing) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if line == "" {
		return len(p), nil
	}
	r.mu.Lock()
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
	r.mu.Unlock()
	return len(p), nil
}

// Lines returns a snapshot of all buffered log lines.
func (r *LogRing) Lines() []string {
	r.mu.Lock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	r.mu.Unlock()
	return out
}
