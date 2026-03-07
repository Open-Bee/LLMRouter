package config

import (
	"os"
	"path/filepath"
	"testing"

	"llm-router/internal/model"
)

func TestLoadBackendConfigFile(t *testing.T) {
	content := `
model_list:
- model_name: TestModel
  litellm_params:
    model: openai/TestModel
    api_base: http://10.0.0.1:8000/v1
    api_key: dummy
    supports_vision: true
    weight: 1.0
    source_type: openai
    rpm_limit: null
- model_name: TestModel
  litellm_params:
    model: openai/TestModel
    api_base: http://10.0.0.2:8000/v1
    api_key: key2
    supports_vision: false
    weight: 2.0
    source_type: openai
    rpm_limit: 100
`
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadBackendConfigFile(path)
	if err != nil {
		t.Fatalf("LoadBackendConfigFile failed: %v", err)
	}

	if len(cfg.ModelList) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(cfg.ModelList))
	}

	ep1 := cfg.ModelList[0]
	if ep1.ModelName != "TestModel" {
		t.Errorf("unexpected model name: %s", ep1.ModelName)
	}
	if ep1.LiteLLMParams.APIBase != "http://10.0.0.1:8000/v1" {
		t.Errorf("unexpected api_base: %s", ep1.LiteLLMParams.APIBase)
	}
	if ep1.LiteLLMParams.RPMLimit != nil {
		t.Errorf("expected nil rpm_limit for ep1")
	}

	ep2 := cfg.ModelList[1]
	if ep2.LiteLLMParams.RPMLimit == nil || *ep2.LiteLLMParams.RPMLimit != 100 {
		t.Errorf("expected rpm_limit=100 for ep2")
	}
	if ep2.LiteLLMParams.Weight != 2.0 {
		t.Errorf("unexpected weight: %f", ep2.LiteLLMParams.Weight)
	}
}

func TestLoadBackendConfigs_FilterBySource(t *testing.T) {
	dir := t.TempDir()

	// Create openai config
	openaiContent := `
model_list:
- model_name: M1
  litellm_params:
    api_base: http://localhost/v1
    source_type: openai
- model_name: M2
  litellm_params:
    api_base: http://localhost:2/v1
    source_type: openai
`
	if err := os.WriteFile(filepath.Join(dir, "openai_config.yaml"), []byte(openaiContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create self_deployed config
	selfDeployedContent := `
model_list:
- model_name: M3
  litellm_params:
    api_base: http://localhost:8000/v1
    source_type: self_deployed
`
	if err := os.WriteFile(filepath.Join(dir, "self_deployed_config.yaml"), []byte(selfDeployedContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Enable both openai and self_deployed
	configs, err := LoadBackendConfigs(dir, []string{"openai", "self_deployed"})
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	totalEndpoints := 0
	for _, cfg := range configs {
		totalEndpoints += len(cfg.ModelList)
	}
	if totalEndpoints != 3 {
		t.Errorf("expected 3 endpoints (openai+self_deployed), got %d", totalEndpoints)
	}

	// Enable only openai
	configs, err = LoadBackendConfigs(dir, []string{"openai"})
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	totalEndpoints = 0
	for _, cfg := range configs {
		totalEndpoints += len(cfg.ModelList)
	}
	if totalEndpoints != 2 {
		t.Errorf("expected 2 endpoints (openai only), got %d", totalEndpoints)
	}

	// Enable only self_deployed
	configs, err = LoadBackendConfigs(dir, []string{"self_deployed"})
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	totalEndpoints = 0
	for _, cfg := range configs {
		totalEndpoints += len(cfg.ModelList)
	}
	if totalEndpoints != 1 {
		t.Errorf("expected 1 endpoint (self_deployed only), got %d", totalEndpoints)
	}

	// Empty enabled sources should load nothing
	configs, err = LoadBackendConfigs(dir, []string{})
	if err != nil {
		t.Fatalf("failed: %v", err)
	}
	totalEndpoints = 0
	for _, cfg := range configs {
		totalEndpoints += len(cfg.ModelList)
	}
	if totalEndpoints != 0 {
		t.Errorf("expected 0 endpoints with empty sources, got %d", totalEndpoints)
	}
}

func TestWriteBackendConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.yaml")

	cfg := &model.BackendConfig{
		ModelList: []model.ServiceEndpoint{
			{
				ModelName: "WriteTest",
				LiteLLMParams: model.LiteLLMParams{
					APIBase:    "http://localhost/v1",
					SourceType: "openai",
				},
			},
		},
	}
	err := WriteBackendConfigFile(path, cfg)
	if err != nil {
		t.Fatalf("WriteBackendConfigFile failed: %v", err)
	}

	// Read it back
	loaded, err := LoadBackendConfigFile(path)
	if err != nil {
		t.Fatalf("re-read failed: %v", err)
	}
	if len(loaded.ModelList) != 1 {
		t.Errorf("expected 1 entry, got %d", len(loaded.ModelList))
	}
	if loaded.ModelList[0].ModelName != "WriteTest" {
		t.Errorf("unexpected model name: %s", loaded.ModelList[0].ModelName)
	}
}
