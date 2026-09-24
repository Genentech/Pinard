package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// PressoirConfig selects the pressoir provider and its connection parameters for a
// vignoble or a single repo. Provider defaults to "gitlab" when empty.
type PressoirConfig struct {
	ProviderName string `yaml:"provider,omitempty"`  // "gitlab" | "github"; default "gitlab"
	Host         string `yaml:"host,omitempty"`      // override the default host for this provider
	Org          string `yaml:"org,omitempty"`       // GitHub org / GitLab group
	TokenEnv     string `yaml:"token_env,omitempty"` // env var holding the PAT/token
}

// Provider returns the normalised provider name, defaulting to "gitlab".
func (f PressoirConfig) Provider() string {
	if f.ProviderName != "" {
		return f.ProviderName
	}
	return "gitlab"
}

type TestsConfig struct {
	Strategy string `yaml:"strategy"` // local | k3d | none
	Command  string `yaml:"command"`  // optional override
}

type Vigne struct {
	Path             string      `yaml:"path"`
	Repo             string      `yaml:"repo"`
	DefaultBranch    string      `yaml:"default_branch,omitempty"`
	Model            ModelConfig `yaml:"model,omitempty"`
	Process          string      `yaml:"process,omitempty"`
	AutoMerge        *bool       `yaml:"auto_merge,omitempty"`
	AutoReview       *bool       `yaml:"auto_review,omitempty"`
	MonitorPostMerge *bool       `yaml:"monitor_post_merge,omitempty"`
	Tests            TestsConfig `yaml:"tests,omitempty"`
	// Runtime controls how workers for this vigne are launched:
	//   "" / "local"      — bare `pinard` on the daemon host (default)
	//   "singularity"     — wrap the worker in `singularity run --containall <binds> <sif>`
	Runtime  string   `yaml:"runtime,omitempty"`
	Sif      string   `yaml:"sif,omitempty"`        // path to the .sif image (runtime=singularity)
	Binds    []string `yaml:"binds,omitempty"`      // singularity --bind entries (host[:container[:ro]])
	NoWorktree bool           `yaml:"no_worktree,omitempty"` // run in the project path, skip git worktree (data jobs)
	Pressoir   PressoirConfig `yaml:"pressoir,omitempty"`    // per-repo pressoir override
}

func (v *Vigne) TargetBranch() string {
	if v.DefaultBranch != "" {
		return v.DefaultBranch
	}
	return "main"
}

func (v *Vigne) ExpandedPath() string {
	p := v.Path
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, p[2:])
	}
	return p
}

func (v *Vigne) ShouldAutoMerge(global bool) bool {
	if v.AutoMerge != nil {
		return *v.AutoMerge
	}
	return global
}

// ShouldAutoReview reports whether MRs for this vigne should trigger an automated
// review by the owning maître. Default is true (opt-out). Pass the vignoble-level
// AutoReview pointer; nil vignoble default means true.
func (v *Vigne) ShouldAutoReview(vignobleAutoReview *bool) bool {
	if v.AutoReview != nil {
		return *v.AutoReview
	}
	if vignobleAutoReview != nil {
		return *vignobleAutoReview
	}
	return true // default: auto-review on
}

func (v *Vigne) WorkerModel(vignobleDefault string) string {
	if v.Model.Tier != "" {
		return v.Model.Tier
	}
	if v.Model.ID != "" {
		return v.Model.ID
	}
	if vignobleDefault != "" {
		return vignobleDefault
	}
	return "sonnet"
}

func (v *Vigne) ShouldMonitorPostMerge() bool {
	if v.MonitorPostMerge != nil {
		return *v.MonitorPostMerge
	}
	return true // default: enabled
}

type ModelConfig struct {
	ID   string `yaml:"id"`
	Tier string `yaml:"tier"`
}

type ModelsConfig struct {
	Provider  string      `yaml:"provider,omitempty"` // LLM provider name: "proxy" (default) | "openai" | "deepseek" | …
	API       string      `yaml:"api,omitempty"`      // pi provider API type: "anthropic-messages" (default) | "openai-responses" | …
	Conductor ModelConfig `yaml:"conductor,omitempty"`
	Worker    ModelConfig `yaml:"worker,omitempty"`
}

// ProviderName returns the normalised LLM provider name, defaulting to "proxy".
func (m ModelsConfig) ProviderName() string {
	if m.Provider != "" {
		return m.Provider
	}
	return "proxy"
}

// APIType returns the pi provider API type, defaulting to "anthropic-messages" for the
// proxy provider and "openai-responses" for openai/deepseek.
func (m ModelsConfig) APIType() string {
	if m.API != "" {
		return m.API
	}
	switch m.ProviderName() {
	case "openai", "deepseek":
		return "openai-responses"
	default:
		return "anthropic-messages"
	}
}

type VignobleConfig struct {
	GitLabHost  string           `yaml:"gitlab_host"`
	GitLabGroup string           `yaml:"gitlab_group"`
	AutoMerge   bool             `yaml:"auto_merge"`
	AutoReview  *bool            `yaml:"auto_review,omitempty"` // nil = default true
	Models      ModelsConfig     `yaml:"models,omitempty"`
	Pressoir    PressoirConfig   `yaml:"pressoir,omitempty"` // vignoble-level pressoir default
	Vignes      map[string]Vigne `yaml:"vignes"`
}

func LoadVignoble(configPath string) (*VignobleConfig, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var cfg VignobleConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

type Vignoble struct {
	Path       string
	Name       string
	ConfigPath string
	StateDir   string
	LogDir     string
	Config     *VignobleConfig
}

// ResolvePressoirConfig returns the effective PressoirConfig for a repo,
// applying precedence: per-repo Vigne.Pressoir > vignoble Pressoir > default ("gitlab").
// repo may be either a full repo path (e.g. "sirloon/exohub-test") or a vigne map key
// (e.g. "exohub-test"). Repo-path matching is tried first so that the common case —
// callers passing entry.Repo or --repo <path> — works correctly.
// The returned PressoirConfig always has a non-empty Provider().
func (v *Vignoble) ResolvePressoirConfig(repo string) PressoirConfig {
	// Primary: match by Repo field (full path like "owner/name").
	for _, vigne := range v.Config.Vignes {
		if vigne.Repo == repo && vigne.Pressoir.ProviderName != "" {
			return vigne.Pressoir
		}
	}
	// Secondary: match by map key (short vigne name like "exohub-test").
	if vigne, ok := v.Config.Vignes[repo]; ok {
		if vigne.Pressoir.ProviderName != "" {
			return vigne.Pressoir
		}
	}
	if v.Config.Pressoir.ProviderName != "" {
		return v.Config.Pressoir
	}
	// Default: inherit GitLab host/group from top-level fields.
	return PressoirConfig{
		ProviderName: "gitlab",
		Host:         v.Config.GitLabHost,
		Org:          v.Config.GitLabGroup,
	}
}

func ResolveVignoble() (*Vignoble, error) {
	configPath := os.Getenv("AOC_CONFIG")
	if configPath == "" {
		cwd, _ := os.Getwd()
		candidate := filepath.Join(cwd, "vignes.yaml")
		if _, err := os.Stat(candidate); err == nil {
			configPath = candidate
		}
	}
	if configPath == "" {
		return nil, fmt.Errorf("no vignoble found — run from a vignoble directory or set AOC_CONFIG")
	}

	dir := filepath.Dir(configPath)
	name := filepath.Base(dir)
	name = strings.TrimPrefix(name, "vignoble-")

	cfg, err := LoadVignoble(configPath)
	if err != nil {
		return nil, err
	}

	return &Vignoble{
		Path:       dir,
		Name:       name,
		ConfigPath: configPath,
		StateDir:   filepath.Join(dir, ".state"),
		LogDir:     filepath.Join(dir, "logs"),
		Config:     cfg,
	}, nil
}
