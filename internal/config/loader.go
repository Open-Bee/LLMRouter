package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"llm-router/internal/model"

	"gopkg.in/yaml.v3"
)

// sourceTypeFileMap maps source_type names to their expected config file prefixes.
var sourceTypeFileMap = map[string]string{
	"openai":        "openai",
	"self_deployed": "self_deployed",
}

// LoadBackendConfigs loads all backend configuration files from the given directory,
// filtered by the enabled source types.
func LoadBackendConfigs(configDir string, enabledSources []string) (map[string]*model.BackendConfig, error) {
	result := make(map[string]*model.BackendConfig)

	entries, err := os.ReadDir(configDir)
	if err != nil {
		return nil, fmt.Errorf("read config dir %s: %w", configDir, err)
	}

	enabledSet := make(map[string]bool)
	for _, s := range enabledSources {
		enabledSet[s] = true
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}

		// Determine source type from filename
		sourceType := detectSourceType(name, enabledSet)
		if sourceType == "" {
			continue
		}

		filePath := filepath.Join(configDir, name)
		cfg, err := LoadBackendConfigFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("load backend config %s: %w", filePath, err)
		}

		// Merge into existing config for this source type, or create new
		if existing, ok := result[filePath]; ok {
			existing.ModelList = append(existing.ModelList, cfg.ModelList...)
		} else {
			result[filePath] = cfg
		}
	}

	return result, nil
}

// detectSourceType determines the source type from the config filename.
func detectSourceType(filename string, enabledSet map[string]bool) string {
	lower := strings.ToLower(filename)
	for sourceType, prefix := range sourceTypeFileMap {
		if !enabledSet[sourceType] {
			continue
		}
		if strings.HasPrefix(lower, prefix) {
			return sourceType
		}
	}
	return ""
}

// LoadBackendConfigFile loads a single backend configuration YAML file.
func LoadBackendConfigFile(path string) (*model.BackendConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", path, err)
	}

	var cfg model.BackendConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse YAML %s: %w", path, err)
	}

	return &cfg, nil
}

// WriteBackendConfigFile writes a backend configuration to a YAML file.
func WriteBackendConfigFile(path string, cfg *model.BackendConfig) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}

	return nil
}
