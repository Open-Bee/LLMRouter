package util

import "strings"

// JoinURL joins a base URL with a path, ensuring proper slash handling.
func JoinURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	path = strings.TrimLeft(path, "/")
	return base + "/" + path
}

// ChatCompletionsURL returns the full URL for the chat completions endpoint.
func ChatCompletionsURL(apiBase string) string {
	return JoinURL(apiBase, "chat/completions")
}

// ModelsURL returns the full URL for the models listing endpoint.
func ModelsURL(apiBase string) string {
	return JoinURL(apiBase, "models")
}

// MetricsURL returns the full URL for the Prometheus metrics endpoint.
// The metrics endpoint is at the root of the server, not under /v1.
func MetricsURL(apiBase string) string {
	// apiBase is like http://host:port/v1, metrics is at http://host:port/metrics
	base := strings.TrimSuffix(apiBase, "/v1")
	base = strings.TrimSuffix(base, "/v1/")
	return JoinURL(base, "metrics")
}
