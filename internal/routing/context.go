package routing

import (
	"llm-router/internal/model"
)

// Context holds the state for a single routing request, tracking attempted backends.
type Context struct {
	Model       string
	Attempted   map[string]int // backend ID -> attempt count
	MaxRetries  int
	AttemptNum  int
}

// NewContext creates a new routing context for the given model.
func NewContext(modelName string, maxRetries int) *Context {
	return &Context{
		Model:      modelName,
		Attempted:  make(map[string]int),
		MaxRetries: maxRetries,
		AttemptNum: 0,
	}
}

// MarkAttempted records that a backend has been tried.
func (c *Context) MarkAttempted(b *model.Backend) {
	c.Attempted[b.ID]++
	c.AttemptNum++
}

// HasAttempted returns true if the backend has been tried before.
func (c *Context) HasAttempted(b *model.Backend) bool {
	return c.Attempted[b.ID] > 0
}

// CanRetry returns true if more retries are allowed.
func (c *Context) CanRetry() bool {
	return c.AttemptNum <= c.MaxRetries
}

// FilterUnattempted returns backends that have not been tried yet.
// If all have been tried, returns the full list to allow re-use.
func (c *Context) FilterUnattempted(backends []*model.Backend) []*model.Backend {
	unattempted := make([]*model.Backend, 0, len(backends))
	for _, b := range backends {
		if !c.HasAttempted(b) {
			unattempted = append(unattempted, b)
		}
	}
	if len(unattempted) > 0 {
		return unattempted
	}
	return backends
}
