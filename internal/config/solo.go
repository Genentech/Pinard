package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
)

// SoloConfig holds port overrides for solo-mode (local) operation.
// Defaults match the pinard-solo image port layout. On macOS, values are
// read from ~/Library/Application Support/Pinard/config.json when present.
type SoloConfig struct {
	NATSPort    int // default 4222
	EngramPort  int // default 7437
	SurrealPort int // default 8000
	WebtermPort int // default 8080
}

// soloConfigJSON is the shape of ~/Library/Application Support/Pinard/config.json
// written by the native macOS ServiceOrchestrator app.
type soloConfigJSON struct {
	NATSPort    int `json:"nats_port"`
	EngramPort  int `json:"engram_port"`
	SurrealPort int `json:"surreal_port"`
	WebtermPort int `json:"webterm_port"`
}

// ReadSoloConfig returns the solo port configuration. On macOS it attempts to
// read ~/Library/Application Support/Pinard/config.json; on all platforms
// (including when the file is absent or unparseable) it falls back to defaults.
// Never returns an error — always returns a usable config.
func ReadSoloConfig() SoloConfig {
	cfg := SoloConfig{
		NATSPort:    4222,
		EngramPort:  7437,
		SurrealPort: 8000,
		WebtermPort: 8080,
	}

	if runtime.GOOS != "darwin" {
		return cfg
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return cfg
	}

	path := filepath.Join(home, "Library", "Application Support", "Pinard", "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg // file absent — use defaults
	}

	var j soloConfigJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return cfg // unparseable — use defaults
	}

	if j.NATSPort > 0 {
		cfg.NATSPort = j.NATSPort
	}
	if j.EngramPort > 0 {
		cfg.EngramPort = j.EngramPort
	}
	if j.SurrealPort > 0 {
		cfg.SurrealPort = j.SurrealPort
	}
	if j.WebtermPort > 0 {
		cfg.WebtermPort = j.WebtermPort
	}
	return cfg
}
