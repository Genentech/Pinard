package webterm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// pipeBuffer continuously drains an *os.File into a synchronised buffer, so
// tests can poll for expected substrings without blocking on a fixed-size
// pipe filling up.
type pipeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func drainInto(f *os.File) *pipeBuffer {
	pb := &pipeBuffer{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				pb.mu.Lock()
				pb.buf.Write(buf[:n])
				pb.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return pb
}

func (pb *pipeBuffer) contains(s string) bool {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	return bytes.Contains(pb.buf.Bytes(), []byte(s))
}

func (pb *pipeBuffer) waitFor(t *testing.T, s string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pb.contains(s) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	pb.mu.Lock()
	got := pb.buf.String()
	pb.mu.Unlock()
	t.Fatalf("timed out waiting for %q in mirrored output; got: %q", s, got)
}

// withFakeTerminal swaps os.Stdin/os.Stdout for pipes for the duration of a
// RunSupervised call, standing in for the supervisor's real controlling
// terminal. Returns the write end of the fake stdin (for simulated operator
// input) and a pipeBuffer draining the fake stdout (mirrored child output).
func withFakeTerminal(t *testing.T) (opIn *os.File, mirrored *pipeBuffer) {
	t.Helper()
	rIn, wIn, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = rIn, wOut
	t.Cleanup(func() {
		os.Stdin, os.Stdout = origIn, origOut
		_ = wIn.Close()
		_ = rOut.Close()
	})

	return wIn, drainInto(rOut)
}

type superResult struct {
	code int
	err  error
}

func runSupervisedAsync(ctx context.Context, cmdArgs []string, resp *Responder, target string) <-chan superResult {
	done := make(chan superResult, 1)
	go func() {
		code, err := RunSupervised(ctx, cmdArgs, resp, target)
		done <- superResult{code, err}
	}()
	return done
}

// TestRunSupervisedMirrorsOutputAndPropagatesExitCode verifies that a child's
// output reaches the real controlling terminal and its exit code is returned.
func TestRunSupervisedMirrorsOutputAndPropagatesExitCode(t *testing.T) {
	opIn, mirrored := withFakeTerminal(t)

	done := runSupervisedAsync(context.Background(),
		[]string{"sh", "-c", "read x; printf 'out-%s\\n' \"$x\"; exit 7"}, nil, "")

	if _, err := opIn.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write operator input: %v", err)
	}

	mirrored.waitFor(t, "out-hello", 3*time.Second)

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("RunSupervised returned error: %v", res.err)
		}
		if res.code != 7 {
			t.Errorf("expected exit code 7, got %d", res.code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for RunSupervised to return")
	}
}

// TestRunSupervisedGrantsGateKeystrokes verifies that a viewer's grant mode is
// enforced end-to-end through the supervised PTY: a read-only grant cannot
// inject keystrokes into the child, while a read-write grant can.
func TestRunSupervisedGrantsGateKeystrokes(t *testing.T) {
	_, mirrored := withFakeTerminal(t)

	ns, url := startEmbeddedNATSLocal(t)
	defer ns.Shutdown()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	vignoble := "supervised-vigne"
	target := "supervised-session"
	resp := &Responder{
		NC:          nc,
		Vignoble:    vignoble,
		GrantSecret: grantSecret,
		MaxViewers:  4,
	}

	done := runSupervisedAsync(context.Background(),
		[]string{"sh", "-c", "read a; printf 'saw-%s\\n' \"$a\"; exit 0"}, resp, target)

	// Wait for the responder to come up (subscription registration is async).
	time.Sleep(150 * time.Millisecond)

	attach := func(mode string) ReqReply {
		grant, err := SignGrant(Grant{Vignoble: vignoble, Target: target, Mode: mode, Exp: time.Now().Add(time.Minute).Unix()}, grantSecret)
		if err != nil {
			t.Fatalf("sign grant: %v", err)
		}
		viewerID := "viewer-" + mode
		req, _ := json.Marshal(ReqMsg{Grant: grant, ViewerID: viewerID, Cols: 80, Rows: 24})
		msg, err := nc.Request(ReqSubject(vignoble), req, 2*time.Second)
		if err != nil {
			t.Fatalf("request (%s): %v", mode, err)
		}
		var reply ReqReply
		if err := json.Unmarshal(msg.Data, &reply); err != nil {
			t.Fatalf("reply unmarshal (%s): %v", mode, err)
		}
		if !reply.OK {
			t.Fatalf("expected OK reply for %s grant, got: %s", mode, reply.Reason)
		}
		return reply
	}

	attach(ModeRO)
	time.Sleep(80 * time.Millisecond)
	_ = nc.Publish(InSubject(vignoble, "viewer-"+ModeRO), []byte("sneaky\n"))
	_ = nc.Flush()

	// The RO keystroke must not reach the child: it's still blocked on `read a`,
	// so RunSupervised must not have returned yet.
	select {
	case res := <-done:
		t.Fatalf("child exited after an RO keystroke (leak): code=%d err=%v", res.code, res.err)
	case <-time.After(300 * time.Millisecond):
	}
	if mirrored.contains("saw-") {
		t.Fatal("RO keystroke leaked into the child's input")
	}

	attach(ModeRW)
	time.Sleep(80 * time.Millisecond)
	_ = nc.Publish(InSubject(vignoble, "viewer-"+ModeRW), []byte("letmein\n"))
	_ = nc.Flush()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("RunSupervised returned error: %v", res.err)
		}
		if res.code != 0 {
			t.Errorf("expected exit code 0, got %d", res.code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the RW keystroke to reach the child")
	}
	mirrored.waitFor(t, "saw-letmein", time.Second)
}

// TestRunSupervisedConcurrentViewersGetFullOutput verifies that two
// simultaneously-attached viewers each receive a complete, uncorrupted copy
// of the child's output (a regression test for a shared-reader race where
// concurrent viewers — or the local terminal mirror — would otherwise split
// bytes between them instead of each seeing the full stream).
func TestRunSupervisedConcurrentViewersGetFullOutput(t *testing.T) {
	_, mirrored := withFakeTerminal(t)

	ns, url := startEmbeddedNATSLocal(t)
	defer ns.Shutdown()
	nc, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	vignoble := "concurrent-vigne"
	target := "concurrent-session"
	resp := &Responder{
		NC:          nc,
		Vignoble:    vignoble,
		GrantSecret: grantSecret,
		MaxViewers:  4,
	}

	// The child delays its first line so both viewers attach before anything
	// has been broadcast (ptyFanout's replay buffer would otherwise mask a
	// split-stream regression here), and paces subsequent lines so the run
	// overlaps both viewers' subscription windows.
	done := runSupervisedAsync(context.Background(),
		[]string{"sh", "-c", "sleep 0.3; for i in 1 2 3 4 5; do echo line-$i; sleep 0.05; done"}, resp, target)

	time.Sleep(150 * time.Millisecond)

	attachAndCollect := func(viewerID string) *pipeBuffer {
		grant, err := SignGrant(Grant{Vignoble: vignoble, Target: target, Mode: ModeRO, Exp: time.Now().Add(time.Minute).Unix()}, grantSecret)
		if err != nil {
			t.Fatalf("sign grant: %v", err)
		}
		pb := &pipeBuffer{}
		_, err = nc.Subscribe(OutSubject(vignoble, viewerID), func(m *nats.Msg) {
			pb.mu.Lock()
			pb.buf.Write(m.Data)
			pb.mu.Unlock()
		})
		if err != nil {
			t.Fatalf("subscribe out (%s): %v", viewerID, err)
		}
		req, _ := json.Marshal(ReqMsg{Grant: grant, ViewerID: viewerID, Cols: 80, Rows: 24})
		msg, err := nc.Request(ReqSubject(vignoble), req, 2*time.Second)
		if err != nil {
			t.Fatalf("request (%s): %v", viewerID, err)
		}
		var reply ReqReply
		if err := json.Unmarshal(msg.Data, &reply); err != nil {
			t.Fatalf("reply unmarshal (%s): %v", viewerID, err)
		}
		if !reply.OK {
			t.Fatalf("expected OK reply for %s, got: %s", viewerID, reply.Reason)
		}
		return pb
	}

	viewerA := attachAndCollect("viewer-a")
	viewerB := attachAndCollect("viewer-b")

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("RunSupervised returned error: %v", res.err)
		}
		if res.code != 0 {
			t.Errorf("expected exit code 0, got %d", res.code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for RunSupervised to return")
	}

	for i := 1; i <= 5; i++ {
		line := fmt.Sprintf("line-%d", i)
		if !mirrored.contains(line) {
			t.Errorf("local terminal mirror missing %q", line)
		}
		if !viewerA.contains(line) {
			t.Errorf("viewer A missing %q", line)
		}
		if !viewerB.contains(line) {
			t.Errorf("viewer B missing %q", line)
		}
	}
}

// TestPTYFanoutReplaysBufferedOutputToLateSubscriber verifies that a viewer
// subscribing after output has already been produced is repainted with that
// output immediately, ahead of any subsequently broadcast live chunk.
func TestPTYFanoutReplaysBufferedOutputToLateSubscriber(t *testing.T) {
	f := newPTYFanout(nil)

	f.broadcast([]byte("hello "))
	f.broadcast([]byte("world"))

	sub := f.subscribe()
	defer sub.Close()

	buf := make([]byte, 32)
	n, err := sub.Read(buf)
	if err != nil {
		t.Fatalf("read replayed chunk: %v", err)
	}
	if got := string(buf[:n]); got != "hello world" {
		t.Fatalf("expected replayed buffer %q, got %q", "hello world", got)
	}

	// A live chunk broadcast after subscribing must arrive strictly after the
	// replay, not interleaved with or ahead of it.
	f.broadcast([]byte("!"))
	n, err = sub.Read(buf)
	if err != nil {
		t.Fatalf("read live chunk: %v", err)
	}
	if got := string(buf[:n]); got != "!" {
		t.Fatalf("expected live chunk %q, got %q", "!", got)
	}
}

// TestPTYFanoutReplayBufferIsBounded verifies that the replay buffer never
// grows past replayCapBytes and always retains the most recent bytes.
func TestPTYFanoutReplayBufferIsBounded(t *testing.T) {
	f := newPTYFanout(nil)

	f.broadcast(bytes.Repeat([]byte("a"), replayCapBytes))
	f.broadcast([]byte("TAIL"))

	sub := f.subscribe()
	defer sub.Close()

	replayed := make([]byte, replayCapBytes)
	if _, err := io.ReadFull(sub, replayed); err != nil {
		t.Fatalf("read replay buffer: %v", err)
	}
	if !bytes.HasSuffix(replayed, []byte("TAIL")) {
		t.Fatalf("expected replay buffer to end with %q, got tail %q", "TAIL", replayed[len(replayed)-16:])
	}

	// Nothing beyond the capped buffer should have been kept: the next live
	// chunk must be exactly what's broadcast now, not stale overflow.
	f.broadcast([]byte("NEXT"))
	more := make([]byte, 4)
	if _, err := io.ReadFull(sub, more); err != nil {
		t.Fatalf("read next live chunk: %v", err)
	}
	if string(more) != "NEXT" {
		t.Fatalf("expected next live chunk %q, got %q", "NEXT", more)
	}
}

// TestSupervisedBackendViewerWinsOverLocalResize verifies the "viewer-attached
// wins" PTY size arbitration policy: while a viewer is attached, a simulated
// local SIGWINCH must not override the viewer's size, and once the last
// viewer detaches the local size is resynced.
func TestSupervisedBackendViewerWinsOverLocalResize(t *testing.T) {
	f := newPTYFanout(nil)

	var viewers int32
	var lastCols, lastRows int
	var localRestores int

	setsize := func(cols, rows int) { lastCols, lastRows = cols, rows }
	// Mirrors RunSupervised's syncSize: a no-op while any viewer is attached.
	simulateLocalSigwinch := func() {
		if atomic.LoadInt32(&viewers) > 0 {
			return
		}
		setsize(999, 999)
	}

	backend := &supervisedBackend{
		target:  "t",
		fanout:  f,
		setsize: setsize,
		viewers: &viewers,
		restoreLocalSize: func() {
			localRestores++
			setsize(999, 999)
		},
	}

	// No viewer attached yet: local SIGWINCH applies the local size.
	simulateLocalSigwinch()
	if lastCols != 999 || lastRows != 999 {
		t.Fatalf("expected local size to apply before any viewer attaches, got %dx%d", lastCols, lastRows)
	}

	_, cleanup, _, err := backend.Attach("t", 80, 24, false)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if lastCols != 80 || lastRows != 24 {
		t.Fatalf("expected attach to set the viewer's size, got %dx%d", lastCols, lastRows)
	}

	// A local SIGWINCH while a viewer is attached must not clobber it.
	simulateLocalSigwinch()
	if lastCols != 80 || lastRows != 24 {
		t.Fatalf("local resize must not override an attached viewer's size, got %dx%d", lastCols, lastRows)
	}

	cleanup()
	if localRestores != 1 {
		t.Fatalf("expected restoreLocalSize to run exactly once after the last viewer detaches, got %d", localRestores)
	}
	if lastCols != 999 || lastRows != 999 {
		t.Fatalf("expected local size to be restored after detach, got %dx%d", lastCols, lastRows)
	}
}
