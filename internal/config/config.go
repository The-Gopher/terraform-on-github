package config

import (
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version        int               `yaml:"version"`
	Defaults       Defaults          `yaml:"defaults"`
	ModuleSources   []string          `yaml:"module_sources"`
	Workspaces     []Workspace       `yaml:"workspaces"`
}

type Defaults struct {
	TerraformVersion string        `yaml:"terraform_version"`
	PlanTimeout      string        `yaml:"plan_timeout"`
	ApplyTimeout     string        `yaml:"apply_timeout"`
	SummaryDetail    string        `yaml:"summary_detail"`
	VarFiles         []string      `yaml:"var_files"`
	PlanArgs         []string      `yaml:"plan_args"`
}

type Workspace struct {
	Name               string            `yaml:"name"`
	Branch             string            `yaml:"branch"`
	Dir                string            `yaml:"dir"`
	Watch              []string          `yaml:"watch"`
	TerraformWorkspace string            `yaml:"terraform_workspace"`
	Backend            Backend           `yaml:"backend"`
	Impersonate        Impersonate       `yaml:"impersonate"`
	Apply              *ApplySettings    `yaml:"apply"`
	// Overrides
	TerraformVersion   string            `yaml:"terraform_version"`
	PlanTimeout        string            `yaml:"plan_timeout"`
	ApplyTimeout       string            `yaml:"apply_timeout"`
	SummaryDetail      string            `yaml:"summary_detail"`
	VarFiles           []string          `yaml:"var_files"`
	PlanArgs          []string          `yaml:"plan_args"`
}

type Backend struct {
	Bucket string `yaml:"bucket"`
	Prefix string `yaml:"prefix"`
}

type Impersonate struct {
	Plan  string `yaml:"plan"`
	Apply string `yaml:"apply"`
}

type ApplySettings struct {
	Enabled               bool   `yaml:"enabled"`
	Environment           string `yaml:"environment"`
	ApprovalTimeout       string `yaml:"approval_timeout"`
	OnStale               string `yaml:"on_stale"`
	RequireProtectedBase bool `yaml:"require_protected_base"`
}

var nameRegex = regexp.MustCompile(`^[a-z0-9-]{1,48}$`)
var emailRegex = regexp.MustCompile(`^[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}$`)

func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("version must be 1, got %d", c.Version)
	}

	if len(c.Workspaces) == 0 {
		return fmt.Errorf("at least one workspace must be defined")
	}

	names := make(map[string]bool)
	for i := range c.Workspaces {
		w := &c.Workspaces[i]
		if err := w.Validate(c.Defaults); err != nil {
			return fmt.Errorf("workspace %s: %w", w.Name, err)
		}
		if names[w.Name] {
			return fmt.Errorf("duplicate workspace name: %s", w.Name)
		}
		names[w.Name] = true
	}

	return nil
}

func (w *Workspace) Validate(def Defaults) error {
	if !nameRegex.MatchString(w.Name) {
		return fmt.Errorf("invalid name: %s (must match [a-z0-9-]{1,48})", w.Name)
	}
	if w.Branch == "" {
		return fmt.Errorf("branch is required")
	}
	if w.Dir == "" {
		return fmt.Errorf("dir is required")
	}
	if w.Dir == "/" || w.Dir[0] == '/' {
		return fmt.Errorf("dir must not be absolute: %s", w.Dir)
	}
	if regexp.MustCompile(`\.\.`).MatchString(w.Dir) {
		return fmt.Errorf("dir must not contain '..': %s", w.Dir)
	}
	
	if w.Backend.Bucket == "" || w.Backend.Prefix == "" {
		return fmt.Errorf("backend bucket and prefix are required")
	}
	if w.Impersonate.Plan == "" || w.Impersonate.Apply == "" {
		return fmt.Errorf("impersonate plan and apply SAs are required")
	}
	if !emailRegex.MatchString(w.Impersonate.Plan) || !emailRegex.MatchString(w.Impersonate.Apply) {
		return fmt.Errorf("impersonate SAs must be valid email addresses")
	}

	if w.TerraformVersion == "" && def.TerraformVersion == "" {
		return fmt.Errorf("terraform_version is required (either in defaults or workspace)")
	}
	
	// Check if version is exact (no ~> or ^)
	version := w.TerraformVersion
	if version == "" {
		version = def.TerraformVersion
	}
	if containsVersionRange(version) {
		return fmt.Errorf("terraform_version must be exact, got %s", version)
	}

	if w.Apply != nil && w.Apply.OnStale == "replan_if_equivalent" {
		return fmt.Errorf("on_stale: replan_if_equivalent is rejected; please refer to DESIGN §6.3")
	}

	return nil
}

func containsVersionRange(v string) bool {
	// Simple check for common range symbols
	for _, char := range v {
		if char == '~' || char == '^' || char == '*' || char == '>' || char == '<' {
			return true
		}
	}
	return false
}
