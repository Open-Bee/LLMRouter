package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the main configuration structure loaded from the primary YAML config file.
type Config struct {
	Server      ServerConfig      `yaml:"server"`
	Routing     RoutingConfig     `yaml:"routing"`
	HTTPClient  HTTPClientConfig  `yaml:"http_client"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	Retry       RetryConfig       `yaml:"retry"`
	Webhook     WebhookConfig     `yaml:"webhook"`
	Monitor     MonitorConfig     `yaml:"monitor"`
	Backends    BackendsConfig    `yaml:"backends"`
	Dashboard   DashboardConfig   `yaml:"dashboard"`
	Log         LogConfig         `yaml:"log"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// RoutingConfig holds routing strategy settings.
type RoutingConfig struct {
	Strategy   string `yaml:"strategy"`
	NumRetries int    `yaml:"num_retries"`
}

// HTTPClientConfig holds settings for the outgoing HTTP client.
type HTTPClientConfig struct {
	RequestTimeout    int `yaml:"request_timeout"`
	ConnectTimeout    int `yaml:"connect_timeout"`
	MaxIdleConns      int `yaml:"max_idle_conns"`
	MaxConnsPerHost   int `yaml:"max_conns_per_host"`
	IdleConnTimeout   int `yaml:"idle_conn_timeout"`
}

// HealthCheckConfig holds settings for asynchronous health checking.
type HealthCheckConfig struct {
	Interval              int  `yaml:"interval"`
	Timeout               int  `yaml:"timeout"`
	FailureThreshold      int  `yaml:"failure_threshold"`
	MaxConcurrency        int  `yaml:"max_concurrency"`
	PruneUnhealthyOnStart bool `yaml:"prune_unhealthy_on_start"` // remove unhealthy backends from config files on startup
}

// RetryConfig holds retry and backoff settings.
type RetryConfig struct {
	BaseDelay        int     `yaml:"base_delay_ms"`
	MaxDelay         int     `yaml:"max_delay_ms"`
	JitterFactor     float64 `yaml:"jitter_factor"`
	FailureThreshold int     `yaml:"failure_threshold"`
}

// WebhookConfig holds settings for webhook notifications.
type WebhookConfig struct {
	Enabled          bool   `yaml:"enabled"`
	URL              string `yaml:"url"`
	CooldownSeconds  int    `yaml:"cooldown_seconds"`
	BatchInterval    int    `yaml:"batch_interval"`
	ReportIntervalActive int `yaml:"report_interval_active"`
	ReportIntervalIdle   int `yaml:"report_interval_idle"`
}

// MonitorConfig holds settings for Prometheus metrics collection.
type MonitorConfig struct {
	CollectInterval int `yaml:"collect_interval"`
}

// BackendsConfig specifies the backend configuration directory and enabled source types.
type BackendsConfig struct {
	ConfigDir        string   `yaml:"config_dir"`
	EnabledSources   []string `yaml:"enabled_sources"`
}

// DashboardConfig holds settings for the terminal TUI dashboard.
type DashboardConfig struct {
	Enabled        bool `yaml:"enabled"`
	RefreshInterval int  `yaml:"refresh_interval"`
	WebPort        int  `yaml:"web_port"` // port for the web dashboard; 0 = disabled
}

// LogConfig holds logging settings.
type LogConfig struct {
	Level      string `yaml:"level"`
	Dir        string `yaml:"dir"`          // directory for log files; empty = no file logging
	MaxSizeMB  int    `yaml:"max_size_mb"`   // max size per log file in MB before rotation
	MaxBackups int    `yaml:"max_backups"`   // number of old log files to keep
	MaxAgeDays int    `yaml:"max_age_days"` // max age in days to retain old log files
}

// DefaultConfig returns a Config with sensible default values.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "0.0.0.0",
			Port: 8080,
		},
		Routing: RoutingConfig{
			Strategy:   "simple-shuffle",
			NumRetries: 3,
		},
		HTTPClient: HTTPClientConfig{
			RequestTimeout:  3600,
			ConnectTimeout:  10,
			MaxIdleConns:    2000,
			MaxConnsPerHost: 0,
			IdleConnTimeout: 90,
		},
		HealthCheck: HealthCheckConfig{
			Interval:              30,
			Timeout:               10,
			FailureThreshold:      3,
			MaxConcurrency:        20,
			PruneUnhealthyOnStart: false,
		},
		Retry: RetryConfig{
			BaseDelay:        500,
			MaxDelay:         5000,
			JitterFactor:     0.2,
			FailureThreshold: 3,
		},
		Webhook: WebhookConfig{
			Enabled:              false,
			CooldownSeconds:      300,
			BatchInterval:        60,
			ReportIntervalActive: 3600,
			ReportIntervalIdle:   10800,
		},
		Monitor: MonitorConfig{
			CollectInterval: 3,
		},
		Backends: BackendsConfig{
			ConfigDir:      "configs",
			EnabledSources: []string{"openai", "self_deployed"},
		},
		Dashboard: DashboardConfig{
			Enabled:         true,
			RefreshInterval: 2,
			WebPort:         80,
		},
		Log: LogConfig{
			Level:      "info",
			Dir:        "logs",
			MaxSizeMB:  100,
			MaxBackups: 10,
			MaxAgeDays: 7,
		},
	}
}

// Load reads a YAML config file and merges it with defaults.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}

	return cfg, nil
}
