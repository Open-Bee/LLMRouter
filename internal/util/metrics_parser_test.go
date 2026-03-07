package util

import (
	"testing"

	"llm-router/internal/model"
)

func TestParsePrometheusMetrics_VLLM(t *testing.T) {
	body := `# HELP vllm:num_requests_running Number of requests currently running
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running 5
# HELP vllm:num_requests_waiting Number of requests waiting
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting 3
# HELP vllm:gpu_cache_usage_perc GPU cache usage percentage
# TYPE vllm:gpu_cache_usage_perc gauge
vllm:gpu_cache_usage_perc 0.75
`
	metrics, engine := ParsePrometheusMetrics(body)

	if engine != model.EngineVLLM {
		t.Errorf("expected vllm engine, got %s", engine)
	}
	if metrics.RunningRequests != 5 {
		t.Errorf("running: expected 5, got %d", metrics.RunningRequests)
	}
	if metrics.WaitingRequests != 3 {
		t.Errorf("waiting: expected 3, got %d", metrics.WaitingRequests)
	}
	// 0.75 <= 1.0, so multiplied by 100 = 75.0
	if metrics.GPUCacheUsage != 75.0 {
		t.Errorf("gpu_cache: expected 75.0, got %f", metrics.GPUCacheUsage)
	}
}

func TestParsePrometheusMetrics_SGLang(t *testing.T) {
	body := `# HELP sglang:num_running_reqs Running requests
# TYPE sglang:num_running_reqs gauge
sglang:num_running_reqs 10
# HELP sglang:num_queue_reqs Queued requests
# TYPE sglang:num_queue_reqs gauge
sglang:num_queue_reqs 7
# HELP sglang:token_usage Token usage
# TYPE sglang:token_usage gauge
sglang:token_usage 0.5
`
	metrics, engine := ParsePrometheusMetrics(body)

	if engine != model.EngineSGLang {
		t.Errorf("expected sglang engine, got %s", engine)
	}
	if metrics.RunningRequests != 10 {
		t.Errorf("running: expected 10, got %d", metrics.RunningRequests)
	}
	if metrics.WaitingRequests != 7 {
		t.Errorf("waiting: expected 7, got %d", metrics.WaitingRequests)
	}
	if metrics.GPUCacheUsage != 50.0 {
		t.Errorf("gpu_cache: expected 50.0, got %f", metrics.GPUCacheUsage)
	}
}

func TestParsePrometheusMetrics_Generic(t *testing.T) {
	body := `num_requests_running 2
num_requests_waiting 1
`
	metrics, engine := ParsePrometheusMetrics(body)

	if engine != model.EngineUnknown {
		t.Errorf("expected unknown engine, got %s", engine)
	}
	if metrics.RunningRequests != 2 {
		t.Errorf("running: expected 2, got %d", metrics.RunningRequests)
	}
	if metrics.WaitingRequests != 1 {
		t.Errorf("waiting: expected 1, got %d", metrics.WaitingRequests)
	}
}

func TestParsePrometheusMetrics_WithLabels(t *testing.T) {
	body := `vllm:num_requests_running{model="test"} 8
vllm:num_requests_waiting{model="test"} 4
`
	metrics, engine := ParsePrometheusMetrics(body)

	if engine != model.EngineVLLM {
		t.Errorf("expected vllm, got %s", engine)
	}
	if metrics.RunningRequests != 8 {
		t.Errorf("running: expected 8, got %d", metrics.RunningRequests)
	}
	if metrics.WaitingRequests != 4 {
		t.Errorf("waiting: expected 4, got %d", metrics.WaitingRequests)
	}
}

func TestParsePrometheusMetrics_Empty(t *testing.T) {
	metrics, engine := ParsePrometheusMetrics("")
	if engine != model.EngineUnknown {
		t.Errorf("expected unknown for empty, got %s", engine)
	}
	if metrics.RunningRequests != 0 || metrics.WaitingRequests != 0 {
		t.Error("empty should have zero metrics")
	}
}

func TestParsePrometheusMetrics_CommentsOnly(t *testing.T) {
	body := `# This is a comment
# Another comment
`
	metrics, _ := ParsePrometheusMetrics(body)
	if metrics.RunningRequests != 0 {
		t.Error("comments only should have zero metrics")
	}
}

func TestParsePrometheusMetrics_PartialMatch(t *testing.T) {
	// "num_requests_running_total" should NOT match "num_requests_running"
	body := `num_requests_running_total 999
num_requests_running 3
`
	metrics, _ := ParsePrometheusMetrics(body)
	if metrics.RunningRequests != 3 {
		t.Errorf("expected 3, got %d (partial match bug)", metrics.RunningRequests)
	}
}

func TestParsePrometheusMetrics_GPUCacheAbove1(t *testing.T) {
	// gpu_cache value > 1.0 should not be multiplied
	body := `vllm:gpu_cache_usage_perc 55.5
`
	metrics, _ := ParsePrometheusMetrics(body)
	if metrics.GPUCacheUsage != 55.5 {
		t.Errorf("expected 55.5, got %f", metrics.GPUCacheUsage)
	}
}

func TestParseFloat(t *testing.T) {
	tests := []struct {
		input    string
		expected float64
		ok       bool
	}{
		{"123", 123, true},
		{"0", 0, true},
		{"3.14", 3.14, true},
		{"0.5", 0.5, true},
		{"-1.5", -1.5, true},
		{"", 0, false},
		{"abc", 0, false},
		{"1.2.3", 0, false},
	}

	for _, tt := range tests {
		got, ok := parseFloat(tt.input)
		if ok != tt.ok {
			t.Errorf("parseFloat(%q): ok=%v, want %v", tt.input, ok, tt.ok)
			continue
		}
		if ok && got != tt.expected {
			t.Errorf("parseFloat(%q) = %f, want %f", tt.input, got, tt.expected)
		}
	}
}
