package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"llm-router/internal/model"
	"llm-router/internal/pool"

	"github.com/rs/zerolog"
)

func makeTestPool(backends ...*model.Backend) *pool.BackendPool {
	p := pool.NewBackendPool()
	if len(backends) > 0 {
		p.AddBackends(backends)
	}
	return p
}

func makeBackend(name, apiBase string) *model.Backend {
	return model.NewBackend(model.ServiceEndpoint{
		ModelName:     name,
		LiteLLMParams: model.LiteLLMParams{APIBase: apiBase, Weight: 1.0},
	})
}

func TestChecker_HealthyBackend(t *testing.T) {
	// Create a mock backend that returns 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	b := makeBackend("M", srv.URL+"/v1")
	p := makeTestPool(b)

	logger := zerolog.Nop()
	checker := NewChecker(p, 30*time.Second, 5*time.Second, 3, 10, logger)

	checker.CheckAll(context.Background())

	if !b.IsHealthy() {
		t.Error("backend should be healthy after successful check")
	}
	if b.ConsecutiveFailures() != 0 {
		t.Error("failures should be 0 after successful check")
	}
}

func TestChecker_UnhealthyAfterThreshold(t *testing.T) {
	// Create a mock backend that returns 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	b := makeBackend("M", srv.URL+"/v1")
	p := makeTestPool(b)

	logger := zerolog.Nop()
	checker := NewChecker(p, 30*time.Second, 5*time.Second, 3, 10, logger)

	// Check 3 times (threshold=3)
	for i := 0; i < 3; i++ {
		checker.CheckAll(context.Background())
	}

	if b.IsHealthy() {
		t.Error("backend should be unhealthy after 3 consecutive failures")
	}
	if b.ConsecutiveFailures() < 3 {
		t.Errorf("expected >= 3 failures, got %d", b.ConsecutiveFailures())
	}
}

func TestChecker_RecoveryAfterFailure(t *testing.T) {
	var mu sync.Mutex
	healthy := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := healthy
		mu.Unlock()
		if h {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data":[]}`))
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	b := makeBackend("M", srv.URL+"/v1")
	p := makeTestPool(b)

	logger := zerolog.Nop()
	checker := NewChecker(p, 30*time.Second, 5*time.Second, 2, 10, logger)

	// Fail twice (threshold=2)
	checker.CheckAll(context.Background())
	checker.CheckAll(context.Background())
	if b.IsHealthy() {
		t.Error("should be unhealthy")
	}

	// Now recover
	mu.Lock()
	healthy = true
	mu.Unlock()

	checker.CheckAll(context.Background())
	if !b.IsHealthy() {
		t.Error("should recover to healthy")
	}
	if b.ConsecutiveFailures() != 0 {
		t.Error("failures should be reset on recovery")
	}
}

func TestChecker_StatusChangeNotification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	b := makeBackend("M", srv.URL+"/v1")
	p := makeTestPool(b)

	logger := zerolog.Nop()
	checker := NewChecker(p, 30*time.Second, 5*time.Second, 1, 10, logger)

	var changes []StatusChange
	var mu sync.Mutex
	checker.OnStatusChange(func(change StatusChange) {
		mu.Lock()
		changes = append(changes, change)
		mu.Unlock()
	})

	checker.CheckAll(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(changes) != 1 {
		t.Fatalf("expected 1 status change, got %d", len(changes))
	}
	if changes[0].Healthy {
		t.Error("change should indicate unhealthy")
	}
}

func TestChecker_ConnectionRefused(t *testing.T) {
	// Use a URL that will refuse connections
	b := makeBackend("M", "http://127.0.0.1:1/v1")
	p := makeTestPool(b)

	logger := zerolog.Nop()
	checker := NewChecker(p, 30*time.Second, 2*time.Second, 1, 10, logger)

	checker.CheckAll(context.Background())

	if b.IsHealthy() {
		t.Error("should be unhealthy after connection refused")
	}
}

func TestChecker_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	p := makeTestPool()
	logger := zerolog.Nop()
	checker := NewChecker(p, 100*time.Millisecond, 5*time.Second, 3, 10, logger)

	// Run should return quickly due to cancelled context
	done := make(chan struct{})
	go func() {
		checker.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Error("Run should have returned quickly after context cancel")
	}
}
