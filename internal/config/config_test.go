package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("unexpected host: %s", cfg.Server.Host)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("unexpected port: %d", cfg.Server.Port)
	}
	if cfg.Routing.Strategy != "simple-shuffle" {
		t.Errorf("unexpected strategy: %s", cfg.Routing.Strategy)
	}
	if cfg.Routing.NumRetries != 3 {
		t.Errorf("unexpected num_retries: %d", cfg.Routing.NumRetries)
	}
	if cfg.HealthCheck.FailureThreshold != 3 {
		t.Errorf("unexpected failure_threshold: %d", cfg.HealthCheck.FailureThreshold)
	}
	if cfg.Log.Dir != "logs" {
		t.Errorf("unexpected log dir: %s", cfg.Log.Dir)
	}
	if cfg.Dashboard.Enabled != true {
		t.Error("dashboard should be enabled by default")
	}
}

func TestLoad_ValidConfig(t *testing.T) {
	content := `
server:
  host: "127.0.0.1"
  port: 9090
routing:
  strategy: "least-connections"
  num_retries: 5
log:
  level: "debug"
  dir: ""
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("expected host 127.0.0.1, got %s", cfg.Server.Host)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Server.Port)
	}
	if cfg.Routing.Strategy != "least-connections" {
		t.Errorf("expected least-connections, got %s", cfg.Routing.Strategy)
	}
	if cfg.Routing.NumRetries != 5 {
		t.Errorf("expected 5 retries, got %d", cfg.Routing.NumRetries)
	}
	// Defaults should be preserved for unset fields
	if cfg.HTTPClient.MaxIdleConns != 2000 {
		t.Errorf("expected default MaxIdleConns=2000, got %d", cfg.HTTPClient.MaxIdleConns)
	}
}

func TestLoad_NonExistentFile(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("{{invalid yaml"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}
