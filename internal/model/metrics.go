package model

import "time"

// LoadMetrics holds the load metrics collected from a backend's Prometheus endpoint.
type LoadMetrics struct {
	RunningRequests  int     `json:"running_requests"`
	WaitingRequests  int     `json:"waiting_requests"`
	GPUCacheUsage    float64 `json:"gpu_cache_usage"`
	PromptTokens     float64 `json:"prompt_tokens"`
	GenerationTokens float64 `json:"generation_tokens"`
	CollectedAt      time.Time `json:"collected_at"`
	Error            string  `json:"error,omitempty"`
}

// EngineType represents the type of LLM serving engine.
type EngineType string

const (
	EngineVLLM    EngineType = "vllm"
	EngineSGLang  EngineType = "sglang"
	EngineUnknown EngineType = "unknown"
)
