package pool

import (
	"sync"
	"sync/atomic"

	"llm-router/internal/model"
)

// snapshot holds an immutable point-in-time view of all backends.
type snapshot struct {
	// byModel maps model_name -> slice of backends for that model
	byModel map[string][]*model.Backend
	// all is the flat list of all backends
	all []*model.Backend
}

// BackendPool manages backends using Copy-on-Write for lock-free reads.
type BackendPool struct {
	snap atomic.Value // *snapshot
	mu   sync.Mutex   // protects writes only
}

// NewBackendPool creates an empty BackendPool.
func NewBackendPool() *BackendPool {
	p := &BackendPool{}
	p.snap.Store(&snapshot{
		byModel: make(map[string][]*model.Backend),
		all:     nil,
	})
	return p
}

// getSnapshot returns the current immutable snapshot (lock-free read).
func (p *BackendPool) getSnapshot() *snapshot {
	return p.snap.Load().(*snapshot)
}

// GetBackendsForModel returns all backends registered for the given model name.
// Scans the full backend list for consistency after hot-reload.
func (p *BackendPool) GetBackendsForModel(modelName string) []*model.Backend {
	s := p.getSnapshot()
	var result []*model.Backend
	for _, b := range s.all {
		if b.ModelName == modelName {
			result = append(result, b)
		}
	}
	return result
}

// GetHealthyBackends returns healthy backends for the given model name.
// It scans the full backend list instead of relying on the byModel index
// to guarantee consistency after hot-reload (ReplaceBySourceType).
func (p *BackendPool) GetHealthyBackends(modelName string) []*model.Backend {
	s := p.getSnapshot()
	healthy := make([]*model.Backend, 0, 8)
	for _, b := range s.all {
		if b.ModelName == modelName && b.IsHealthy() {
			healthy = append(healthy, b)
		}
	}
	if len(healthy) == 0 {
		return nil
	}
	return healthy
}

// AllBackends returns all backends across all models.
func (p *BackendPool) AllBackends() []*model.Backend {
	return p.getSnapshot().all
}

// ModelNames returns all registered model names.
func (p *BackendPool) ModelNames() []string {
	s := p.getSnapshot()
	seen := make(map[string]bool)
	for _, b := range s.all {
		seen[b.ModelName] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	return names
}

// HasModel returns true if the given model name is registered.
func (p *BackendPool) HasModel(modelName string) bool {
	s := p.getSnapshot()
	for _, b := range s.all {
		if b.ModelName == modelName {
			return true
		}
	}
	return false
}

// AddBackends adds a batch of backends to the pool (write path, takes lock).
func (p *BackendPool) AddBackends(backends []*model.Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()

	old := p.getSnapshot()
	newByModel := copyByModel(old.byModel)
	newAll := make([]*model.Backend, len(old.all))
	copy(newAll, old.all)

	for _, b := range backends {
		newByModel[b.ModelName] = append(newByModel[b.ModelName], b)
		newAll = append(newAll, b)
	}

	p.snap.Store(&snapshot{byModel: newByModel, all: newAll})
}

// RemoveBackends removes backends matching the given IDs (write path, takes lock).
func (p *BackendPool) RemoveBackends(ids map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	old := p.getSnapshot()
	newByModel := make(map[string][]*model.Backend)
	newAll := make([]*model.Backend, 0, len(old.all))

	for modelName, backends := range old.byModel {
		filtered := make([]*model.Backend, 0, len(backends))
		for _, b := range backends {
			if !ids[b.ID] {
				filtered = append(filtered, b)
				newAll = append(newAll, b)
			}
		}
		if len(filtered) > 0 {
			newByModel[modelName] = filtered
		}
	}

	p.snap.Store(&snapshot{byModel: newByModel, all: newAll})
}

// ReplaceAll replaces all backends in the pool atomically.
func (p *BackendPool) ReplaceAll(backends []*model.Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()

	newByModel := make(map[string][]*model.Backend)
	for _, b := range backends {
		newByModel[b.ModelName] = append(newByModel[b.ModelName], b)
	}

	newAll := make([]*model.Backend, len(backends))
	copy(newAll, backends)

	p.snap.Store(&snapshot{byModel: newByModel, all: newAll})
}

// ReplaceBySourceType atomically replaces all backends of the given source type
// with the provided list. Other source types are untouched. This ensures there
// is never a moment where the pool has zero backends for a model during the swap.
func (p *BackendPool) ReplaceBySourceType(sourceType string, backends []*model.Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()

	old := p.getSnapshot()

	// Collect all backends NOT of this source type (keep them)
	newAll := make([]*model.Backend, 0, len(old.all))
	for _, b := range old.all {
		if b.SourceType != sourceType {
			newAll = append(newAll, b)
		}
	}

	// Append the new backends for this source type
	newAll = append(newAll, backends...)

	// Rebuild byModel index
	newByModel := make(map[string][]*model.Backend)
	for _, b := range newAll {
		newByModel[b.ModelName] = append(newByModel[b.ModelName], b)
	}

	p.snap.Store(&snapshot{byModel: newByModel, all: newAll})
}

// UpdateBackend finds a backend by ID and applies the updater function.
// Returns true if the backend was found and updated.
func (p *BackendPool) UpdateBackend(id string, updater func(b *model.Backend)) bool {
	s := p.getSnapshot()
	for _, b := range s.all {
		if b.ID == id {
			updater(b)
			return true
		}
	}
	return false
}

// FindBackend finds a backend by ID.
func (p *BackendPool) FindBackend(id string) *model.Backend {
	s := p.getSnapshot()
	for _, b := range s.all {
		if b.ID == id {
			return b
		}
	}
	return nil
}

// GetBackendsBySourceType returns all backends of the given source type.
func (p *BackendPool) GetBackendsBySourceType(sourceType string) []*model.Backend {
	s := p.getSnapshot()
	var result []*model.Backend
	for _, b := range s.all {
		if b.SourceType == sourceType {
			result = append(result, b)
		}
	}
	return result
}

func copyByModel(src map[string][]*model.Backend) map[string][]*model.Backend {
	dst := make(map[string][]*model.Backend, len(src))
	for k, v := range src {
		cp := make([]*model.Backend, len(v))
		copy(cp, v)
		dst[k] = cp
	}
	return dst
}
