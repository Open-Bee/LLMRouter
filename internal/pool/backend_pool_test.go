package pool

import (
	"fmt"
	"sync"
	"testing"

	"llm-router/internal/model"
)

func makeBackend(modelName, apiBase string) *model.Backend {
	return model.NewBackend(model.ServiceEndpoint{
		ModelName: modelName,
		LiteLLMParams: model.LiteLLMParams{
			APIBase:    apiBase,
			SourceType: "dedicated",
			Weight:     1.0,
		},
	})
}

func TestNewBackendPool_Empty(t *testing.T) {
	p := NewBackendPool()
	if len(p.AllBackends()) != 0 {
		t.Error("new pool should be empty")
	}
	if len(p.ModelNames()) != 0 {
		t.Error("new pool should have no models")
	}
	if p.HasModel("anything") {
		t.Error("empty pool should not have any model")
	}
}

func TestBackendPool_AddAndQuery(t *testing.T) {
	p := NewBackendPool()
	b1 := makeBackend("ModelA", "http://host1/v1")
	b2 := makeBackend("ModelA", "http://host2/v1")
	b3 := makeBackend("ModelB", "http://host3/v1")

	p.AddBackends([]*model.Backend{b1, b2, b3})

	if len(p.AllBackends()) != 3 {
		t.Errorf("expected 3 backends, got %d", len(p.AllBackends()))
	}

	if !p.HasModel("ModelA") {
		t.Error("should have ModelA")
	}
	if !p.HasModel("ModelB") {
		t.Error("should have ModelB")
	}
	if p.HasModel("ModelC") {
		t.Error("should not have ModelC")
	}

	modelA := p.GetBackendsForModel("ModelA")
	if len(modelA) != 2 {
		t.Errorf("expected 2 backends for ModelA, got %d", len(modelA))
	}

	modelB := p.GetBackendsForModel("ModelB")
	if len(modelB) != 1 {
		t.Errorf("expected 1 backend for ModelB, got %d", len(modelB))
	}

	names := p.ModelNames()
	if len(names) != 2 {
		t.Errorf("expected 2 model names, got %d", len(names))
	}
}

func TestBackendPool_GetHealthyBackends(t *testing.T) {
	p := NewBackendPool()
	b1 := makeBackend("M", "http://host1/v1")
	b2 := makeBackend("M", "http://host2/v1")
	b3 := makeBackend("M", "http://host3/v1")

	b2.SetHealthy(false)

	p.AddBackends([]*model.Backend{b1, b2, b3})

	healthy := p.GetHealthyBackends("M")
	if len(healthy) != 2 {
		t.Errorf("expected 2 healthy, got %d", len(healthy))
	}
	for _, b := range healthy {
		if !b.IsHealthy() {
			t.Error("returned unhealthy backend")
		}
	}

	// Non-existent model
	if got := p.GetHealthyBackends("NoModel"); got != nil {
		t.Errorf("expected nil for non-existent model, got %v", got)
	}
}

func TestBackendPool_RemoveBackends(t *testing.T) {
	p := NewBackendPool()
	b1 := makeBackend("M", "http://host1/v1")
	b2 := makeBackend("M", "http://host2/v1")
	b3 := makeBackend("N", "http://host3/v1")

	p.AddBackends([]*model.Backend{b1, b2, b3})

	// Remove b2
	p.RemoveBackends(map[string]bool{b2.ID: true})

	if len(p.AllBackends()) != 2 {
		t.Errorf("expected 2 after removal, got %d", len(p.AllBackends()))
	}
	if len(p.GetBackendsForModel("M")) != 1 {
		t.Errorf("expected 1 for model M after removal, got %d", len(p.GetBackendsForModel("M")))
	}

	// Remove all of model N
	p.RemoveBackends(map[string]bool{b3.ID: true})
	if p.HasModel("N") {
		t.Error("model N should be removed")
	}
}

func TestBackendPool_ReplaceAll(t *testing.T) {
	p := NewBackendPool()
	b1 := makeBackend("Old", "http://old/v1")
	p.AddBackends([]*model.Backend{b1})

	b2 := makeBackend("New", "http://new/v1")
	p.ReplaceAll([]*model.Backend{b2})

	if p.HasModel("Old") {
		t.Error("Old model should be gone")
	}
	if !p.HasModel("New") {
		t.Error("New model should exist")
	}
	if len(p.AllBackends()) != 1 {
		t.Errorf("expected 1, got %d", len(p.AllBackends()))
	}
}

func TestBackendPool_FindBackend(t *testing.T) {
	p := NewBackendPool()
	b := makeBackend("M", "http://host/v1")
	p.AddBackends([]*model.Backend{b})

	found := p.FindBackend(b.ID)
	if found == nil {
		t.Fatal("expected to find backend")
	}
	if found.ID != b.ID {
		t.Errorf("wrong backend found")
	}

	if p.FindBackend("nonexistent") != nil {
		t.Error("should return nil for nonexistent ID")
	}
}

func TestBackendPool_UpdateBackend(t *testing.T) {
	p := NewBackendPool()
	b := makeBackend("M", "http://host/v1")
	p.AddBackends([]*model.Backend{b})

	ok := p.UpdateBackend(b.ID, func(b *model.Backend) {
		b.Weight = 5.0
	})
	if !ok {
		t.Error("update should return true")
	}

	found := p.FindBackend(b.ID)
	if found.Weight != 5.0 {
		t.Errorf("weight not updated: %f", found.Weight)
	}

	ok = p.UpdateBackend("nonexistent", func(b *model.Backend) {})
	if ok {
		t.Error("should return false for nonexistent")
	}
}

func TestBackendPool_GetBackendsBySourceType(t *testing.T) {
	p := NewBackendPool()
	b1 := model.NewBackend(model.ServiceEndpoint{
		ModelName:     "M1",
		LiteLLMParams: model.LiteLLMParams{APIBase: "http://h1/v1", SourceType: "openai"},
	})
	b2 := model.NewBackend(model.ServiceEndpoint{
		ModelName:     "M2",
		LiteLLMParams: model.LiteLLMParams{APIBase: "http://h2/v1", SourceType: "self_deployed"},
	})
	b3 := model.NewBackend(model.ServiceEndpoint{
		ModelName:     "M3",
		LiteLLMParams: model.LiteLLMParams{APIBase: "http://h3/v1", SourceType: "openai"},
	})

	p.AddBackends([]*model.Backend{b1, b2, b3})

	openai := p.GetBackendsBySourceType("openai")
	if len(openai) != 2 {
		t.Errorf("expected 2 openai, got %d", len(openai))
	}

	selfDeployed := p.GetBackendsBySourceType("self_deployed")
	if len(selfDeployed) != 1 {
		t.Errorf("expected 1 self_deployed, got %d", len(selfDeployed))
	}

	none := p.GetBackendsBySourceType("other")
	if len(none) != 0 {
		t.Errorf("expected 0 other, got %d", len(none))
	}
}

func TestBackendPool_ConcurrentReadWrite(t *testing.T) {
	p := NewBackendPool()

	// Pre-populate
	for i := 0; i < 10; i++ {
		b := makeBackend("M", fmt.Sprintf("http://host%d/v1", i))
		p.AddBackends([]*model.Backend{b})
	}

	var wg sync.WaitGroup

	// Concurrent reads
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.AllBackends()
			_ = p.GetBackendsForModel("M")
			_ = p.GetHealthyBackends("M")
			_ = p.HasModel("M")
			_ = p.ModelNames()
		}()
	}

	// Concurrent writes
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			b := makeBackend("M", fmt.Sprintf("http://new%d/v1", idx))
			p.AddBackends([]*model.Backend{b})
		}(i)
	}

	wg.Wait()
	// No race or panic = pass
}
