package memory

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLLMClientCompleteAnthropic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.Error(w, "not found", 404)
			return
		}
		resp := map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "test response"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &LLMClient{
		API:     "anthropic-messages",
		Model:   "claude-haiku-4-5-20251001",
		BaseURL: srv.URL,
		hc:      &http.Client{},
		tp:      &staticKeyProvider{key: "test-key"},
	}

	text, err := c.Complete([]LLMMessage{{Role: "user", Content: "hello"}}, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if text != "test response" {
		t.Errorf("response = %q, want %q", text, "test response")
	}
}

func TestLLMClientCompleteOpenAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An override BaseURL is treated as the full API root (OpenAI SDK
		// convention): only "/chat/completions" is appended, never "/v1/...".
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "not found", 404)
			return
		}
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "openai response"}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &LLMClient{
		API:     "openai-chat",
		Model:   "gpt-4",
		BaseURL: srv.URL,
		hc:      &http.Client{},
		tp:      &staticKeyProvider{key: "test-key"},
	}

	text, err := c.Complete([]LLMMessage{{Role: "user", Content: "hello"}}, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if text != "openai response" {
		t.Errorf("response = %q, want %q", text, "openai response")
	}
}

func TestLLMClientAuthError401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", 401)
	}))
	defer srv.Close()

	c := &LLMClient{
		API:     "anthropic-messages",
		Model:   "test",
		BaseURL: srv.URL,
		hc:      &http.Client{},
		tp:      &staticKeyProvider{key: "bad-key"},
	}

	_, err := c.Complete([]LLMMessage{{Role: "user", Content: "hi"}}, 10, "")
	if err == nil {
		t.Fatal("expected auth error")
	}
	if _, ok := err.(*LLMAuthError); !ok {
		t.Errorf("expected *LLMAuthError, got %T: %v", err, err)
	}
}

func TestStaticKeyProviderNoKey(t *testing.T) {
	p := &staticKeyProvider{key: ""}
	_, err := p.GetToken()
	if err == nil {
		t.Fatal("expected error for empty key")
	}
	if _, ok := err.(*LLMAuthError); !ok {
		t.Errorf("expected *LLMAuthError, got %T", err)
	}
}

func TestURLTokenProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"api_key": "fetched-key"})
	}))
	defer srv.Close()

	p := &urlTokenProvider{urls: []string{srv.URL}, hc: &http.Client{}}
	tok, err := p.GetToken()
	if err != nil {
		t.Fatal(err)
	}
	if tok != "fetched-key" {
		t.Errorf("token = %q, want fetched-key", tok)
	}
}

func TestLLMClientSystemPrompt(t *testing.T) {
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		resp := map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &LLMClient{
		API:     "anthropic-messages",
		Model:   "test",
		BaseURL: srv.URL,
		hc:      &http.Client{},
		tp:      &staticKeyProvider{key: "k"},
	}

	_, err := c.Complete([]LLMMessage{{Role: "user", Content: "hi"}}, 10, "You are a helper")
	if err != nil {
		t.Fatal(err)
	}
	if capturedBody["system"] != "You are a helper" {
		t.Errorf("system prompt not set: %v", capturedBody["system"])
	}
}

// ── googleSAProvider tests ────────────────────────────────────────────────────

// TestGoogleSAProviderMissingFile checks that a non-existent credentials file
// returns an LLMAuthError (not a panic or generic error).
func TestGoogleSAProviderMissingFile(t *testing.T) {
	p := &googleSAProvider{saPath: "/nonexistent/path/sa.json"}
	_, err := p.GetToken()
	if err == nil {
		t.Fatal("expected error for missing credentials file")
	}
	if _, ok := err.(*LLMAuthError); !ok {
		t.Errorf("expected *LLMAuthError, got %T: %v", err, err)
	}
}

// TestGoogleSAProviderInvalidJSON checks that a malformed JSON file returns an
// LLMAuthError from the parse step (not a gcloud exec error).
func TestGoogleSAProviderInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badFile, []byte("not-valid-json{{{"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &googleSAProvider{saPath: badFile}
	_, err := p.GetToken()
	if err == nil {
		t.Fatal("expected error for invalid JSON credentials")
	}
	authErr, ok := err.(*LLMAuthError)
	if !ok {
		t.Errorf("expected *LLMAuthError, got %T: %v", err, err)
		return
	}
	// Must NOT mention gcloud — this error must come from the Go oauth2 library.
	if containsGcloud(authErr.Message) {
		t.Errorf("error message mentions gcloud (should use native Go auth): %s", authErr.Message)
	}
}

// TestGoogleSAProviderTokenCached checks that a warm cache is returned without
// re-reading the file. We inject a pre-filled token/expiry directly.
func TestGoogleSAProviderTokenCached(t *testing.T) {
	p := &googleSAProvider{
		saPath:  "/should-not-be-read.json",
		token:   "cached-access-token",
		expires: timeNowPlusTen(),
	}
	tok, err := p.GetToken()
	if err != nil {
		t.Fatalf("unexpected error with warm cache: %v", err)
	}
	if tok != "cached-access-token" {
		t.Errorf("token = %q, want cached-access-token", tok)
	}
}

// TestResolveTokenProviderGoogleSA checks that MEMORY_LLM_AUTH=google-sa wires
// up a googleSAProvider (not a staticKeyProvider or urlTokenProvider), and that
// it fails fast with LLMAuthError when the file is missing (not with an exec
// error from gcloud).
func TestResolveTokenProviderGoogleSA(t *testing.T) {
	dir := t.TempDir()
	fakeCredPath := filepath.Join(dir, "sa.json")
	// Write a syntactically valid but semantically invalid SA JSON — enough to
	// get past os.ReadFile and into google.CredentialsFromJSON.
	saJSON := `{"type":"service_account","project_id":"test","private_key_id":"kid",` +
		`"private_key":"not-a-real-key","client_email":"test@test.iam.gserviceaccount.com",` +
		`"client_id":"12345","auth_uri":"https://accounts.google.com/o/oauth2/auth",` +
		`"token_uri":"https://oauth2.googleapis.com/token"}`
	if err := os.WriteFile(fakeCredPath, []byte(saJSON), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MEMORY_LLM_AUTH", "google-sa")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fakeCredPath)

	tp, err := resolveTokenProvider("openai-chat", &http.Client{})
	if err != nil {
		t.Fatalf("resolveTokenProvider unexpected error: %v", err)
	}
	if _, ok := tp.(*googleSAProvider); !ok {
		t.Errorf("expected *googleSAProvider, got %T", tp)
	}
}

// TestResolveTokenProviderGoogleSARequiresCreds verifies that MEMORY_LLM_AUTH=google-sa
// without GOOGLE_APPLICATION_CREDENTIALS returns an error from resolveTokenProvider.
func TestResolveTokenProviderGoogleSARequiresCreds(t *testing.T) {
	t.Setenv("MEMORY_LLM_AUTH", "google-sa")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")

	_, err := resolveTokenProvider("openai-chat", &http.Client{})
	if err == nil {
		t.Fatal("expected error when GOOGLE_APPLICATION_CREDENTIALS is unset")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func containsGcloud(s string) bool {
	return len(s) >= 6 && (func() bool {
		for i := 0; i <= len(s)-6; i++ {
			if s[i:i+6] == "gcloud" {
				return true
			}
		}
		return false
	})()
}

func timeNowPlusTen() time.Time {
	return time.Now().Add(10 * time.Minute)
}

// TestOpenAIChatURL guards the #264 regression: the OpenAI-compat completions
// URL must not double the version segment. An override base (e.g. a Vertex
// ".../endpoints/openapi" endpoint) already includes the version, so only
// "/chat/completions" is appended — never "/v1/chat/completions".
func TestOpenAIChatURL(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "default (no override) keeps /v1",
			baseURL: "",
			want:    "https://api.openai.com/v1/chat/completions",
		},
		{
			name:    "vertex openapi base does not double /v1 (#264)",
			baseURL: "https://aiplatform.googleapis.com/v1beta1/projects/p/locations/global/endpoints/openapi",
			want:    "https://aiplatform.googleapis.com/v1beta1/projects/p/locations/global/endpoints/openapi/chat/completions",
		},
		{
			name:    "trailing slash on override is trimmed",
			baseURL: "https://host/endpoints/openapi/",
			want:    "https://host/endpoints/openapi/chat/completions",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &LLMClient{BaseURL: tc.baseURL}
			if got := c.openAIChatURL(); got != tc.want {
				t.Errorf("openAIChatURL()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// ── Round-robin urlTokenProvider tests ───────────────────────────────────────

func TestURLTokenProviderRoundRobin(t *testing.T) {
	// Two servers; verify that consecutive calls alternate between them.
	calls := make([]int, 2)
	make1 := func(i int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[i]++
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
		}))
	}
	s0 := make1(0)
	s1 := make1(1)
	defer s0.Close()
	defer s1.Close()

	p := &urlTokenProvider{urls: []string{s0.URL, s1.URL}, hc: &http.Client{}}
	for i := 0; i < 4; i++ {
		if _, err := p.GetToken(); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}
	// Each server should have been called exactly twice.
	if calls[0] != 2 || calls[1] != 2 {
		t.Errorf("call distribution = %v, want [2 2]", calls)
	}
}

func TestURLTokenProviderFailover(t *testing.T) {
	// First server always returns 500; second returns a valid token.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "good-token")
	}))
	defer good.Close()

	p := &urlTokenProvider{urls: []string{bad.URL, good.URL}, hc: &http.Client{}}
	tok, err := p.GetToken()
	if err != nil {
		t.Fatalf("expected failover to succeed, got: %v", err)
	}
	if tok != "good-token" {
		t.Errorf("token = %q, want good-token", tok)
	}
}

func TestURLTokenProviderAllFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "error", http.StatusInternalServerError)
	}))
	defer bad.Close()

	p := &urlTokenProvider{urls: []string{bad.URL, bad.URL}, hc: &http.Client{}}
	_, err := p.GetToken()
	if err == nil {
		t.Fatal("expected error when all URLs fail")
	}
	if _, ok := err.(*LLMAuthError); !ok {
		t.Errorf("expected *LLMAuthError, got %T: %v", err, err)
	}
}

func TestURLTokenProviderGone(t *testing.T) {
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "revoked", http.StatusGone)
	}))
	defer gone.Close()

	p := &urlTokenProvider{urls: []string{gone.URL}, hc: &http.Client{}}
	_, err := p.GetToken()
	if err == nil {
		t.Fatal("expected error for 410 Gone")
	}
	authErr, ok := err.(*LLMAuthError)
	if !ok {
		t.Fatalf("expected *LLMAuthError, got %T: %v", err, err)
	}
	if !strings.Contains(authErr.Message, "all token URLs failed") {
		t.Errorf("error message %q does not mention all URLs failed", authErr.Message)
	}
}

func TestResolveTokenProviderURLMulti(t *testing.T) {
	t.Setenv("MEMORY_LLM_AUTH", "url")
	t.Setenv("MEMORY_TOKEN_URLS", "https://url1.example.com,https://url2.example.com")
	t.Setenv("MEMORY_TOKEN_URL", "")

	tp, err := resolveTokenProvider("anthropic-messages", &http.Client{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	p, ok := tp.(*urlTokenProvider)
	if !ok {
		t.Fatalf("expected *urlTokenProvider, got %T", tp)
	}
	if len(p.urls) != 2 {
		t.Errorf("expected 2 URLs, got %d: %v", len(p.urls), p.urls)
	}
	if p.urls[0] != "https://url1.example.com" || p.urls[1] != "https://url2.example.com" {
		t.Errorf("unexpected URLs: %v", p.urls)
	}
}
