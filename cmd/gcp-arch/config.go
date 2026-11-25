package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/arrase/gcp-arch/internal/synth"
)

// stringSlice accepts either a single YAML scalar or a YAML sequence of strings.
type stringSlice []string

func (s *stringSlice) UnmarshalYAML(value *yaml.Node) error {
	if value.Tag == "!!null" {
		*s = nil
		return nil
	}
	if value.Kind == yaml.ScalarNode {
		var single string
		if err := value.Decode(&single); err != nil {
			return err
		}
		*s = []string{single}
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*s = list
	return nil
}

// appConfig holds optional user defaults loaded from ~/.config/gcp-arch/config.yaml
// (or $XDG_CONFIG_HOME/gcp-arch/config.yaml). Precedence: CLI > env > config.yaml > built-in defaults.
type appConfig struct {
	Org            stringSlice       `yaml:"org"`
	Folder         stringSlice       `yaml:"folder"`
	Projects       stringSlice       `yaml:"projects"`
	ProjectAliases map[string]string `yaml:"project_aliases"`
	QuotaProject   string            `yaml:"quota_project"`
	VertexProject  string            `yaml:"vertex_project"`
	VertexLocation string            `yaml:"vertex_location"`
	Model          string            `yaml:"model"`
	ThinkingLevel  string            `yaml:"thinking_level"`
	Concurrency    *int              `yaml:"concurrency"`
	CloudBuildRuns *int              `yaml:"cloudbuild_runs"`
	ChromePath     string            `yaml:"chrome_path"`
	Format         stringSlice       `yaml:"format"`
}

func defaultConfigPath() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "gcp-arch", "config.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gcp-arch", "config.yaml"), nil
}

func loadDefaultConfig() (appConfig, error) {
	path, err := defaultConfigPath()
	if err != nil {
		return appConfig{}, runtimeErr(fmt.Errorf("resolve config directory: %w", err))
	}
	return loadConfigFile(path)
}

func loadConfigFile(path string) (appConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return appConfig{}, nil
		}
		return appConfig{}, runtimeErr(fmt.Errorf("read config %q: %w", path, err))
	}

	var cfg appConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return appConfig{}, nil
		}
		return appConfig{}, usageErr(fmt.Errorf("decode config %q: %w", path, err))
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return appConfig{}, usageErr(fmt.Errorf("decode config %q: unexpected trailing YAML document", path))
	}

	if err := validateConfig(&cfg); err != nil {
		return appConfig{}, usageErr(fmt.Errorf("invalid config %q: %w", path, err))
	}
	return cfg, nil
}

func validateConfig(cfg *appConfig) error {
	scopes := scopeFlags{
		orgs:     cfg.Org,
		folders:  cfg.Folder,
		projects: cfg.Projects,
	}
	if _, err := scopes.parse(); err != nil {
		return err
	}

	if len(cfg.ProjectAliases) > 0 {
		cleaned := make(map[string]string, len(cfg.ProjectAliases))
		for rawNum, rawID := range cfg.ProjectAliases {
			num := strings.TrimSpace(rawNum)
			id := strings.TrimSpace(rawID)
			if !isAllDigits(num) || !isValidProjectID(id) {
				return fmt.Errorf("invalid project_aliases entry %q=%q (expected <NUMBER>: <ID>)", rawNum, rawID)
			}
			if prev, exists := cleaned[num]; exists && prev != id {
				return fmt.Errorf("conflicting project_aliases for project number %s (%q vs %q)", num, prev, id)
			}
			cleaned[num] = id
		}
		cfg.ProjectAliases = cleaned
	}

	cfg.QuotaProject = strings.TrimSpace(cfg.QuotaProject)
	cfg.VertexProject = strings.TrimSpace(cfg.VertexProject)
	cfg.VertexLocation = strings.TrimSpace(cfg.VertexLocation)
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.ThinkingLevel = strings.TrimSpace(cfg.ThinkingLevel)
	cfg.ChromePath = strings.TrimSpace(cfg.ChromePath)

	if _, err := synth.ParseThinkingLevel(cfg.ThinkingLevel); err != nil {
		return err
	}
	if cfg.Concurrency != nil && *cfg.Concurrency < 1 {
		return fmt.Errorf("concurrency must be >= 1")
	}
	if cfg.CloudBuildRuns != nil && *cfg.CloudBuildRuns < 1 {
		return fmt.Errorf("cloudbuild_runs must be >= 1")
	}
	if cfg.Format != nil {
		if _, _, _, _, err := parseRenderFormats(strings.Join(cfg.Format, ",")); err != nil {
			return err
		}
	}
	return nil
}
