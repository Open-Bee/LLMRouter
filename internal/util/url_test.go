package util

import "testing"

func TestJoinURL(t *testing.T) {
	tests := []struct {
		base, path, expected string
	}{
		{"http://host:8000/v1", "chat/completions", "http://host:8000/v1/chat/completions"},
		{"http://host:8000/v1/", "chat/completions", "http://host:8000/v1/chat/completions"},
		{"http://host:8000/v1", "/chat/completions", "http://host:8000/v1/chat/completions"},
		{"http://host:8000/v1/", "/chat/completions", "http://host:8000/v1/chat/completions"},
		{"http://host", "models", "http://host/models"},
	}

	for _, tt := range tests {
		got := JoinURL(tt.base, tt.path)
		if got != tt.expected {
			t.Errorf("JoinURL(%q, %q) = %q, want %q", tt.base, tt.path, got, tt.expected)
		}
	}
}

func TestChatCompletionsURL(t *testing.T) {
	got := ChatCompletionsURL("http://10.0.0.1:8000/v1")
	expected := "http://10.0.0.1:8000/v1/chat/completions"
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestModelsURL(t *testing.T) {
	got := ModelsURL("http://10.0.0.1:8000/v1")
	expected := "http://10.0.0.1:8000/v1/models"
	if got != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestMetricsURL(t *testing.T) {
	tests := []struct {
		apiBase  string
		expected string
	}{
		{"http://10.0.0.1:8000/v1", "http://10.0.0.1:8000/metrics"},
		{"http://10.0.0.1:8000/v1/", "http://10.0.0.1:8000/metrics"},
		{"http://host:9000/v1", "http://host:9000/metrics"},
	}

	for _, tt := range tests {
		got := MetricsURL(tt.apiBase)
		if got != tt.expected {
			t.Errorf("MetricsURL(%q) = %q, want %q", tt.apiBase, got, tt.expected)
		}
	}
}
