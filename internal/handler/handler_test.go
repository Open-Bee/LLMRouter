package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llm-router/internal/model"
	"llm-router/internal/pool"

	"github.com/rs/zerolog"
)

// --- Mock Router ---

type mockRouter struct {
	lastModel string
	lastBody  []byte
	called    bool
}

func (m *mockRouter) Route(w http.ResponseWriter, r *http.Request, model string, body []byte) {
	m.called = true
	m.lastModel = model
	m.lastBody = body
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"id":"test","choices":[]}`))
}

// --- ChatHandler Tests ---

func TestChatHandler_ValidRequest(t *testing.T) {
	router := &mockRouter{}
	handler := NewChatHandler(router, zerolog.Nop())

	body := `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if !router.called {
		t.Error("router should have been called")
	}
	if router.lastModel != "gpt-4" {
		t.Errorf("expected model gpt-4, got %s", router.lastModel)
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestChatHandler_MissingModel(t *testing.T) {
	router := &mockRouter{}
	handler := NewChatHandler(router, zerolog.Nop())

	body := `{"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if router.called {
		t.Error("router should NOT have been called")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestChatHandler_EmptyModel(t *testing.T) {
	router := &mockRouter{}
	handler := NewChatHandler(router, zerolog.Nop())

	body := `{"model":"","messages":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if router.called {
		t.Error("router should NOT have been called for empty model")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestChatHandler_WrongMethod(t *testing.T) {
	router := &mockRouter{}
	handler := NewChatHandler(router, zerolog.Nop())

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if router.called {
		t.Error("router should NOT be called for GET")
	}
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

// --- ModelsHandler Tests ---

func TestModelsHandler_ReturnsModels(t *testing.T) {
	p := pool.NewBackendPool()
	b1 := model.NewBackend(model.ServiceEndpoint{
		ModelName:     "ModelA",
		LiteLLMParams: model.LiteLLMParams{APIBase: "http://h1/v1"},
	})
	b2 := model.NewBackend(model.ServiceEndpoint{
		ModelName:     "ModelB",
		LiteLLMParams: model.LiteLLMParams{APIBase: "http://h2/v1"},
	})
	p.AddBackends([]*model.Backend{b1, b2})

	handler := NewModelsHandler(p, zerolog.Nop())
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp.Object != "list" {
		t.Errorf("expected object=list, got %s", resp.Object)
	}
	if len(resp.Data) != 2 {
		t.Errorf("expected 2 models, got %d", len(resp.Data))
	}

	// Verify all entries have correct structure
	for _, m := range resp.Data {
		if m.Object != "model" {
			t.Errorf("model object should be 'model', got %s", m.Object)
		}
		if m.OwnedBy != "llm-router" {
			t.Errorf("owned_by should be llm-router, got %s", m.OwnedBy)
		}
	}
}

func TestModelsHandler_EmptyPool(t *testing.T) {
	p := pool.NewBackendPool()
	handler := NewModelsHandler(p, zerolog.Nop())
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	var resp struct {
		Data []interface{} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Data) != 0 {
		t.Errorf("expected empty data, got %d items", len(resp.Data))
	}
}

// --- HealthHandler Tests ---

func TestHealthHandler(t *testing.T) {
	handler := NewHealthHandler()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("expected status=ok, got %s", resp.Status)
	}

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected application/json, got %s", ct)
	}
}
