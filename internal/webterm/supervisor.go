package webterm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Genentech/pinard/internal/session"
	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
)

// replayCapBytes bounds ptyFanout's replay buffer: enough to repaint a
// full-screen TUI (or a modest amount of scrollback) for a newly attached
// viewer, without letting memory grow unbounded on a long-running worker.
const replayCapBytes = 128 * 1024

// ptyFanout ensures there is exactly one physical reader of a PTY master,
// broadcasting each chunk it reads to every current subscriber. Without this,
// the local terminal mirror and any attached viewer (via ProcessBackend) would
// each call Read() directly on the same underlying fd, splitting the output
// stream unpredictably between them instead of each seeing a full copy.
type ptyFanout struct {
	ptmx *os.File

	mu     sync.Mutex
	nextID int
	subs   map[int]chan []byte
	// replay holds up to replayCapBytes of the most recently broadcast output,
	// so a newly subscribed viewer can be repainted immediately instead of
	// seeing a blank screen until the child next writes.
	replay []byte
}

func newPTYFanout(ptmx *os.File) *ptyFanout {
	return &ptyFanout{ptmx: ptmx, subs: map[int]chan []byte{}}
}

// run reads the master until it is exhausted (the child exited and every
// slave-side fd has been closed), broadcasting each chunk to every current
// subscriber, then closes all subscriber channels. Must be started in its own
// goroutine and is the sole reader of ptmx.
func (f *ptyFanout) run() {
	buf := make([]byte, 32*1024)
	for {
		n, err := f.ptmx.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			f.broadcast(chunk)
		}
		if err != nil {
			f.closeAll()
			return
		}
	}
}

func (f *ptyFanout) broadcast(chunk []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replay = append(f.replay, chunk...)
	if over := len(f.replay) - replayCapBytes; over > 0 {
		f.replay = f.replay[over:]
	}
	for _, ch := range f.subs {
		select {
		case ch <- chunk:
		default:
			// A stalled subscriber must never block the shared reader (that
			// would stall every other subscriber too). The responder's own
			// pump() already coalesces/rate-caps/drops for viewers; losing a
			// chunk here just means a brief gap under extreme backpressure.
		}
	}
}

func (f *ptyFanout) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, ch := range f.subs {
		close(ch)
		delete(f.subs, id)
	}
}

// subscribe registers a new independent reader of the broadcast stream and
// returns an io.ReadWriteCloser: reads drain the subscription, writes go
// straight through to the real master (there is only ever one physical
// writer target, so writes need no fan-out).
func (f *ptyFanout) subscribe() io.ReadWriteCloser {
	f.mu.Lock()
	id := f.nextID
	f.nextID++
	ch := make(chan []byte, 256)
	// Seed the replay buffer before this channel is exposed to broadcast() by
	// inserting it into f.subs, so the replayed bytes are strictly ordered
	// ahead of anything broadcast() could deliver concurrently.
	if len(f.replay) > 0 {
		ch <- append([]byte(nil), f.replay...)
	}
	f.subs[id] = ch
	f.mu.Unlock()
	return &ptyFanoutSub{f: f, id: id, ch: ch}
}

type ptyFanoutSub struct {
	f   *ptyFanout
	id  int
	ch  chan []byte
	buf []byte
}

func (s *ptyFanoutSub) Read(p []byte) (int, error) {
	for len(s.buf) == 0 {
		chunk, ok := <-s.ch
		if !ok {
			return 0, io.EOF
		}
		s.buf = chunk
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func (s *ptyFanoutSub) Write(p []byte) (int, error) {
	return s.f.ptmx.Write(p)
}

func (s *ptyFanoutSub) Close() error {
	s.f.mu.Lock()
	delete(s.f.subs, s.id)
	s.f.mu.Unlock()
	return nil
}

// supervisedBackend implements PTYBackend by handing each attaching viewer an
// independent fan-out subscription of the supervised child's PTY. A plain
// ProcessBackend reuses a single shared PTY field for every Attach() call,
// which is only safe for one reader at a time; here Attach() creates a fresh
// subscription per call, so concurrent viewers (and the local terminal
// mirror) each get a full, uncorrupted copy of the output stream instead of
// racing to Read() the same object.
type supervisedBackend struct {
	target  string
	fanout  *ptyFanout
	setsize func(cols, rows int)

	// viewers and restoreLocalSize implement the "viewer-attached wins" PTY
	// size policy documented on RunSupervised's syncSize: while any viewer is
	// attached, restoreLocalSize's caller (syncSize) refuses to re-apply the
	// local controlling terminal's size, and Attach's cleanup calls
	// restoreLocalSize once the last viewer detaches to resync the PTY back
	// to the local terminal. Both may be nil (e.g. in tests).
	viewers          *int32
	restoreLocalSize func()
}

func (b *supervisedBackend) EnumeratesSessions() bool { return false }

func (b *supervisedBackend) HasSession(target string) bool {
	base, _ := parseTarget(target)
	return session.SanitizeName(base) == session.SanitizeName(b.target)
}

func (b *supervisedBackend) Attach(_ string, cols, rows int, writable bool) (io.ReadWriter, func(), func(int, int), error) {
	if b.viewers != nil {
		atomic.AddInt32(b.viewers, 1)
	}
	if b.setsize != nil {
		b.setsize(cols, rows)
	}
	sub := b.fanout.subscribe()
	var rw io.ReadWriter = sub
	if !writable {
		// Belt-and-suspenders, matching ProcessBackend: a RO grant must not be
		// able to write to the child even if the writable-gate upstream regressed.
		rw = &readOnlyWrapper{Reader: sub}
	}
	cleanup := func() {
		_ = sub.Close()
		if b.viewers != nil && atomic.AddInt32(b.viewers, -1) == 0 && b.restoreLocalSize != nil {
			b.restoreLocalSize()
		}
	}
	resizeFn := func(c, r int) {
		if b.setsize != nil {
			b.setsize(c, r)
		}
	}
	return rw, cleanup, resizeFn, nil
}

// RunSupervised launches cmdArgs (e.g. pi + its full arg list) on a new PTY
// slave and mirrors it to this process's real controlling terminal (stdin ->
// child, child -> stdout), so an operator attached to the supervisor's own TTY
// (tmux, ssh, a babysitter-allocated pty, …) sees and drives the child exactly
// as if it had been exec'd directly.
//
// When resp is non-nil, the same PTY master is wired into a ProcessBackend and
// served for the lifetime of the child, so browser viewers get the identical
// stream (via an independent fan-out subscription, so a viewer never steals
// bytes from the local terminal mirror or vice versa). This is the fix for
// daemon-less/HPC workers: the responder must own the PTY master with the
// child running on the slave, never a stray fd handed in from elsewhere.
//
// RunSupervised blocks until the child exits and returns its exit code.
func RunSupervised(ctx context.Context, cmdArgs []string, resp *Responder, target string) (int, error) {
	if len(cmdArgs) == 0 {
		return 0, fmt.Errorf("webterm supervisor: empty command")
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = os.Environ()

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return 0, fmt.Errorf("webterm supervisor: start child: %w", err)
	}

	// Bind the controlling terminal once into a local rather than repeatedly
	// reading the mutable global os.Stdin: the supervisor's controlling
	// terminal shouldn't be able to change identity mid-run, and this also
	// avoids racing with anything that reassigns os.Stdin (e.g. test harnesses)
	// concurrently with syncSize firing from viewer-detach cleanup.
	tty := os.Stdin

	// viewers counts browser viewers currently attached via supervisedBackend.
	// PTY size arbitration policy (explicit decision, not an ordering accident):
	// "viewer-attached wins". While viewers > 0, syncSize below (driven by the
	// local controlling terminal's own SIGWINCH) is suspended, so a viewer's
	// size cannot be clobbered by an unrelated local resize (e.g. the
	// run.sh/Singularity TTY on an HPC host). When the last viewer detaches,
	// supervisedBackend.Attach's cleanup calls syncSize once to resync the PTY
	// back to the local terminal's size. Among multiple concurrent viewers,
	// whichever resizes most recently wins, same as before this change.
	var viewers int32

	// Seed the child's PTY size from the real controlling terminal, and keep it
	// in sync for the life of the child, except while a viewer is attached.
	syncSize := func() {
		if atomic.LoadInt32(&viewers) > 0 {
			return
		}
		if size, err := pty.GetsizeFull(tty); err == nil {
			_ = pty.Setsize(ptmx, size)
		}
	}
	syncSize()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			syncSize()
		}
	}()

	// Raw mode on the real terminal so control sequences (Ctrl-C, arrow keys, …)
	// pass through to the child's own line discipline unmodified, instead of
	// being consumed by this process's terminal driver.
	var rawState *term.State
	if term.IsTerminal(tty.Fd()) {
		rawState, _ = term.MakeRaw(tty.Fd())
	}

	fanout := newPTYFanout(ptmx)
	fanoutDone := make(chan struct{})
	go func() {
		fanout.run()
		close(fanoutDone)
	}()

	localSub := fanout.subscribe()
	localDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(os.Stdout, localSub)
		close(localDone)
	}()
	go func() { _, _ = io.Copy(ptmx, tty) }()

	respCtx, cancelResp := context.WithCancel(ctx)
	var respWG sync.WaitGroup
	if resp != nil {
		resp.Backend = &supervisedBackend{
			target: target,
			fanout: fanout,
			setsize: func(cols, rows int) {
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
			},
			viewers:          &viewers,
			restoreLocalSize: syncSize,
		}
		respWG.Add(1)
		go func() {
			defer respWG.Done()
			_ = resp.Run(respCtx)
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			if s, ok := sig.(syscall.Signal); ok {
				_ = cmd.Process.Signal(s)
			}
		}
	}()

	waitErr := cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)

	// Drain whatever output is already sitting in the pty before tearing
	// anything down: fanout.run() is the sole reader of ptmx and will observe
	// a natural EOF once the child's exit has released the last slave-side fd
	// (pty.Start already closed the parent's own slave dup). Closing ptmx
	// ourselves before this completes would truncate the last bytes out from
	// under fanout.run()'s in-flight Read().
	select {
	case <-fanoutDone:
	case <-time.After(2 * time.Second):
	}
	select {
	case <-localDone:
	case <-time.After(2 * time.Second):
	}

	cancelResp()
	respWG.Wait()
	_ = ptmx.Close()

	if rawState != nil {
		_ = term.Restore(tty.Fd(), rawState)
	}

	if waitErr == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code, nil
		}
		// Negative exit code: the child was terminated by a signal.
		return 1, nil
	}
	return 1, waitErr
}
