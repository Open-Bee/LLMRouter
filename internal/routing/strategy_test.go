package routing

import (
	"testing"

	"llm-router/internal/model"
)

func makeTestBackend(name, apiBase string, weight float64) *model.Backend {
	return model.NewBackend(model.ServiceEndpoint{
		ModelName: name,
		LiteLLMParams: model.LiteLLMParams{
			APIBase: apiBase,
			Weight:  weight,
		},
	})
}

// --- SimpleShuffleStrategy ---

func TestSimpleShuffle_Empty(t *testing.T) {
	s := NewSimpleShuffle()
	if s.Select(nil) != nil {
		t.Error("nil candidates should return nil")
	}
	if s.Select([]*model.Backend{}) != nil {
		t.Error("empty candidates should return nil")
	}
}

func TestSimpleShuffle_Single(t *testing.T) {
	s := NewSimpleShuffle()
	b := makeTestBackend("M", "http://h/v1", 1.0)
	if s.Select([]*model.Backend{b}) != b {
		t.Error("single candidate should be returned")
	}
}

func TestSimpleShuffle_AlwaysReturnsCandidate(t *testing.T) {
	s := NewSimpleShuffle()
	b1 := makeTestBackend("M", "http://h1/v1", 1.0)
	b2 := makeTestBackend("M", "http://h2/v1", 1.0)
	candidates := []*model.Backend{b1, b2}

	for i := 0; i < 100; i++ {
		result := s.Select(candidates)
		if result == nil {
			t.Fatal("should never return nil")
		}
		if result != b1 && result != b2 {
			t.Fatal("returned unknown backend")
		}
	}
}

func TestSimpleShuffle_WeightDistribution(t *testing.T) {
	s := NewSimpleShuffle()
	heavy := makeTestBackend("M", "http://heavy/v1", 9.0)
	light := makeTestBackend("M", "http://light/v1", 1.0)
	candidates := []*model.Backend{heavy, light}

	counts := map[string]int{}
	n := 10000
	for i := 0; i < n; i++ {
		b := s.Select(candidates)
		counts[b.ID]++
	}

	heavyRatio := float64(counts[heavy.ID]) / float64(n)
	// With 9:1 weight, heavy should get ~90%. Allow wide margin.
	if heavyRatio < 0.80 || heavyRatio > 0.98 {
		t.Errorf("heavy ratio %.2f outside expected range [0.80, 0.98]", heavyRatio)
	}
}

func TestSimpleShuffle_Name(t *testing.T) {
	s := NewSimpleShuffle()
	if s.Name() != "simple-shuffle" {
		t.Errorf("unexpected name: %s", s.Name())
	}
}

// --- LeastConnectionsStrategy ---

func TestLeastConnections_Empty(t *testing.T) {
	s := NewLeastConnections()
	if s.Select(nil) != nil {
		t.Error("nil should return nil")
	}
}

func TestLeastConnections_Single(t *testing.T) {
	s := NewLeastConnections()
	b := makeTestBackend("M", "http://h/v1", 1.0)
	if s.Select([]*model.Backend{b}) != b {
		t.Error("single candidate should be returned")
	}
}

func TestLeastConnections_PrefersLowerConnections(t *testing.T) {
	s := NewLeastConnections()
	busy := makeTestBackend("M", "http://busy/v1", 1.0)
	idle := makeTestBackend("M", "http://idle/v1", 1.0)

	// Make busy have 100 connections
	for i := 0; i < 100; i++ {
		busy.IncrConnections()
	}

	candidates := []*model.Backend{busy, idle}
	idleCount := 0
	n := 1000
	for i := 0; i < n; i++ {
		if s.Select(candidates) == idle {
			idleCount++
		}
	}

	// idle should be selected most of the time via P2C
	if idleCount < n/2 {
		t.Errorf("idle selected only %d/%d times, expected majority", idleCount, n)
	}
}

func TestLeastConnections_Name(t *testing.T) {
	if NewLeastConnections().Name() != "least-connections" {
		t.Error("wrong name")
	}
}

// --- LeastWaitingStrategy ---

type mockMetricsProvider struct {
	metrics map[string]*model.LoadMetrics
}

func (m *mockMetricsProvider) GetMetrics(id string) *model.LoadMetrics {
	return m.metrics[id]
}

func TestLeastWaiting_Empty(t *testing.T) {
	s := NewLeastWaiting(&mockMetricsProvider{})
	if s.Select(nil) != nil {
		t.Error("nil should return nil")
	}
}

func TestLeastWaiting_Single(t *testing.T) {
	s := NewLeastWaiting(&mockMetricsProvider{})
	b := makeTestBackend("M", "http://h/v1", 1.0)
	if s.Select([]*model.Backend{b}) != b {
		t.Error("single should return that backend")
	}
}

func TestLeastWaiting_PrefersLowerLoad(t *testing.T) {
	loaded := makeTestBackend("M", "http://loaded/v1", 1.0)
	light := makeTestBackend("M", "http://light/v1", 1.0)

	// Set metrics on both
	loaded.SetLoadMetrics(&model.LoadMetrics{WaitingRequests: 50})
	light.SetLoadMetrics(&model.LoadMetrics{WaitingRequests: 1})

	s := NewLeastWaiting(&mockMetricsProvider{})
	candidates := []*model.Backend{loaded, light}

	lightCount := 0
	n := 1000
	for i := 0; i < n; i++ {
		if s.Select(candidates) == light {
			lightCount++
		}
	}

	if lightCount < n/2 {
		t.Errorf("light selected only %d/%d times", lightCount, n)
	}
}

func TestLeastWaiting_Name(t *testing.T) {
	if NewLeastWaiting(nil).Name() != "least-waiting" {
		t.Error("wrong name")
	}
}

// --- Factory ---

func TestNewStrategy_Factory(t *testing.T) {
	tests := []struct {
		name     string
		expected string
	}{
		{"simple-shuffle", "simple-shuffle"},
		{"least-connections", "least-connections"},
		{"least-waiting", "least-waiting"},
		{"unknown", "simple-shuffle"}, // default
		{"", "simple-shuffle"},
	}

	for _, tt := range tests {
		s := NewStrategy(tt.name, &mockMetricsProvider{})
		if s.Name() != tt.expected {
			t.Errorf("NewStrategy(%q): got %s, want %s", tt.name, s.Name(), tt.expected)
		}
	}
}
