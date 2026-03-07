package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"llm-router/internal/model"
	"llm-router/internal/monitor"
	"llm-router/internal/pool"
	"llm-router/internal/ratelimit"
	"llm-router/internal/routing"
	"llm-router/internal/util"

	"github.com/rs/zerolog"
)

func makeBackend(name, apiBase string) *model.Backend {
	return model.NewBackend(model.ServiceEndpoint{
		ModelName:     name,
		LiteLLMParams: model.LiteLLMParams{APIBase: apiBase, Weight: 1.0},
	})
}

func intPtr(v int) *int { return &v }

func setupManager(backends []*model.Backend, opts ...ManagerOption) (*Manager, *pool.BackendPool) {
	p := pool.NewBackendPool()
	if len(backends) > 0 {
		p.AddBackends(backends)
	}
	strategy := routing.NewSimpleShuffle()
	rl := ratelimit.NewRateLimiter()
	crpm := monitor.NewCompletionRPM()
	stats := util.NewRequestStats()
	client := &http.Client{Timeout: 10 * time.Second}
	logger := zerolog.Nop()

	defaultOpts := []ManagerOption{
		WithMaxRetries(2),
		WithBackoff(10, 100, 0.1),
		WithFailureThreshold(3),
	}
	allOpts := append(defaultOpts, opts...)

	mgr := NewManager(p, strategy, rl, crpm, stats, client, logger, allOpts...)
	return mgr, p
}

// --- Route Tests ---

func TestRoute_ModelNotFound(t *testing.T) {
	mgr, _ := setupManager(nil)

	body := []byte(`{"model":"nonexistent"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	w := httptest.NewRecorder()

	mgr.Route(w, req, "nonexistent", body)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestRoute_SuccessfulForward(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"resp1","choices":[]}`)
	}))
	defer srv.Close()

	b := makeBackend("TestModel", srv.URL+"/v1")
	mgr, _ := setupManager([]*model.Backend{b})

	body := []byte(`{"model":"TestModel","messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	mgr.Route(w, req, "TestModel", body)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "resp1") {
		t.Error("response body mismatch")
	}
}

func TestRoute_4xxNotRetried(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"bad request"}`)
	}))
	defer srv.Close()

	b := makeBackend("M", srv.URL+"/v1")
	mgr, _ := setupManager([]*model.Backend{b})

	body := []byte(`{"model":"M"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	mgr.Route(w, req, "M", body)

	if callCount != 1 {
		t.Errorf("4xx should not be retried, but was called %d times", callCount)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 passthrough, got %d", w.Code)
	}
}

func TestRoute_5xxRetried(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "error")
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"ok"}`)
	}))
	defer srv.Close()

	b := makeBackend("M", srv.URL+"/v1")
	mgr, _ := setupManager([]*model.Backend{b})

	body := []byte(`{"model":"M"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	mgr.Route(w, req, "M", body)

	if callCount < 2 {
		t.Errorf("expected at least 2 calls (retry), got %d", callCount)
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected eventual 200, got %d", w.Code)
	}
}

func TestRoute_AllBackendsUnhealthy(t *testing.T) {
	b := makeBackend("M", "http://127.0.0.1:1/v1")
	b.SetHealthy(false)
	mgr, _ := setupManager([]*model.Backend{b})

	body := []byte(`{"model":"M"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	mgr.Route(w, req, "M", body)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestRoute_ConnectionRefusedRetried(t *testing.T) {
	// First backend: connection refused, second: success
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"ok"}`)
	}))
	defer goodSrv.Close()

	badBackend := makeBackend("M", "http://127.0.0.1:1/v1")
	goodBackend := makeBackend("M", goodSrv.URL+"/v1")

	mgr, _ := setupManager([]*model.Backend{badBackend, goodBackend})

	body := []byte(`{"model":"M"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	mgr.Route(w, req, "M", body)

	// Should eventually succeed via the good backend
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 after failover, got %d", w.Code)
	}
}

func TestRoute_FailureThresholdMarksUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	b := makeBackend("M", srv.URL+"/v1")
	mgr, _ := setupManager([]*model.Backend{b}, WithFailureThreshold(2))

	body := []byte(`{"model":"M"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	mgr.Route(w, req, "M", body)

	// After retries exhaust, backend should be marked unhealthy
	if b.IsHealthy() {
		t.Error("backend should be unhealthy after exceeding failure threshold")
	}
}

// --- classifyError Tests ---

func TestClassifyError_Nil(t *testing.T) {
	if classifyError(nil) != 0 {
		t.Error("nil error should return 0")
	}
}

func TestClassifyError_Timeout(t *testing.T) {
	err := context.DeadlineExceeded
	if classifyError(err) != ErrTimeout {
		t.Error("DeadlineExceeded should be ErrTimeout")
	}
}

func TestClassifyError_ConnectionRefused(t *testing.T) {
	err := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	if classifyError(err) != ErrConnRefused {
		t.Error("dial OpError should be ErrConnRefused")
	}
}

func TestClassifyError_ConnReset(t *testing.T) {
	err := fmt.Errorf("something: %w", syscall.ECONNRESET)
	if classifyError(err) != ErrConnReset {
		t.Error("ECONNRESET should be ErrConnReset")
	}
}

func TestClassifyError_DNSError(t *testing.T) {
	err := &net.DNSError{Err: "no such host", Name: "invalid.host"}
	if classifyError(err) != ErrConnRefused {
		t.Error("DNS error should be ErrConnRefused")
	}
}

func TestClassifyError_MessagePatterns(t *testing.T) {
	tests := []struct {
		msg      string
		expected ErrorType
	}{
		{"connection refused", ErrConnRefused},
		{"connection reset by peer", ErrConnReset},
		{"broken pipe", ErrConnReset},
		{"request timeout exceeded", ErrTimeout},
		{"context deadline exceeded", ErrTimeout},
		{"unknown error xyz", ErrConnReset}, // default
	}

	for _, tt := range tests {
		err := errors.New(tt.msg)
		got := classifyError(err)
		if got != tt.expected {
			t.Errorf("classifyError(%q) = %d, want %d", tt.msg, got, tt.expected)
		}
	}
}

// --- backoffDelay Tests ---

func TestBackoffDelay(t *testing.T) {
	mgr, _ := setupManager(nil, WithBackoff(100, 5000, 0.0))

	d1 := mgr.backoffDelay(1)
	d2 := mgr.backoffDelay(2)
	d3 := mgr.backoffDelay(3)

	// With 0 jitter, delays should be exact: 100, 200, 400
	if d1 != 100*time.Millisecond {
		t.Errorf("attempt 1: expected 100ms, got %v", d1)
	}
	if d2 != 200*time.Millisecond {
		t.Errorf("attempt 2: expected 200ms, got %v", d2)
	}
	if d3 != 400*time.Millisecond {
		t.Errorf("attempt 3: expected 400ms, got %v", d3)
	}
}

func TestBackoffDelay_CappedAtMax(t *testing.T) {
	mgr, _ := setupManager(nil, WithBackoff(1000, 2000, 0.0))

	d := mgr.backoffDelay(10) // Would be 1000 * 2^9 = 512000 without cap
	if d != 2000*time.Millisecond {
		t.Errorf("expected cap at 2000ms, got %v", d)
	}
}

func TestBackoffDelay_WithJitter(t *testing.T) {
	mgr, _ := setupManager(nil, WithBackoff(100, 5000, 0.5))

	// With jitter, results should vary
	seen := make(map[time.Duration]bool)
	for i := 0; i < 100; i++ {
		d := mgr.backoffDelay(1)
		seen[d] = true
		// Should be within [50ms, 150ms] with 50% jitter on 100ms base
		if d < 0 || d > 200*time.Millisecond {
			t.Errorf("delay %v outside expected range", d)
		}
	}
	if len(seen) < 2 {
		t.Error("jitter should produce varying delays")
	}
}

// --- Options Tests ---

func TestManagerOptions(t *testing.T) {
	p := pool.NewBackendPool()
	mgr := NewManager(p, routing.NewSimpleShuffle(), ratelimit.NewRateLimiter(),
		monitor.NewCompletionRPM(), util.NewRequestStats(), &http.Client{}, zerolog.Nop(),
		WithMaxRetries(5),
		WithBackoff(200, 10000, 0.3),
		WithFailureThreshold(10),
	)

	if mgr.maxRetries != 5 {
		t.Errorf("maxRetries: expected 5, got %d", mgr.maxRetries)
	}
	if mgr.baseDelay != 200*time.Millisecond {
		t.Errorf("baseDelay: expected 200ms, got %v", mgr.baseDelay)
	}
	if mgr.maxDelay != 10000*time.Millisecond {
		t.Errorf("maxDelay: expected 10s, got %v", mgr.maxDelay)
	}
	if mgr.failureThreshold != 10 {
		t.Errorf("failureThreshold: expected 10, got %d", mgr.failureThreshold)
	}
}
