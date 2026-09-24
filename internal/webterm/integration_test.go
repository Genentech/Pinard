package webterm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nats-io/nats.go"
)

// TestEndToEndReadOnlyStream streams a scratch tmux session over NATS through the
// gateway to a WebSocket client, read-only. Skipped where tmux is unavailable.
func TestEndToEndReadOnlyStream(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	// Opt-in: this drives a real tmux/pty and streams the pane back over a
	// websocket. It is flaky on headless CI runners (e.g. GitHub ubuntu-latest,
	// which ships tmux so the LookPath guard above does not skip it) because the
	// unattached pane never renders the typed marker. Run it deliberately with a
	// real terminal via PINARD_WEBTERM_E2E=1.
	if os.Getenv("PINARD_WEBTERM_E2E") == "" {
		t.Skip("set PINARD_WEBTERM_E2E=1 to run the tmux/pty end-to-end stream test")
	}

	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	socket := fmt.Sprintf("webterm-test-%d", os.Getpid())
	sess := "scratch"
	marker := "MARKER_ABC_" + strconv.Itoa(os.Getpid())
	// Scratch session that keeps re-printing the marker so live output flows.
	loop := fmt.Sprintf("while :; do echo %s; sleep 0.4; done", marker)
	if err := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", sess, "sh", "-c", loop).Run(); err != nil {
		t.Fatalf("tmux new-session: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
		_ = os.Remove(tmuxSocketPath(socket))
	})

	// Responder attached to the scratch socket.
	runResponder(t, nc, "test", socket)

	// Gateway over httptest.
	gw := &Gateway{NC: nc, Vignobles: []string{"test"}, LinkSecret: testSecret, GrantSecret: grantSecret}
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	// Signed link → WS URL.
	exp := time.Now().Add(time.Hour)
	link := BuildLink(srv.URL, "test", sess, exp, testSecret)
	wsURL := "ws" + strings.TrimPrefix(strings.Replace(link, "/sessions?", "/sessions/ws?", 1), "http")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	// Resize control (exercises the ctl path).
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","cols":100,"rows":30}`))

	// Read until we see the marker or time out.
	deadline := time.Now().Add(6 * time.Second)
	var buf strings.Builder
	found := false
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		buf.Write(data)
		if strings.Contains(buf.String(), marker) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("marker %q not seen in stream (got %d bytes)", marker, buf.Len())
	}
}

// TestTermPageRenders confirms the terminal page template executes (incl. the
// {{.Writable}} flag) and defaults to read-only for a viewer link.
func TestTermPageRenders(t *testing.T) {
	gw := &Gateway{Vignobles: []string{"test"}, LinkSecret: testSecret, GrantSecret: grantSecret}
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	link := BuildLink(srv.URL, "test", "sess", time.Now().Add(time.Hour), testSecret)
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("term page → %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !regexp.MustCompile(`writable\s*=\s*false`).Match(body) {
		t.Fatal("expected a read-only page (writable=false)")
	}
}

// TestBadLinkRejected confirms the page/WS handlers reject a tampered link.
func TestBadLinkRejected(t *testing.T) {
	gw := &Gateway{Vignobles: []string{"test"}, LinkSecret: testSecret, GrantSecret: grantSecret}
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	// Valid link, then corrupt the signature.
	link := BuildLink(srv.URL, "test", "s", time.Now().Add(time.Hour), testSecret)
	bad := link[:len(link)-4] + "0000"
	resp, err := http.Get(bad)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for tampered link, got %d", resp.StatusCode)
	}
}
