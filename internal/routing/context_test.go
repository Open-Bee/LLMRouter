package routing

import (
	"testing"

	"llm-router/internal/model"
)

func makeCtxBackend(id string) *model.Backend {
	return model.NewBackend(model.ServiceEndpoint{
		ModelName:     "M",
		LiteLLMParams: model.LiteLLMParams{APIBase: "http://" + id + "/v1"},
	})
}

func TestContext_NewContext(t *testing.T) {
	ctx := NewContext("TestModel", 3)
	if ctx.Model != "TestModel" {
		t.Errorf("unexpected model: %s", ctx.Model)
	}
	if ctx.MaxRetries != 3 {
		t.Errorf("unexpected max retries: %d", ctx.MaxRetries)
	}
	if ctx.AttemptNum != 0 {
		t.Errorf("unexpected attempt num: %d", ctx.AttemptNum)
	}
}

func TestContext_CanRetry(t *testing.T) {
	ctx := NewContext("M", 2)
	// 0 attempts: can retry (0 <= 2)
	if !ctx.CanRetry() {
		t.Error("should be able to retry at attempt 0")
	}

	b := makeCtxBackend("b1")
	ctx.MarkAttempted(b) // attempt 1
	if !ctx.CanRetry() {
		t.Error("should be able to retry at attempt 1")
	}

	ctx.MarkAttempted(b) // attempt 2
	if !ctx.CanRetry() {
		t.Error("should be able to retry at attempt 2")
	}

	ctx.MarkAttempted(b) // attempt 3
	if ctx.CanRetry() {
		t.Error("should NOT be able to retry at attempt 3 (max=2)")
	}
}

func TestContext_MarkAndHasAttempted(t *testing.T) {
	ctx := NewContext("M", 5)
	b1 := makeCtxBackend("b1")
	b2 := makeCtxBackend("b2")

	if ctx.HasAttempted(b1) {
		t.Error("b1 should not be attempted yet")
	}

	ctx.MarkAttempted(b1)
	if !ctx.HasAttempted(b1) {
		t.Error("b1 should be attempted")
	}
	if ctx.HasAttempted(b2) {
		t.Error("b2 should not be attempted")
	}

	if ctx.Attempted[b1.ID] != 1 {
		t.Errorf("b1 attempt count should be 1, got %d", ctx.Attempted[b1.ID])
	}

	ctx.MarkAttempted(b1)
	if ctx.Attempted[b1.ID] != 2 {
		t.Errorf("b1 attempt count should be 2, got %d", ctx.Attempted[b1.ID])
	}
}

func TestContext_FilterUnattempted(t *testing.T) {
	ctx := NewContext("M", 5)
	b1 := makeCtxBackend("b1")
	b2 := makeCtxBackend("b2")
	b3 := makeCtxBackend("b3")

	all := []*model.Backend{b1, b2, b3}

	// Nothing attempted -> return all
	result := ctx.FilterUnattempted(all)
	if len(result) != 3 {
		t.Errorf("expected 3, got %d", len(result))
	}

	// Mark b1 attempted
	ctx.MarkAttempted(b1)
	result = ctx.FilterUnattempted(all)
	if len(result) != 2 {
		t.Errorf("expected 2 unattempted, got %d", len(result))
	}
	for _, b := range result {
		if b == b1 {
			t.Error("b1 should be filtered out")
		}
	}

	// Mark all attempted -> should return full list
	ctx.MarkAttempted(b2)
	ctx.MarkAttempted(b3)
	result = ctx.FilterUnattempted(all)
	if len(result) != 3 {
		t.Errorf("all attempted -> should return full list, got %d", len(result))
	}
}
