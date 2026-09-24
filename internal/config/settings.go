package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// claudeSettings mirrors the parts of ~/.claude/settings.json we read.
// Pi reads the same file; the env map holds ANTHROPIC_DEFAULT_*_MODEL among others.
type claudeSettings struct {
	Env map[string]string `json:"env"`
}

func loadClaudeSettings() claudeSettings {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".claude", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return claudeSettings{}
	}
	var s claudeSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return claudeSettings{}
	}
	return s
}

// StripThinkingSuffix removes a trailing "[Nm]" thinking marker (e.g.
// "claude-sonnet-4-6[1m]" -> "claude-sonnet-4-6"). The proxy provider configures
// thinking separately, so model IDs must not carry the suffix.
func StripThinkingSuffix(id string) string {
	if i := strings.IndexByte(id, '['); i >= 0 {
		return id[:i]
	}
	return id
}

// ResolveModelTier maps a tier name (sonnet|opus|haiku) to the concrete model ID
// from ~/.claude/settings.json env.ANTHROPIC_DEFAULT_<TIER>_MODEL. If the input is
// not a known tier it is returned unchanged (already a model ID). Returns "" if a
// tier has no configured model.
func ResolveModelTier(tier string) string {
	switch strings.ToLower(tier) {
	case "sonnet", "opus", "haiku":
		key := "ANTHROPIC_DEFAULT_" + strings.ToUpper(tier) + "_MODEL"
		s := loadClaudeSettings()
		return StripThinkingSuffix(s.Env[key])
	default:
		// Not a tier — assume it's already a model ID.
		return StripThinkingSuffix(tier)
	}
}

// SettingsModels returns the resolved opus/sonnet/haiku model IDs (empty string for
// any tier without a configured model), for building the conductor's --models list.
func SettingsModels() (opus, sonnet, haiku string) {
	s := loadClaudeSettings()
	return StripThinkingSuffix(s.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"]),
		StripThinkingSuffix(s.Env["ANTHROPIC_DEFAULT_SONNET_MODEL"]),
		StripThinkingSuffix(s.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"])
}
