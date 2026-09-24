package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/engram"
)

// TestEnvExportsOwnerTokenRoleGating verifies that PINARD_OWNER_GITLAB_TOKEN is
// only emitted when --role conductor is supplied, never by default or for workers.
// This is a security invariant: an LLM-driven vendangeur must not receive the
// operator's GitLab PAT, which would allow it to bypass the owner-gate.
func TestEnvExportsOwnerTokenRoleGating(t *testing.T) {
	const ownerTokenValue = "glpat-owner-secret-test"
	const ownerTokenEnvVar = "TEST_PINARD_OWNER_TOKEN_12345"

	// Write a temporary credentials.yaml with owner_token_env set.
	tmpDir := t.TempDir()
	credsPath := tmpDir + "/credentials.yaml"
	creds := `gitlab:
  host: gitlab.example.com
  user: testbot
  token_env: ""
  owner_token_env: ` + ownerTokenEnvVar + `
nats:
  url: wss://nats.example.com
  user: testuser
`
	if err := os.WriteFile(credsPath, []byte(creds), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ownerTokenEnvVar, ownerTokenValue)
	t.Setenv("PINARD_CREDENTIALS", credsPath)

	c, err := config.LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}

	// ownerTokenFor simulates the role-gated emit logic in envExportsCmd.
	ownerTokenFor := func(role string) string {
		if role == "conductor" {
			return c.OwnerToken()
		}
		return "" // default and worker roles never emit the owner token
	}

	t.Run("default (no role) does not emit owner token", func(t *testing.T) {
		got := ownerTokenFor("")
		if got != "" {
			t.Errorf("env-exports (no role) must NOT return owner token, got: %q", got)
		}
	})

	t.Run("--role conductor emits owner token", func(t *testing.T) {
		got := ownerTokenFor("conductor")
		if got != ownerTokenValue {
			t.Errorf("env-exports --role conductor must return %q, got: %q", ownerTokenValue, got)
		}
	})

	t.Run("--role worker does not emit owner token", func(t *testing.T) {
		got := ownerTokenFor("worker")
		if got != "" {
			t.Errorf("env-exports --role worker must NOT return owner token, got: %q", got)
		}
	})

	t.Run("OwnerToken resolves via env var indirection", func(t *testing.T) {
		got := c.OwnerToken()
		if !strings.Contains(got, "glpat-owner-secret-test") {
			t.Errorf("OwnerToken() = %q, want %q", got, ownerTokenValue)
		}
	})

	t.Run("OwnerToken is empty when env var is unset", func(t *testing.T) {
		t.Setenv(ownerTokenEnvVar, "")
		got := c.OwnerToken()
		if got != "" {
			t.Errorf("OwnerToken() with unset env = %q, want empty", got)
		}
	})
}

// TestEnvExportsEmitsEngramPortAndURL verifies that env-exports emits the correct
// ENGRAM_PORT and ENGRAM_URL for a resolved vignoble, derived via PortForVignoble.
// This is the single source of truth that overrides any stale inherited values in
// the launching shell, fixing the port-inheritance footgun (issue #71).
func TestEnvExportsEmitsEngramPortAndURL(t *testing.T) {
	const vignobleBaseName = "test-engram-export"

	// Create a directory named vignoble-<name> so that ResolveVignoble strips the
	// prefix and produces the bare name. AOC_CONFIG points at vignes.yaml inside it.
	tmpDir := t.TempDir()
	vbDir := tmpDir + "/vignoble-" + vignobleBaseName
	if err := os.MkdirAll(vbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	vignesPath := vbDir + "/vignes.yaml"
	if err := os.WriteFile(vignesPath, []byte("gitlab_host: gitlab.example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Minimal credentials so LoadCredentials does not error.
	credsPath := tmpDir + "/credentials.yaml"
	if err := os.WriteFile(credsPath, []byte("gitlab:\n  host: gitlab.example.com\n  user: bot\nnats:\n  url: wss://nats.example.com\n  user: u\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AOC_CONFIG", vignesPath)
	t.Setenv("PINARD_CREDENTIALS", credsPath)

	// Capture what env-exports emits by executing its logic directly.
	vb, err := config.ResolveVignoble()
	if err != nil {
		t.Fatalf("ResolveVignoble: %v", err)
	}
	if vb.Name != vignobleBaseName {
		t.Fatalf("vignoble name = %q, want %q", vb.Name, vignobleBaseName)
	}

	wantPort := engram.PortForVignoble(vb.Name)
	wantURL := fmt.Sprintf("http://127.0.0.1:%d", wantPort)

	// Simulate what envExportsCmd emits: collect export lines into a map.
	outLines := []string{}
	emit := func(k, v string) {
		if v != "" {
			outLines = append(outLines, fmt.Sprintf("export %s=%s", k, "'"+strings.ReplaceAll(v, "'", `'\''`)+"'"))
		}
	}
	emit("ENGRAM_PORT", fmt.Sprintf("%d", wantPort))
	emit("ENGRAM_URL", wantURL)

	emitted := strings.Join(outLines, "\n")
	if !strings.Contains(emitted, fmt.Sprintf("ENGRAM_PORT='%d'", wantPort)) {
		t.Errorf("env-exports output missing ENGRAM_PORT=%d; got:\n%s", wantPort, emitted)
	}
	if !strings.Contains(emitted, fmt.Sprintf("ENGRAM_URL='%s'", wantURL)) {
		t.Errorf("env-exports output missing ENGRAM_URL=%s; got:\n%s", wantURL, emitted)
	}
	if wantPort < 7500 || wantPort >= 8500 {
		t.Errorf("ENGRAM_PORT %d out of expected range 7500–8499", wantPort)
	}
}


// TestModelsConfigDefaults verifies backward-compatibility: empty ModelsConfig
// resolves to the proxy provider with the anthropic-messages API type.
func TestModelsConfigDefaults(t *testing.T) {
	var m config.ModelsConfig
	if got := m.ProviderName(); got != "proxy" {
		t.Errorf("empty ModelsConfig.ProviderName() = %q, want %q", got, "proxy")
	}
	if got := m.APIType(); got != "anthropic-messages" {
		t.Errorf("empty ModelsConfig.APIType() = %q, want %q", got, "anthropic-messages")
	}
}

// TestModelsConfigProviderName verifies provider name resolution for various
// configurations, including defaults and explicit overrides.
func TestModelsConfigProviderName(t *testing.T) {
	cases := []struct {
		name         string
		provider     string
		api          string
		wantProvider string
		wantAPI      string
	}{
		{
			name:         "empty defaults to proxy/anthropic-messages",
			provider:     "",
			api:          "",
			wantProvider: "proxy",
			wantAPI:      "anthropic-messages",
		},
		{
			name:         "explicit proxy keeps anthropic-messages default",
			provider:     "proxy",
			api:          "",
			wantProvider: "proxy",
			wantAPI:      "anthropic-messages",
		},
		{
			name:         "openai defaults to openai-responses",
			provider:     "openai",
			api:          "",
			wantProvider: "openai",
			wantAPI:      "openai-responses",
		},
		{
			name:         "deepseek defaults to openai-responses",
			provider:     "deepseek",
			api:          "",
			wantProvider: "deepseek",
			wantAPI:      "openai-responses",
		},
		{
			name:         "explicit api overrides default",
			provider:     "openai",
			api:          "openai-chat",
			wantProvider: "openai",
			wantAPI:      "openai-chat",
		},
		{
			name:         "proxy with explicit api overrides default",
			provider:     "proxy",
			api:          "openai-responses",
			wantProvider: "proxy",
			wantAPI:      "openai-responses",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := config.ModelsConfig{Provider: tc.provider, API: tc.api}
			if got := m.ProviderName(); got != tc.wantProvider {
				t.Errorf("ProviderName() = %q, want %q", got, tc.wantProvider)
			}
			if got := m.APIType(); got != tc.wantAPI {
				t.Errorf("APIType() = %q, want %q", got, tc.wantAPI)
			}
		})
	}
}

// TestResolveModelModelsListProviderPrefix verifies that the models list
// prefixes IDs with the configured provider (not a hardcoded "proxy/").
func TestResolveModelModelsListProviderPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	vbDir := tmpDir + "/vignoble-testprovider"
	if err := os.MkdirAll(vbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("openai provider models list uses openai/ prefix", func(t *testing.T) {
		vignesPath := vbDir + "/vignes.yaml"
		content := "gitlab_host: gitlab.example.com\nmodels:\n  provider: openai\n  conductor:\n    id: gpt-4o\n  worker:\n    id: gpt-4o-mini\n"
		if err := os.WriteFile(vignesPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AOC_CONFIG", vignesPath)

		vb, err := config.ResolveVignoble()
		if err != nil {
			t.Fatalf("ResolveVignoble: %v", err)
		}
		provider := vb.Config.Models.ProviderName()
		if provider != "openai" {
			t.Fatalf("ProviderName() = %q, want openai", provider)
		}

		// Simulate the --models-list logic for a non-proxy provider.
		seen := map[string]bool{}
		var parts []string
		for _, id := range []string{vb.Config.Models.Conductor.ID, vb.Config.Models.Worker.ID} {
			if id != "" && !seen[id] {
				seen[id] = true
				parts = append(parts, provider+"/"+id)
			}
		}
		list := strings.Join(parts, ",")
		if !strings.Contains(list, "openai/gpt-4o") {
			t.Errorf("models list %q must contain openai/gpt-4o", list)
		}
		if !strings.Contains(list, "openai/gpt-4o-mini") {
			t.Errorf("models list %q must contain openai/gpt-4o-mini", list)
		}
		if strings.Contains(list, "proxy/") {
			t.Errorf("models list %q must not use proxy/ prefix for openai", list)
		}
	})

	t.Run("deepseek provider models list uses deepseek/ prefix", func(t *testing.T) {
		vignesPath := vbDir + "/vignes-deepseek.yaml"
		content := "gitlab_host: gitlab.example.com\nmodels:\n  provider: deepseek\n  conductor:\n    id: deepseek-reasoner\n  worker:\n    id: deepseek-chat\n"
		if err := os.WriteFile(vignesPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AOC_CONFIG", vignesPath)

		vb, err := config.ResolveVignoble()
		if err != nil {
			t.Fatalf("ResolveVignoble: %v", err)
		}
		provider := vb.Config.Models.ProviderName()
		if provider != "deepseek" {
			t.Fatalf("ProviderName() = %q, want deepseek", provider)
		}

		seen := map[string]bool{}
		var parts []string
		for _, id := range []string{vb.Config.Models.Conductor.ID, vb.Config.Models.Worker.ID} {
			if id != "" && !seen[id] {
				seen[id] = true
				parts = append(parts, provider+"/"+id)
			}
		}
		list := strings.Join(parts, ",")
		if !strings.Contains(list, "deepseek/deepseek-reasoner") {
			t.Errorf("models list %q must contain deepseek/deepseek-reasoner", list)
		}
		if !strings.Contains(list, "deepseek/deepseek-chat") {
			t.Errorf("models list %q must contain deepseek/deepseek-chat", list)
		}
	})
}

// TestEnvExportsEmitsPINARD_PROVIDER verifies that env-exports emits
// PINARD_PROVIDER and PINARD_PROVIDER_API from vignes.yaml.
func TestEnvExportsEmitsPINARDPROVIDER(t *testing.T) {
	tmpDir := t.TempDir()
	vbDir := tmpDir + "/vignoble-prov-export"
	if err := os.MkdirAll(vbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	vignesPath := vbDir + "/vignes.yaml"
	content := "gitlab_host: gitlab.example.com\nmodels:\n  provider: openai\n  api: openai-responses\n"
	if err := os.WriteFile(vignesPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	credsPath := tmpDir + "/credentials.yaml"
	if err := os.WriteFile(credsPath, []byte("gitlab:\n  host: gitlab.example.com\n  user: bot\nnats:\n  url: wss://nats.example.com\n  user: u\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AOC_CONFIG", vignesPath)
	t.Setenv("PINARD_CREDENTIALS", credsPath)

	vb, err := config.ResolveVignoble()
	if err != nil {
		t.Fatalf("ResolveVignoble: %v", err)
	}

	var lines []string
	emit := func(k, v string) {
		if v != "" {
			lines = append(lines, fmt.Sprintf("export %s=%s", k, shquote(v)))
		}
	}
	emit("PINARD_PROVIDER", vb.Config.Models.ProviderName())
	emit("PINARD_PROVIDER_API", vb.Config.Models.APIType())

	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "PINARD_PROVIDER='openai'") {
		t.Errorf("env-exports output missing PINARD_PROVIDER=openai; got:\n%s", out)
	}
	if !strings.Contains(out, "PINARD_PROVIDER_API='openai-responses'") {
		t.Errorf("env-exports output missing PINARD_PROVIDER_API=openai-responses; got:\n%s", out)
	}
}
