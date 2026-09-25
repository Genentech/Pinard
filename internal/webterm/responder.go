package webterm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Genentech/pinard/internal/pnats"
	"github.com/Genentech/pinard/internal/session"
	"github.com/creack/pty"
	"github.com/nats-io/nats.go"
)

// Flow-control tuning. Terminal output is coalesced over short windows and
// rate-capped inside a bounded buffer so a runaway `cat`/`yes` cannot flood a
// viewer or the NATS connection.
const (
	flushInterval = 16 * time.Millisecond
	maxMsgBytes   = 32 * 1024       // max bytes per NATS output message
	maxBufBytes   = 256 * 1024      // bounded per-viewer buffer; overflow is dropped
	rateCapPerSec = 2 * 1024 * 1024 // ~2 MiB/s output ceiling per viewer
	throttleMark  = "\r\n\x1b[33m…output throttled…\x1b[0m\r\n"
)

// PTYBackend abstracts how the Responder acquires a PTY for a viewer. Two
// implementations exist: TmuxBackend (attaches a local tmux session — the host
// daemon / standalone webterm-responder path) and ProcessBackend (wraps an
// already-open PTY file descriptor — the daemon-less worker self-serve path).
type PTYBackend interface {
	// HasSession returns true when the named target is available to attach.
	HasSession(target string) bool
	// Attach opens a PTY for the given target and size. writable controls whether
	// the PTY is opened read-write (steer) or read-only. Returns the PTY file (for
	// read/write/resize), a cleanup func that must be called when done, and a
	// resize func that adjusts the backing session's window dimensions. Any of these
	// may be no-ops depending on the backend.
	Attach(target string, cols, rows int, writable bool) (ptmx io.ReadWriter, cleanup func(), resize func(cols, rows int), err error)
	// EnumeratesSessions returns true when the backend can enumerate local tmux
	// sessions for control-room listing. Only TmuxBackend returns true;
	// ProcessBackend returns false — it must not answer ListSubject.
	EnumeratesSessions() bool
}

// TmuxBackend is the standard backend: attaches a local tmux session.
type TmuxBackend struct {
	// Socket is the tmux socket name; defaults to "pinard-<Vignoble>".
	Socket   string
	Vignoble string
}

func (b *TmuxBackend) socket() string {
	if b.Socket != "" {
		return b.Socket
	}
	return "pinard-" + b.Vignoble
}

func (b *TmuxBackend) EnumeratesSessions() bool { return true }

func (b *TmuxBackend) HasSession(target string) bool {
	base, _ := parseTarget(target)
	return exec.Command("tmux", "-L", b.socket(), "has-session", "-t", base).Run() == nil
}

func (b *TmuxBackend) Attach(target string, cols, rows int, writable bool) (io.ReadWriter, func(), func(int, int), error) {
	base, window := parseTarget(target)
	plainSession := window == ""

	attachTarget := base
	groupName := ""
	if window != "" {
		groupName = session.SanitizeName("wt-" + target)
		if err := exec.Command("tmux", "-L", b.socket(), "new-session", "-d", "-s", groupName, "-t", base).Run(); err == nil {
			_ = exec.Command("tmux", "-L", b.socket(), "set-option", "-t", groupName, "window-size", "manual").Run()
			_ = exec.Command("tmux", "-L", b.socket(), "select-window", "-t", groupName+":"+window).Run()
			attachTarget = groupName
		} else {
			groupName = ""
		}
	}

	args := []string{"-L", b.socket(), "attach"}
	if !writable {
		args = append(args, "-r")
	}
	args = append(args, "-t", attachTarget)
	cmd := exec.Command("tmux", args...)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		if groupName != "" {
			_ = exec.Command("tmux", "-L", b.socket(), "kill-session", "-t", groupName).Run()
		}
		return nil, nil, nil, err
	}

	cleanup := func() {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if groupName != "" {
			_ = exec.Command("tmux", "-L", b.socket(), "kill-session", "-t", groupName).Run()
		}
	}

	// For plain sessions, drive the tmux window size directly (attach -r sets
	// ignore-size on the client, so we must push the size ourselves).
	resizeFn := func(c, r int) {
		if !plainSession || c <= 0 || r <= 0 {
			_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(c), Rows: uint16(r)})
			return
		}
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(c), Rows: uint16(r)})
		_ = exec.Command("tmux", "-L", b.socket(), "resize-window", "-t", attachTarget,
			"-x", strconv.Itoa(c), "-y", strconv.Itoa(r)).Run()
	}

	if plainSession {
		_ = exec.Command("tmux", "-L", b.socket(), "set-option", "-t", attachTarget, "window-size", "manual").Run()
		_ = exec.Command("tmux", "-L", b.socket(), "resize-window", "-t", attachTarget,
			"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows)).Run()
	}

	return ptmx, cleanup, resizeFn, nil
}

// ProcessBackend serves an already-open PTY (the worker's own pi process PTY).
// HasSession always returns true for the configured target; Attach returns the
// pre-opened PTY fd directly. Resize adjusts the PTY size but there is no tmux
// window to resize. The PTY is NOT closed on cleanup — the caller owns its
// lifetime.
type ProcessBackend struct {
	// Target is the session identifier this backend answers for.
	Target string
	// PTY is the already-open PTY file (read+write).
	PTY io.ReadWriter
	// Setsize resizes the underlying PTY (optional; may be nil).
	Setsize func(cols, rows int)
}

func (b *ProcessBackend) EnumeratesSessions() bool { return false }

func (b *ProcessBackend) HasSession(target string) bool {
	base, _ := parseTarget(target)
	return session.SanitizeName(base) == session.SanitizeName(b.Target)
}

func (b *ProcessBackend) Attach(_ string, cols, rows int, writable bool) (io.ReadWriter, func(), func(int, int), error) {
	if b.Setsize != nil {
		b.Setsize(cols, rows)
	}
	resizeFn := func(c, r int) {
		if b.Setsize != nil {
			b.Setsize(c, r)
		}
	}
	// Belt-and-suspenders: for read-only grants, wrap the PTY in a write-blocking
	// reader so that even if serveViewer's writable-gate regressed, no keystroke
	// can reach the live pi process PTY. TmuxBackend achieves the same effect via
	// tmux attach -r; ProcessBackend has no equivalent OS-level flag, so we
	// enforce it here. Reads (output to viewer) are unaffected.
	var rw io.ReadWriter = b.PTY
	if !writable {
		rw = &readOnlyWrapper{Reader: b.PTY}
	}
	// cleanup is a no-op: the caller owns the PTY fd lifetime.
	return rw, func() {}, resizeFn, nil
}

// readOnlyWrapper wraps an io.Reader and makes Write a no-op error, providing
// a backend-level read-only lock for ProcessBackend RO grants.
type readOnlyWrapper struct{ io.Reader }

func (r *readOnlyWrapper) Write([]byte) (int, error) {
	return 0, fmt.Errorf("write to read-only PTY disallowed")
}

// Responder attaches to targets on request and streams the PTY over NATS. It
// runs on the pinard host (daemon-managed, TmuxBackend) and on standalone/HPC
// worker hosts — both as `aoc webterm-responder` (TmuxBackend) and as a
// worker-self-served responder bridging the pi PTY directly (ProcessBackend).
type Responder struct {
	NC          *nats.Conn
	Vignoble    string
	GrantSecret []byte
	MaxViewers  int
	IdleTimeout time.Duration

	// Backend determines how PTYs are acquired. Defaults to a TmuxBackend
	// using Socket (or "pinard-<Vignoble>") when nil.
	Backend PTYBackend
	Socket  string // tmux socket name; only used by the default TmuxBackend

	// KV is used to resolve a session's parcelle for interrupt routing.
	// Optional: if nil (or if the session is not found), interrupt is a no-op
	// (logged). With KV wired this should not be reached for live sessions.
	KV           pnats.KVReader
	AgentsBucket string // defaults to "pinard-agents"

	active int32 // atomic count of live viewers
}

func (r *Responder) backend() PTYBackend {
	if r.Backend != nil {
		return r.Backend
	}
	socket := r.Socket
	if socket == "" {
		socket = "pinard-" + r.Vignoble
	}
	return &TmuxBackend{Socket: socket, Vignoble: r.Vignoble}
}

func (r *Responder) agentsBucket() string {
	if r.AgentsBucket != "" {
		return r.AgentsBucket
	}
	return "pinard-agents"
}

// resolveInterruptSubject looks up the parcelle for a session name via the
// pinard-agents KV bucket, then returns the NATS interrupt subject. Returns
// empty string if resolution fails (caller should fall back).
func (r *Responder) resolveInterruptSubject(sessionName string) string {
	if r.KV == nil {
		return ""
	}
	keys, err := r.KV.Keys(r.agentsBucket())
	if err != nil {
		return ""
	}
	for _, key := range keys {
		data, err := r.KV.Get(r.agentsBucket(), key)
		if err != nil || data == nil {
			continue
		}
		name, _ := data["name"].(string)
		if name != sessionName {
			continue
		}
		parcelle, _ := data["parcelle"].(string)
		if parcelle == "" {
			continue
		}
		return pnats.AgentInterruptSubject(r.Vignoble, parcelle, sessionName)
	}
	return ""
}

func (r *Responder) maxViewers() int {
	if r.MaxViewers > 0 {
		return r.MaxViewers
	}
	return 8
}

func (r *Responder) idleTimeout() time.Duration {
	if r.IdleTimeout > 0 {
		return r.IdleTimeout
	}
	return 10 * time.Minute
}

// Run subscribes to the request subject and serves viewers until ctx is done.
func (r *Responder) Run(ctx context.Context) error {
	if r.NC == nil {
		return fmt.Errorf("webterm responder: nil NATS connection")
	}
	if len(r.GrantSecret) == 0 {
		return fmt.Errorf("webterm responder: no grant secret")
	}
	sub, err := r.NC.Subscribe(ReqSubject(r.Vignoble), func(m *nats.Msg) {
		r.handleRequest(ctx, m)
	})
	if err != nil {
		return fmt.Errorf("webterm responder subscribe: %w", err)
	}

	// ListSubject is only meaningful for TmuxBackend (control-room enumeration).
	// ProcessBackend must not subscribe: it would reply with an empty list and race
	// the daemon's full reply, causing the control-room to flicker (#270).
	var listSub *nats.Subscription
	if r.backend().EnumeratesSessions() {
		listSub, err = r.NC.Subscribe(ListSubject(r.Vignoble), func(m *nats.Msg) {
			r.handleList(m)
		})
		if err != nil {
			_ = sub.Unsubscribe()
			return fmt.Errorf("webterm responder list subscribe: %w", err)
		}
	}
	log.Printf("[webterm] responder listening on %s", ReqSubject(r.Vignoble))
	<-ctx.Done()
	_ = sub.Unsubscribe()
	if listSub != nil {
		_ = listSub.Unsubscribe()
	}
	return ctx.Err()
}

func (r *Responder) reply(m *nats.Msg, ok bool, reason string) {
	if m.Reply == "" {
		return
	}
	data, _ := json.Marshal(ReqReply{OK: ok, Reason: reason})
	_ = m.Respond(data)
}

func (r *Responder) handleRequest(ctx context.Context, m *nats.Msg) {
	var req ReqMsg
	if err := json.Unmarshal(m.Data, &req); err != nil {
		r.reply(m, false, "bad request")
		return
	}
	// Verify the gateway grant BEFORE attaching — the sole trust gate host-side.
	// On failure stay SILENT (do not reply): the request is broadcast to every
	// responder on this subject, so a responder that cannot verify the grant (a
	// stale process with an old secret, or another tenant's responder) must not
	// answer — otherwise its rejection could win the reply race against the
	// legitimate responder and surface a spurious "unauthorized". A genuinely bad
	// grant simply gets no reply → the gateway times out ("session not found").
	grant, err := VerifyGrant(req.Grant, r.GrantSecret, time.Now())
	if err != nil {
		log.Printf("[webterm] ignoring request with unverifiable grant (viewer=%s): %v", req.ViewerID, err)
		return
	}
	// The grant is scoped to a vignoble; ignore grants for a different namespace
	// (a responder must only serve its own vignoble). Silent, same reasoning as an
	// unverifiable grant.
	if grant.Vignoble != "" && grant.Vignoble != r.Vignoble {
		log.Printf("[webterm] ignoring request for foreign vignoble %q (ours=%q)", grant.Vignoble, r.Vignoble)
		return
	}
	if req.ViewerID == "" {
		r.reply(m, false, "missing viewer id")
		return
	}

	// When this responder does not own the target, stay SILENT — do not reply.
	// The request is broadcast to every responder on this subject; a fast local
	// responder (e.g. the daemon TmuxBackend) that lacks the target must not send
	// a rejection, because its reply would win the race against the legitimate
	// remote/HPC responder (ProcessBackend) that does own the session.
	// Silent-on-missing is the same pattern used above for unverifiable grants and
	// foreign-vignoble requests. The gateway times out to "session not found" only
	// if no responder claims the target.
	if !r.backend().HasSession(grant.Target) {
		log.Printf("[webterm] no local session %q — staying silent (another responder may own it)", grant.Target)
		return
	}
	if int(atomic.LoadInt32(&r.active)) >= r.maxViewers() {
		r.reply(m, false, "too many viewers")
		return
	}

	// Accept, then start streaming.
	r.reply(m, true, "")
	atomic.AddInt32(&r.active, 1)
	go func() {
		defer atomic.AddInt32(&r.active, -1)
		r.serveViewer(ctx, req, grant)
	}()
}

// parseTarget splits a tmux target into a sanitized session + optional window.
// The two parts are sanitized separately so a legitimate `session:window` target
// (control-room windows) survives — SanitizeName maps ':' to '-'.
func parseTarget(t string) (base, window string) {
	b, w, ok := strings.Cut(t, ":")
	base = session.SanitizeName(b)
	if ok {
		window = session.SanitizeName(w)
	}
	return base, window
}

// handleList answers a control-room enumeration request: the vignoble's tmux
// sessions + the conductor's windows. Grant-verified; silent on failure (same as
// handleRequest) so a stale/foreign responder can't win the reply race.
func (r *Responder) handleList(m *nats.Msg) {
	if m.Reply == "" {
		return
	}
	var req ListReq
	if json.Unmarshal(m.Data, &req) != nil {
		return
	}
	grant, err := VerifyGrant(req.Grant, r.GrantSecret, time.Now())
	if err != nil {
		log.Printf("[webterm] ignoring list request with unverifiable grant: %v", err)
		return
	}
	if grant.Vignoble != "" && grant.Vignoble != r.Vignoble {
		return
	}
	data, _ := json.Marshal(ListReply{
		OK:       true,
		Sessions: r.tmuxSessions(),
		Windows:  r.tmuxConductorWindows(),
	})
	_ = m.Respond(data)
}

func (r *Responder) socket() string {
	if r.Socket != "" {
		return r.Socket
	}
	return "pinard-" + r.Vignoble
}

func (r *Responder) tmuxSessions() []string {
	out, err := exec.Command("tmux", "-L", r.socket(), "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return nil
	}
	var sessions []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			sessions = append(sessions, line)
		}
	}
	return sessions
}

func (r *Responder) tmuxConductorWindows() []WinInfo {
	out, err := exec.Command("tmux", "-L", r.socket(), "list-windows", "-t", "conductor", "-F", "#{window_index}:#{window_name}").Output()
	if err != nil {
		return nil // no conductor session on this host — fine
	}
	var wins []WinInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		idx, name, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(idx)
		wins = append(wins, WinInfo{Index: n, Name: name})
	}
	return wins
}

// serveViewer opens a PTY via the configured backend and bridges it to the
// viewer's per-viewer NATS subjects until the browser disconnects, the session
// goes idle, or the PTY EOF. Read-only unless the grant is ModeRW.
func (r *Responder) serveViewer(ctx context.Context, req ReqMsg, grant Grant) {
	cols, rows := req.Cols, req.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	base, _ := parseTarget(grant.Target)
	writable := grant.Mode == ModeRW

	ptmx, cleanup, resizeFn, err := r.backend().Attach(grant.Target, cols, rows, writable)
	if err != nil {
		r.publishEnded(req.ViewerID, "attach failed")
		return
	}

	viewerCtx, cancel := context.WithCancel(ctx)
	var teardownOnce sync.Once
	teardown := func() {
		teardownOnce.Do(func() {
			cancel()
			cleanup()
		})
	}
	defer teardown()

	// Idle tracking: reset on each gateway heartbeat/ctl message.
	lastSeen := time.Now()
	var idleMu sync.Mutex
	touch := func() { idleMu.Lock(); lastSeen = time.Now(); idleMu.Unlock() }

	// Control channel (gateway → responder): resize / close / heartbeat.
	ctlSub, err := r.NC.Subscribe(CtlSubject(r.Vignoble, req.ViewerID), func(m *nats.Msg) {
		touch()
		var c CtlMsg
		if json.Unmarshal(m.Data, &c) != nil {
			return
		}
		switch c.Type {
		case CtlResize:
			if c.Cols > 0 && c.Rows > 0 {
				resizeFn(c.Cols, c.Rows)
			}
		case CtlClose:
			teardown()
		case CtlInterrupt:
			r.handleInterrupt(base, grant, req.ViewerID)
		}
	})
	if err != nil {
		return
	}
	defer ctlSub.Unsubscribe() //nolint:errcheck

	// Input channel (gateway → responder): keystrokes, only for a writable grant.
	// The gateway only forwards input when it minted a ModeRW grant, and we honor
	// it only when this grant is ModeRW — single-writer steer, gated both ends.
	if writable {
		inSub, ierr := r.NC.Subscribe(InSubject(r.Vignoble, req.ViewerID), func(m *nats.Msg) {
			touch()
			_, _ = ptmx.Write(m.Data)
		})
		if ierr == nil {
			defer inSub.Unsubscribe() //nolint:errcheck
		}
	}

	// Idle watchdog.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-viewerCtx.Done():
				return
			case <-t.C:
				idleMu.Lock()
				idle := time.Since(lastSeen)
				idleMu.Unlock()
				if idle > r.idleTimeout() {
					r.publishEnded(req.ViewerID, "idle timeout")
					teardown()
					return
				}
			}
		}
	}()

	outSubject := OutSubject(r.Vignoble, req.ViewerID)
	r.pump(viewerCtx, ptmx, outSubject)

	// pump returned → PTY EOF (target exited or attach ended).
	r.publishEnded(req.ViewerID, "session ended")
}

// pump reads PTY output and publishes it with coalescing, a bounded buffer, and
// a per-second rate cap.
func (r *Responder) pump(ctx context.Context, ptmx io.Reader, outSubject string) {
	var mu sync.Mutex
	pending := make([]byte, 0, maxBufBytes)
	dropped := false

	// Reader goroutine appends into the bounded buffer.
	readErr := make(chan struct{})
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				pending = append(pending, buf[:n]...)
				if len(pending) > maxBufBytes {
					// Drop oldest to keep the buffer bounded; mark once.
					over := len(pending) - maxBufBytes
					pending = pending[over:]
					dropped = true
				}
				mu.Unlock()
			}
			if err != nil {
				close(readErr)
				return
			}
		}
	}()

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	ticksPerSec := int(time.Second / flushInterval)
	if ticksPerSec < 1 {
		ticksPerSec = 1
	}
	maxPerTick := rateCapPerSec / ticksPerSec

	flush := func() {
		mu.Lock()
		if dropped {
			pending = append([]byte(throttleMark), pending...)
			dropped = false
		}
		n := len(pending)
		if n > maxPerTick {
			n = maxPerTick
		}
		// Copy into a fresh backing array rather than aliasing pending's: take is
		// published below after the lock is released, while the reader goroutine
		// keeps appending to (and, once drained, resetting/reusing the backing
		// array of) pending concurrently — sharing the array here was a data race
		// (the in-flight Publish could read bytes the reader had already
		// overwritten).
		take := make([]byte, n)
		copy(take, pending[:n])
		pending = append(pending[:0], pending[n:]...)
		mu.Unlock()
		for len(take) > 0 {
			chunk := take
			if len(chunk) > maxMsgBytes {
				chunk = take[:maxMsgBytes]
			}
			_ = r.NC.Publish(outSubject, chunk)
			take = take[len(chunk):]
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-readErr:
			flush() // drain remaining output
			return
		case <-ticker.C:
			flush()
		}
	}
}

func (r *Responder) publishEnded(viewerID, reason string) {
	data, _ := json.Marshal(EvtMsg{Type: EvtEnded, Reason: reason})
	_ = r.NC.Publish(EvtSubject(r.Vignoble, viewerID), data)
}

// handleInterrupt sends the interrupt signal to the agent attached to base
// (the session name). It publishes to the agent's NATS interrupt subject
// (resolved via the pinard-agents KV); on failure it is a no-op (logged).
func (r *Responder) handleInterrupt(base string, grant Grant, viewerID string) {
	log.Printf("[webterm-audit] action=interrupt viewer=%s vignoble=%s target=%s mode=%s",
		viewerID, r.Vignoble, grant.Target, grant.Mode)

	subject := r.resolveInterruptSubject(base)
	if subject != "" {
		_ = r.NC.Publish(subject, []byte(`{}`))
		return
	}
	// KV lookup failed (nil KV or session not found). Log and no-op: the designed
	// interrupt mechanism is a NATS publish to the agent's .interrupt subject (same
	// as the conductor's interrupt_worker). A destructive tmux send-keys C-c could
	// terminate the agent process rather than cleanly cancelling the current turn,
	// which is unsafe for read-only capsule viewers. With KV wired this path should
	// not be reached for live sessions.
	log.Printf("[webterm] interrupt: could not resolve NATS subject for session %q (KV unavailable or session not found) — no-op", base)
}
