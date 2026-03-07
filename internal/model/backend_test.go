package model

import (
	"sync"
	"testing"
	"time"
)

func makeEndpoint(modelName, apiBase string) ServiceEndpoint {
	return ServiceEndpoint{
		ModelName: modelName,
		LiteLLMParams: LiteLLMParams{
			Model:          "openai/" + modelName,
			APIBase:        apiBase,
			APIKey:         "test-key",
			SupportsVision: true,
			Weight:         2.0,
			SourceType:     "self_deployed",
			RPMLimit:       nil,
		},
	}
}

func intPtr(v int) *int { return &v }

func TestNewBackend(t *testing.T) {
	ep := makeEndpoint("TestModel", "http://10.0.0.1:8000/v1")
	b := NewBackend(ep)

	if b.ID != "TestModel@http://10.0.0.1:8000/v1" {
		t.Errorf("unexpected ID: %s", b.ID)
	}
	if b.ModelName != "TestModel" {
		t.Errorf("unexpected ModelName: %s", b.ModelName)
	}
	if b.APIBase != "http://10.0.0.1:8000/v1" {
		t.Errorf("unexpected APIBase: %s", b.APIBase)
	}
	if b.APIKey != "test-key" {
		t.Errorf("unexpected APIKey: %s", b.APIKey)
	}
	if !b.SupportsVision {
		t.Error("expected SupportsVision=true")
	}
	if b.Weight != 2.0 {
		t.Errorf("unexpected Weight: %f", b.Weight)
	}
	if b.SourceType != "self_deployed" {
		t.Errorf("unexpected SourceType: %s", b.SourceType)
	}
	if b.RPMLimit != nil {
		t.Errorf("expected nil RPMLimit")
	}
	if !b.IsHealthy() {
		t.Error("new backend should be healthy")
	}
	if b.ActiveConnections() != 0 {
		t.Errorf("new backend should have 0 connections")
	}
	if b.ConsecutiveFailures() != 0 {
		t.Errorf("new backend should have 0 failures")
	}
	if b.GetEngineType() != EngineUnknown {
		t.Errorf("new backend should have unknown engine type")
	}
	if b.GetLoadMetrics() != nil {
		t.Error("new backend should have nil load metrics")
	}
}

func TestNewBackend_DefaultWeight(t *testing.T) {
	ep := ServiceEndpoint{
		ModelName: "M",
		LiteLLMParams: LiteLLMParams{
			APIBase: "http://localhost/v1",
			Weight:  0, // zero weight -> default 1.0
		},
	}
	b := NewBackend(ep)
	if b.Weight != 1.0 {
		t.Errorf("expected default weight 1.0, got %f", b.Weight)
	}
}

func TestNewBackend_WithRPMLimit(t *testing.T) {
	limit := 100
	ep := ServiceEndpoint{
		ModelName: "M",
		LiteLLMParams: LiteLLMParams{
			APIBase:  "http://localhost/v1",
			RPMLimit: &limit,
		},
	}
	b := NewBackend(ep)
	if b.RPMLimit == nil || *b.RPMLimit != 100 {
		t.Errorf("expected RPMLimit=100")
	}
}

func TestBackend_HealthState(t *testing.T) {
	ep := makeEndpoint("M", "http://localhost/v1")
	b := NewBackend(ep)

	if !b.IsHealthy() {
		t.Error("should start healthy")
	}

	b.SetHealthy(false)
	if b.IsHealthy() {
		t.Error("should be unhealthy")
	}

	b.SetHealthy(true)
	if !b.IsHealthy() {
		t.Error("should be healthy again")
	}
}

func TestBackend_Connections(t *testing.T) {
	ep := makeEndpoint("M", "http://localhost/v1")
	b := NewBackend(ep)

	b.IncrConnections()
	b.IncrConnections()
	b.IncrConnections()
	if b.ActiveConnections() != 3 {
		t.Errorf("expected 3 connections, got %d", b.ActiveConnections())
	}

	b.DecrConnections()
	if b.ActiveConnections() != 2 {
		t.Errorf("expected 2 connections, got %d", b.ActiveConnections())
	}
}

func TestBackend_Failures(t *testing.T) {
	ep := makeEndpoint("M", "http://localhost/v1")
	b := NewBackend(ep)

	f1 := b.IncrFailures()
	f2 := b.IncrFailures()
	f3 := b.IncrFailures()

	if f1 != 1 || f2 != 2 || f3 != 3 {
		t.Errorf("failure increments wrong: %d %d %d", f1, f2, f3)
	}
	if b.ConsecutiveFailures() != 3 {
		t.Errorf("expected 3 failures")
	}

	b.ResetFailures()
	if b.ConsecutiveFailures() != 0 {
		t.Errorf("expected 0 failures after reset")
	}
}

func TestBackend_LastHealthCheck(t *testing.T) {
	ep := makeEndpoint("M", "http://localhost/v1")
	b := NewBackend(ep)

	if !b.LastHealthCheck().IsZero() {
		t.Error("initial last health check should be zero")
	}

	now := time.Now()
	b.SetLastHealthCheck(now)
	if !b.LastHealthCheck().Equal(now) {
		t.Error("last health check mismatch")
	}
}

func TestBackend_EngineType(t *testing.T) {
	ep := makeEndpoint("M", "http://localhost/v1")
	b := NewBackend(ep)

	if b.GetEngineType() != EngineUnknown {
		t.Error("should start unknown")
	}

	b.SetEngineType(EngineVLLM)
	if b.GetEngineType() != EngineVLLM {
		t.Error("should be vllm")
	}

	b.SetEngineType(EngineSGLang)
	if b.GetEngineType() != EngineSGLang {
		t.Error("should be sglang")
	}
}

func TestBackend_LoadMetrics(t *testing.T) {
	ep := makeEndpoint("M", "http://localhost/v1")
	b := NewBackend(ep)

	if b.GetLoadMetrics() != nil {
		t.Error("initial metrics should be nil")
	}

	m := &LoadMetrics{RunningRequests: 5, WaitingRequests: 3, GPUCacheUsage: 50.0}
	b.SetLoadMetrics(m)

	got := b.GetLoadMetrics()
	if got == nil {
		t.Fatal("metrics should not be nil")
	}
	if got.RunningRequests != 5 || got.WaitingRequests != 3 {
		t.Errorf("metrics mismatch: %+v", got)
	}
}

func TestBackend_ConcurrentAccess(t *testing.T) {
	ep := makeEndpoint("M", "http://localhost/v1")
	b := NewBackend(ep)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.IncrConnections()
			b.IsHealthy()
			b.SetHealthy(true)
			b.IncrFailures()
			b.GetEngineType()
			b.GetLoadMetrics()
			b.DecrConnections()
			b.ResetFailures()
		}()
	}
	wg.Wait()
	// No race or panic = pass
}
