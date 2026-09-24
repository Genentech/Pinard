package webterm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// operatorGateway builds a gateway with a minimal cookie-only Authenticator
// (no OIDC discovery needed for the session-cookie path).
func operatorGateway(t *testing.T, nc *nats.Conn) (*Gateway, *Authenticator) {
	t.Helper()
	a := &Authenticator{cookieSecret: []byte("cookie-secret"), sessionTTL: time.Hour}
	gw := &Gateway{NC: nc, LinkSecret: testSecret, GrantSecret: grantSecret, Auth: a}
	return gw, a
}

func cookieFor(t *testing.T, a *Authenticator, username string) *http.Cookie {
	t.Helper()
	tok, err := a.signSession(Identity{Username: username}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: sessionCookie, Value: tok}
}

func TestControlRoomAPIGate(t *testing.T) {
	ns, url := startEmbeddedNATSJS(t)
	defer ns.Shutdown()
	client := newJSClient(t, url)
	defer client.Close()
	kv := newKVClient(client)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	if err := PublishOwner(kv, "exohub", "lelongs"); err != nil {
		t.Fatal(err)
	}
	gw, a := operatorGateway(t, nc)
	gw.Owners = &OwnerStore{KV: kv}
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	do := func(path string, c *http.Cookie) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if c != nil {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 4096)
		n, _ := resp.Body.Read(b)
		resp.Body.Close()
		return resp, b[:n]
	}

	op := cookieFor(t, a, "lelongs")

	// Unauthenticated → 401.
	if resp, _ := do("/sessions/api/vignobles", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no cookie → %d, want 401", resp.StatusCode)
	}
	// Operator → owned vignobles.
	resp, body := do("/sessions/api/vignobles", op)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("vignobles → %d, want 200", resp.StatusCode)
	}
	var vs struct{ Vignobles []string }
	_ = json.Unmarshal(body, &vs)
	if len(vs.Vignobles) != 1 || vs.Vignobles[0] != "exohub" {
		t.Fatalf("vignobles = %v, want [exohub]", vs.Vignobles)
	}
	// Non-owned vignoble → 403.
	if resp, _ := do("/sessions/api/sessions?v=genomics", op); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owned vignoble → %d, want 403", resp.StatusCode)
	}
	// Owned vignoble, no responder → 200 with graceful note.
	resp, body = do("/sessions/api/sessions?v=exohub", op)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owned vignoble → %d, want 200", resp.StatusCode)
	}
	var idx sessionIndex
	if err := json.Unmarshal(body, &idx); err != nil || idx.Vignoble != "exohub" {
		t.Fatalf("bad index json: %v (%s)", err, body)
	}
}

// End-to-end: a live responder enumerates a scratch conductor + worker session,
// and the gateway's /api/sessions surfaces the régisseur + vendangeur. Skipped
// where tmux is unavailable.
func TestControlRoomLiveEnumeration(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	ns, url := startEmbeddedNATSJS(t)
	defer ns.Shutdown()
	client := newJSClient(t, url)
	defer client.Close()
	kv := newKVClient(client)
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	socket := fmt.Sprintf("webterm-cr-%d", os.Getpid())
	// A conductor session (régisseur window 0) + one vendangeur session.
	if err := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", "conductor", "-n", "[régisseur]", "sh", "-c", "sleep 60").Run(); err != nil {
		t.Fatalf("tmux conductor: %v", err)
	}
	_ = exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", "exo-cli--exo-cli-abc", "sh", "-c", "sleep 60").Run()
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
		_ = os.Remove(tmuxSocketPath(socket))
	})

	if err := PublishOwner(kv, "exohub", "lelongs"); err != nil {
		t.Fatal(err)
	}
	// Responder bound to the scratch socket.
	runResponder(t, nc, "exohub", socket)

	gw, a := operatorGateway(t, nc)
	gw.Owners = &OwnerStore{KV: kv}
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/sessions/api/sessions?v=exohub", nil)
	req.AddCookie(cookieFor(t, a, "lelongs"))
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var idx sessionIndex
	if err := json.NewDecoder(r.Body).Decode(&idx); err != nil {
		t.Fatal(err)
	}
	if idx.Regisseur == nil {
		t.Fatal("expected a régisseur entry")
	}
	found := false
	for _, e := range idx.Vendangeurs {
		if e.Name == "exo-cli--exo-cli-abc" {
			found = true
			if e.Parcelle != "exo-cli" {
				t.Fatalf("parcelle = %q, want exo-cli", e.Parcelle)
			}
		}
	}
	if !found {
		t.Fatalf("vendangeur not enumerated: %+v", idx.Vendangeurs)
	}
}

// TestBuildIndexRemoteAgents: a KV record with standalone=true and a fresh lastSeen
// and no matching tmux session is surfaced as a Remote vendangeur.
func TestBuildIndexRemoteAgents(t *testing.T) {
	kv := &fakeKV{
		bucket: map[string]map[string]map[string]any{
			"pinard-agents": {
				"agent-remote-1": {
					"vignoble":   "exohub",
					"name":       "mypar--genomics-hpc1",
					"parcelle":   "mypar",
					"tempo":      "active",
					"step":       "building",
					"standalone": true,
					"lastSeen":   time.Now().UTC().Format(time.RFC3339),
				},
			},
		},
	}
	gw := &Gateway{AgentsKV: kv}
	idx := gw.buildIndex("exohub", ListReply{}) // no tmux sessions
	if len(idx.Vendangeurs) != 1 {
		t.Fatalf("expected 1 remote vendangeur, got %d: %+v", len(idx.Vendangeurs), idx.Vendangeurs)
	}
	e := idx.Vendangeurs[0]
	if !e.Remote {
		t.Fatalf("expected Remote=true, got %+v", e)
	}
	if e.Unreachable {
		t.Fatalf("expected Unreachable=false for standalone remote, got %+v", e)
	}
	if e.Name != "mypar--genomics-hpc1" {
		t.Fatalf("unexpected name %q", e.Name)
	}
	if e.Parcelle != "mypar" {
		t.Fatalf("unexpected parcelle %q", e.Parcelle)
	}
	if e.State != "active" {
		t.Fatalf("unexpected state %q", e.State)
	}
	if e.Step != "building" {
		t.Fatalf("unexpected step %q", e.Step)
	}
}

// TestBuildIndexLocalUnreachable: a KV record WITHOUT standalone=true and NOT in
// tmux sessions is surfaced as Unreachable (not Remote) — local agent with down responder.
func TestBuildIndexLocalUnreachable(t *testing.T) {
	kv := &fakeKV{
		bucket: map[string]map[string]map[string]any{
			"pinard-agents": {
				"mypar--exo-cli-xyz": {
					"vignoble": "exohub",
					"name":     "mypar--exo-cli-xyz",
					"parcelle": "mypar",
					"tempo":    "active",
					"step":     "open-mr",
					"lastSeen": time.Now().UTC().Format(time.RFC3339),
					// standalone absent (local worker)
				},
			},
		},
	}
	gw := &Gateway{AgentsKV: kv}
	idx := gw.buildIndex("exohub", ListReply{}) // no tmux sessions (responder down)
	if len(idx.Vendangeurs) != 1 {
		t.Fatalf("expected 1 unreachable vendangeur, got %d: %+v", len(idx.Vendangeurs), idx.Vendangeurs)
	}
	e := idx.Vendangeurs[0]
	if e.Remote {
		t.Fatalf("local agent must not be Remote, got %+v", e)
	}
	if !e.Unreachable {
		t.Fatalf("expected Unreachable=true for local agent with down responder, got %+v", e)
	}
	if e.Name != "mypar--exo-cli-xyz" {
		t.Fatalf("unexpected name %q", e.Name)
	}
	if e.Parcelle != "mypar" {
		t.Fatalf("unexpected parcelle %q", e.Parcelle)
	}
}

// TestBuildIndexStaleAgents: a KV record with an old lastSeen must NOT appear.
func TestBuildIndexStaleAgents(t *testing.T) {
	kv := &fakeKV{
		bucket: map[string]map[string]map[string]any{
			"pinard-agents": {
				"agent-stale": {
					"vignoble": "exohub",
					"name":     "mypar--genomics-old1",
					"parcelle": "mypar",
					"tempo":    "active",
					"lastSeen": time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339),
				},
			},
		},
	}
	gw := &Gateway{AgentsKV: kv}
	idx := gw.buildIndex("exohub", ListReply{})
	if len(idx.Vendangeurs) != 0 {
		t.Fatalf("expected stale record to be excluded, got %+v", idx.Vendangeurs)
	}
}

// TestBuildIndexNoRegression: a local tmux session that also has a KV record
// must appear exactly once (not duplicated), and must NOT be marked Remote.
func TestBuildIndexNoRegression(t *testing.T) {
	// For non-process workers the KV key is the session name (AGENT_ID == SESSION).
	kv := &fakeKV{
		bucket: map[string]map[string]map[string]any{
			"pinard-agents": {
				"mypar--exo-cli-abc": {
					"vignoble": "exohub",
					"name":     "mypar--exo-cli-abc",
					"parcelle": "mypar",
					"tempo":    "active",
					"step":     "running tests",
					"lastSeen": time.Now().UTC().Format(time.RFC3339),
				},
			},
		},
	}
	gw := &Gateway{AgentsKV: kv}
	reply := ListReply{
		Sessions: []string{"conductor", "mypar--exo-cli-abc"},
	}
	idx := gw.buildIndex("exohub", reply)
	if len(idx.Vendangeurs) != 1 {
		t.Fatalf("expected exactly 1 vendangeur, got %d: %+v", len(idx.Vendangeurs), idx.Vendangeurs)
	}
	e := idx.Vendangeurs[0]
	if e.Remote {
		t.Fatalf("local session must not be marked Remote")
	}
	if e.Unreachable {
		t.Fatalf("local session in tmux list must not be marked Unreachable")
	}
	if e.Name != "mypar--exo-cli-abc" {
		t.Fatalf("unexpected name %q", e.Name)
	}
	// Step should be enriched from KV.
	if e.Step != "running tests" {
		t.Fatalf("expected step %q from KV, got %q", "running tests", e.Step)
	}
}

// The responder must ignore a list request with an unverifiable grant (silent).
func TestResponderListIgnoresBadGrant(t *testing.T) {
	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()
	nc, _ := nats.Connect(url)
	defer nc.Close()
	runResponder(t, nc, "exohub", "pinard-nonexistent")

	bad, _ := SignGrant(Grant{Vignoble: "exohub", Mode: ModeList, Exp: time.Now().Add(time.Minute).Unix()}, []byte("wrong"))
	req, _ := json.Marshal(ListReq{Grant: bad})
	if _, err := nc.Request(ListSubject("exohub"), req, 500*time.Millisecond); err == nil {
		t.Fatal("expected silence (no reply) for a list request with a bad grant")
	}
}
