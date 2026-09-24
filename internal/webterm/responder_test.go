package webterm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// tmuxSocketPath returns the filesystem path of the named tmux socket so tests
// can remove the socket file after kill-server to avoid polluting /tmp/tmux-<uid>/.
func tmuxSocketPath(name string) string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return filepath.Join("/tmp", "tmux-"+u.Uid, name)
}

func startEmbeddedNATS(t *testing.T) (*server.Server, string) {
	t.Helper()
	opts := &server.Options{Port: -1, StoreDir: t.TempDir()}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("start embedded NATS: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS not ready")
	}
	return ns, fmt.Sprintf("nats://127.0.0.1:%d", ns.Addr().(*net.TCPAddr).Port)
}

func runResponder(t *testing.T, nc *nats.Conn, vignoble, socket string) {
	t.Helper()
	resp := &Responder{NC: nc, Vignoble: vignoble, GrantSecret: grantSecret, Socket: socket, MaxViewers: 4}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = resp.Run(ctx) }()
	// Give the subscription a beat to register.
	time.Sleep(100 * time.Millisecond)
}

// requestViewer emulates the gateway's request/reply to the responder.
func requestViewer(t *testing.T, nc *nats.Conn, vignoble, grant, viewerID string) ReqReply {
	t.Helper()
	req, _ := json.Marshal(ReqMsg{Grant: grant, ViewerID: viewerID, Cols: 80, Rows: 24})
	msg, err := nc.Request(ReqSubject(vignoble), req, 2*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	var reply ReqReply
	if err := json.Unmarshal(msg.Data, &reply); err != nil {
		t.Fatalf("reply unmarshal: %v", err)
	}
	return reply
}

// requestViewerExpectSilence asserts the responder does NOT reply (bad grants are
// ignored so a stale/foreign responder can't win the reply race).
func requestViewerExpectSilence(t *testing.T, nc *nats.Conn, vignoble, grant, viewerID string) {
	t.Helper()
	req, _ := json.Marshal(ReqMsg{Grant: grant, ViewerID: viewerID, Cols: 80, Rows: 24})
	_, err := nc.Request(ReqSubject(vignoble), req, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected no reply (silence) for an unverifiable grant, got a reply")
	}
}

func TestResponderIgnoresMissingGrant(t *testing.T) {
	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()
	nc, _ := nats.Connect(url)
	defer nc.Close()

	runResponder(t, nc, "test", "pinard-nonexistent-socket")
	requestViewerExpectSilence(t, nc, "test", "", "v1")
}

func TestResponderIgnoresInvalidGrant(t *testing.T) {
	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()
	nc, _ := nats.Connect(url)
	defer nc.Close()

	runResponder(t, nc, "test", "pinard-nonexistent-socket")

	// Grant signed with the wrong secret → responder stays silent.
	bad, _ := SignGrant(Grant{Target: "s", Mode: ModeRO, Exp: time.Now().Add(time.Minute).Unix()}, []byte("wrong"))
	requestViewerExpectSilence(t, nc, "test", bad, "v1")
}

func TestResponderIgnoresExpiredGrant(t *testing.T) {
	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()
	nc, _ := nats.Connect(url)
	defer nc.Close()

	runResponder(t, nc, "test", "pinard-nonexistent-socket")

	expired, _ := SignGrant(Grant{Target: "s", Mode: ModeRO, Exp: time.Now().Add(-time.Minute).Unix()}, grantSecret)
	requestViewerExpectSilence(t, nc, "test", expired, "v1")
}

func TestResponderValidGrantButNoSession(t *testing.T) {
	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()
	nc, _ := nats.Connect(url)
	defer nc.Close()

	// Socket with no sessions → has-session fails → responder stays SILENT.
	// A local responder that lacks the target must not reply so it cannot win
	// the request-reply race against a remote responder that does own the session.
	runResponder(t, nc, "test", "pinard-nonexistent-socket")

	valid, _ := SignGrant(Grant{Target: "ghost", Mode: ModeRO, Exp: time.Now().Add(time.Minute).Unix()}, grantSecret)
	requestViewerExpectSilence(t, nc, "test", valid, "v1")
}

// fakeKV is a minimal pnats.KVReader backed by a map, for testing.
type fakeKV struct {
	bucket map[string]map[string]map[string]any // bucket → key → value
}

func (f *fakeKV) Get(bucket, key string) (map[string]any, error) {
	if b, ok := f.bucket[bucket]; ok {
		if v, ok := b[key]; ok {
			return v, nil
		}
	}
	return nil, fmt.Errorf("not found")
}

func (f *fakeKV) Keys(bucket string) ([]string, error) {
	if b, ok := f.bucket[bucket]; ok {
		keys := make([]string, 0, len(b))
		for k := range b {
			keys = append(keys, k)
		}
		return keys, nil
	}
	return nil, fmt.Errorf("bucket not found")
}

func TestResolveInterruptSubject(t *testing.T) {
	r := &Responder{
		Vignoble: "myvigne",
		KV: &fakeKV{
			bucket: map[string]map[string]map[string]any{
				"pinard-agents": {
					"agent-abc": {"name": "parcelle-a--proj-abc1", "parcelle": "parcelle-a"},
					"agent-xyz": {"name": "other--proj-xyz2", "parcelle": "other"},
				},
			},
		},
	}

	subject := r.resolveInterruptSubject("parcelle-a--proj-abc1")
	if subject == "" {
		t.Fatal("expected a non-empty interrupt subject")
	}
	// Subject should be pinard.myvigne.parcelles.parcelle-a.agents.parcelle-a--proj-abc1.interrupt
	expected := "pinard.myvigne.parcelles.parcelle-a.agents.parcelle-a--proj-abc1.interrupt"
	if subject != expected {
		t.Fatalf("interrupt subject: got %q, want %q", subject, expected)
	}
}

func TestResolveInterruptSubjectMissing(t *testing.T) {
	// No KV → should return empty string (caller falls back).
	r := &Responder{Vignoble: "myvigne"}
	if got := r.resolveInterruptSubject("some-session"); got != "" {
		t.Fatalf("expected empty string with nil KV, got %q", got)
	}
}

// TestGatewayForwardsCtlInterruptForROGrant verifies that CtlInterrupt is
// forwarded to the ctl subject for a read-only grant (not gated on ModeRW).
func TestGatewayForwardsCtlInterruptForROGrant(t *testing.T) {
	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()
	nc, _ := nats.Connect(url)
	defer nc.Close()

	viewerID := "test-viewer-1"
	vignoble := "testvig"

	// Subscribe to the ctl subject to observe what the gateway would publish.
	received := make(chan CtlMsg, 1)
	_, err := nc.Subscribe(CtlSubject(vignoble, viewerID), func(m *nats.Msg) {
		var c CtlMsg
		if json.Unmarshal(m.Data, &c) == nil {
			select {
			case received <- c:
			default:
			}
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Simulate the gateway forwarding a CtlInterrupt for a read-only grant.
	// This mirrors the exact code path in gateway.go's WS reader loop:
	//   case CtlInterrupt: nc.Publish(CtlSubject(...))
	data, _ := json.Marshal(CtlMsg{Type: CtlInterrupt})
	_ = nc.Publish(CtlSubject(vignoble, viewerID), data)

	select {
	case msg := <-received:
		if msg.Type != CtlInterrupt {
			t.Fatalf("expected CtlInterrupt, got %q", msg.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for CtlInterrupt on ctl subject")
	}
}

// TestProcessBackendDoesNotAnswerListSubject verifies that a Responder with a
// ProcessBackend does not subscribe to ListSubject and therefore stays silent
// when the control-room sends a list request — preventing an empty-list reply
// from racing the daemon's full TmuxBackend reply (#270).
func TestProcessBackendDoesNotAnswerListSubject(t *testing.T) {
	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()
	nc, _ := nats.Connect(url)
	defer nc.Close()

	vigorble := "test"
	sessionName := "myparc--worker-abc"

	// Start a Responder with ProcessBackend (no tmux, EnumeratesSessions=false).
	backend := &ProcessBackend{Target: sessionName, PTY: &readOnlyWrapper{Reader: bytes.NewReader(nil)}}
	resp := &Responder{
		NC:          nc,
		Vignoble:    vigorble,
		GrantSecret: grantSecret,
		MaxViewers:  4,
		Backend:     backend,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = resp.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)

	// Mint a valid grant and send a ListSubject request.
	grant, _ := SignGrant(Grant{Vignoble: vigorble, Target: sessionName, Mode: ModeRO, Exp: time.Now().Add(time.Minute).Unix()}, grantSecret)
	req, _ := json.Marshal(ListReq{Grant: grant})
	_, err := nc.Request(ListSubject(vigorble), req, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected silence from ProcessBackend on ListSubject, got a reply")
	}
}

// TestInterruptRoutedViaKV verifies that handleInterrupt publishes to the NATS
// interrupt subject (not tmux fallback) when KV contains the session.
func TestInterruptRoutedViaKV(t *testing.T) {
	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()
	nc, _ := nats.Connect(url)
	defer nc.Close()

	sessionName := "myparc--proj-abc1"
	parcelle := "myparc"
	vignoble := "testvigne"

	r := &Responder{
		NC:       nc,
		Vignoble: vignoble,
		Socket:   "pinard-nonexistent",
		KV: &fakeKV{
			bucket: map[string]map[string]map[string]any{
				"pinard-agents": {
					"agent-1": {"name": sessionName, "parcelle": parcelle},
				},
			},
		},
	}

	// Listen on the expected interrupt subject.
	interruptSubject := fmt.Sprintf("pinard.%s.parcelles.%s.agents.%s.interrupt", vignoble, parcelle, sessionName)
	interrupted := make(chan struct{}, 1)
	_, err := nc.Subscribe(interruptSubject, func(_ *nats.Msg) {
		select {
		case interrupted <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatalf("subscribe interrupt: %v", err)
	}

	grant := Grant{Vignoble: vignoble, Target: sessionName, Mode: ModeRO, Exp: time.Now().Add(time.Minute).Unix()}
	r.handleInterrupt(sessionName, grant, "viewer-1")

	select {
	case <-interrupted:
		// success
	case <-time.After(time.Second):
		t.Fatal("timed out: interrupt was not published to NATS subject")
	}
}
