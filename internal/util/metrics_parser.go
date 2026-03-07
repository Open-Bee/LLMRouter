package util

import (
	"bufio"
	"strconv"
	"strings"

	"llm-router/internal/model"
)

// MetricMapping defines the mapping from Prometheus metric names to our fields.
type MetricMapping struct {
	Name   string
	Field  string
	Engine model.EngineType
}

var metricMappings = []MetricMapping{
	// vLLM metrics
	{Name: "vllm:num_requests_running", Field: "running", Engine: model.EngineVLLM},
	{Name: "vllm:num_requests_waiting", Field: "waiting", Engine: model.EngineVLLM},
	{Name: "vllm:gpu_cache_usage_perc", Field: "gpu_cache", Engine: model.EngineVLLM},
	{Name: "vllm:kv_cache_usage_perc", Field: "gpu_cache", Engine: model.EngineVLLM},
	{Name: "vllm:prompt_tokens_total", Field: "prompt_tokens", Engine: model.EngineVLLM},
	{Name: "vllm:generation_tokens_total", Field: "generation_tokens", Engine: model.EngineVLLM},
	// SGLang metrics
	{Name: "sglang:num_running_reqs", Field: "running", Engine: model.EngineSGLang},
	{Name: "sglang:num_queue_reqs", Field: "waiting", Engine: model.EngineSGLang},
	{Name: "sglang:token_usage", Field: "gpu_cache", Engine: model.EngineSGLang},
	// Generic metrics
	{Name: "num_requests_running", Field: "running", Engine: model.EngineUnknown},
	{Name: "num_requests_waiting", Field: "waiting", Engine: model.EngineUnknown},
}

// ParsePrometheusMetrics parses Prometheus text format and extracts relevant LLM metrics.
// Returns the load metrics and the detected engine type.
func ParsePrometheusMetrics(body string) (*model.LoadMetrics, model.EngineType) {
	metrics := &model.LoadMetrics{}
	engineType := model.EngineUnknown
	found := make(map[string]bool)

	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		for _, mapping := range metricMappings {
			if !strings.HasPrefix(line, mapping.Name) {
				continue
			}

			// Check it's an exact metric name match (followed by space, { or EOF)
			rest := line[len(mapping.Name):]
			if len(rest) > 0 && rest[0] != ' ' && rest[0] != '{' {
				continue
			}

			value := extractValue(line)
			if value < 0 {
				continue
			}

			if mapping.Engine != model.EngineUnknown {
				engineType = mapping.Engine
			}

		switch mapping.Field {
		case "running":
			if !found["running"] {
				metrics.RunningRequests = int(value)
				found["running"] = true
			}
		case "waiting":
			if !found["waiting"] {
				metrics.WaitingRequests = int(value)
				found["waiting"] = true
			}
		case "gpu_cache":
			if !found["gpu_cache"] {
				// Values <= 1.0 are percentages that need to be multiplied by 100
				if value <= 1.0 {
					value *= 100
				}
				metrics.GPUCacheUsage = value
				found["gpu_cache"] = true
			}
		case "prompt_tokens":
			if !found["prompt_tokens"] {
				metrics.PromptTokens = value
				found["prompt_tokens"] = true
			}
		case "generation_tokens":
			if !found["generation_tokens"] {
				metrics.GenerationTokens = value
				found["generation_tokens"] = true
			}
		}
		}
	}

	return metrics, engineType
}

// extractValue extracts the float value from a Prometheus metric line.
// Format: metric_name{labels} value OR metric_name value
func extractValue(line string) float64 {
	// Find the value part: last space-separated token
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return -1
	}

	valueStr := parts[len(parts)-1]
	// Use strconv.ParseFloat to support scientific notation (e.g. 3.2e+07)
	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return -1
	}
	return value
}

// parseFloat is a simple float parser to avoid importing strconv in hot path.
func parseFloat(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	negative := false
	if s[0] == '-' {
		negative = true
		s = s[1:]
	}

	var integer, fraction float64
	var divisor float64 = 1
	dotSeen := false

	for _, c := range s {
		if c == '.' {
			if dotSeen {
				return 0, false
			}
			dotSeen = true
			continue
		}
		if c < '0' || c > '9' {
			return 0, false
		}
		digit := float64(c - '0')
		if dotSeen {
			divisor *= 10
			fraction += digit / divisor
		} else {
			integer = integer*10 + digit
		}
	}

	result := integer + fraction
	if negative {
		result = -result
	}
	return result, true
}
