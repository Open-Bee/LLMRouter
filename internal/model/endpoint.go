package model

// ServiceEndpoint represents the configuration of a backend endpoint loaded from YAML.
// It is a pure configuration data structure with no runtime state.
type ServiceEndpoint struct {
	ModelName      string   `yaml:"model_name"`
	LiteLLMParams  LiteLLMParams `yaml:"litellm_params"`
}

// LiteLLMParams holds the backend connection parameters from the YAML config.
type LiteLLMParams struct {
	Model          string   `yaml:"model"`
	APIBase        string   `yaml:"api_base"`
	APIKey         string   `yaml:"api_key"`
	SupportsVision bool     `yaml:"supports_vision"`
	Weight         float64  `yaml:"weight"`
	SourceType     string   `yaml:"source_type"`
	RPMLimit       *int     `yaml:"rpm_limit"`
}

// BackendConfig represents the top-level structure of a backend configuration YAML file.
type BackendConfig struct {
	ModelList []ServiceEndpoint `yaml:"model_list"`
}
