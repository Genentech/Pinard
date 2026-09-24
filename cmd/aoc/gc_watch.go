package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/pnats"
	"github.com/Genentech/pinard/internal/session"
	"github.com/Genentech/pinard/internal/state"
	"github.com/nats-io/nats.go"
)

// blockedGraceWatcher reaps ephemeral/scheduled workers shortly after their
// turn ends, instead of waiting for the next full GC sweep. It watches the
// pinard-agents KV bucket continuously; when a worker transitions to
// tempo=blocked it schedules a grace-period timer, canceling it if the
// worker resumes (tempo=active) or its entry disappears before the grace
// elapses. This is deliberately NOT keyed on lastSeen (heartbeats keep that
// fresh regardless of tempo) — tempo is the reliable done-signal.
type blockedGraceWatcher struct {
	KV       pnats.KVWriter
	Vignoble *config.Vignoble
	MRState  *state.Store[state.MRWatcherState]
	Grace    time.Duration

	// watch is injected so the reconnect loop can be exercised without a real
	// NATS connection; production wires it to kv.WatchAll("pinard-agents", ...).
	watch func(ctx context.Context) (nats.KeyWatcher, error)

	mu      sync.Mutex
	pending map[string]*time.Timer
}

func newBlockedGraceWatcher(kv *pnats.KV, vb *config.Vignoble, mrState *state.Store[state.MRWatcherState], grace time.Duration) *blockedGraceWatcher {
	return &blockedGraceWatcher{
		KV:       kv,
		Vignoble: vb,
		MRState:  mrState,
		Grace:    grace,
		watch: func(ctx context.Context) (nats.KeyWatcher, error) {
			return kv.WatchAll("pinard-agents", nats.Context(ctx))
		},
		pending: make(map[string]*time.Timer),
	}
}

// Run watches pinard-agents until ctx is canceled, re-subscribing on
// disconnect/expiry. Mirrors internal/dashboard/panel_workers.go's runKVWatcher
// reconnect pattern (10-minute context per subscription, defense-in-depth
// against an orphaned consumer over a flaky connection).
func (b *blockedGraceWatcher) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			b.cancelAll()
			return
		}

		watchCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		watcher, err := b.watch(watchCtx)
		if err != nil {
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		for entry := range watcher.Updates() {
			if entry == nil {
				continue // end-of-snapshot sentinel
			}
			b.handle(entry)
		}
		watcher.Stop()
		cancel()

		if ctx.Err() != nil {
			return
		}
		time.Sleep(time.Second)
	}
}

// handle applies a single KV entry: schedules, resets, or cancels the
// per-worker grace timer.
func (b *blockedGraceWatcher) handle(entry nats.KeyValueEntry) {
	key := entry.Key()

	if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
		b.cancel(key)
		return
	}

	var data map[string]any
	if err := json.Unmarshal(entry.Value(), &data); err != nil {
		return
	}

	if vb, _ := data["vignoble"].(string); vb != "" && vb != b.Vignoble.Name {
		return
	}
	name, _ := data["name"].(string)
	if name == "" {
		name = key
	}
	if name == "conductor" || session.IsReservedWindow(name) {
		return
	}

	tempo, _ := data["tempo"].(string)
	if tempo == "active" {
		// Multi-turn job resumed — reset the grace clock.
		b.cancel(key)
		return
	}

	ephemeral, _ := data["ephemeral"].(bool)
	if !ephemeral && !scheduleNameRe.MatchString(name) {
		return
	}
	if hasOpenMR(b.MRState, key, name) {
		b.cancel(key)
		return
	}
	if tempo != "blocked" {
		return
	}

	b.schedule(key)
}

func (b *blockedGraceWatcher) schedule(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.pending[key]; ok {
		return
	}
	b.pending[key] = time.AfterFunc(b.Grace, func() { b.fire(key) })
}

func (b *blockedGraceWatcher) cancel(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.pending[key]; ok {
		t.Stop()
		delete(b.pending, key)
	}
}

func (b *blockedGraceWatcher) cancelAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, t := range b.pending {
		t.Stop()
		delete(b.pending, key)
	}
}

// fire re-verifies the worker is still blocked/ephemeral/without an open MR
// before reaping — defends against a race between the grace timer firing and
// a KV update that arrived just before it did.
func (b *blockedGraceWatcher) fire(key string) {
	b.mu.Lock()
	delete(b.pending, key)
	b.mu.Unlock()

	data, err := b.KV.Get("pinard-agents", key)
	if err != nil || data == nil {
		return // already gone
	}
	if vb, _ := data["vignoble"].(string); vb != "" && vb != b.Vignoble.Name {
		return
	}
	name, _ := data["name"].(string)
	if name == "" {
		name = key
	}
	tempo, _ := data["tempo"].(string)
	ephemeral, _ := data["ephemeral"].(bool)
	if (!ephemeral && !scheduleNameRe.MatchString(name)) || tempo != "blocked" {
		return
	}
	if hasOpenMR(b.MRState, key, name) {
		return
	}

	agentVignoble, _ := data["vignoble"].(string)
	effectiveVignoble := agentVignoble
	if effectiveVignoble == "" {
		effectiveVignoble = b.Vignoble.Name
	}
	project, _ := data["project"].(string)

	result := &gcResult{}
	reason := fmt.Sprintf("ephemeral/scheduled turn ended (tempo=blocked, grace=%s)", b.Grace.Round(time.Second))
	reapAgentNow(b.KV, b.Vignoble, key, name, effectiveVignoble, project, reason, false, result)
	if len(result.reaped) > 0 {
		log.Printf("[gc-watch] reaped %d worker(s)", len(result.reaped))
	}
}

// runGCBackstopLoop runs the low-frequency backstop sweep, gated by a
// persisted last-run timestamp so it doesn't re-fire on every daemon restart
// within the interval. On first-ever run (no recorded timestamp) it runs
// immediately.
func runGCBackstopLoop(ctx context.Context, kv *pnats.KV, vb *config.Vignoble) {
	gcState, err := state.Load[state.GCState](filepath.Join(vb.StateDir, "gc-state.yaml"))
	if err != nil {
		log.Printf("[gc-backstop] state load failed: %v", err)
		return
	}

	for {
		var last time.Time
		gcState.Read(func(s *state.GCState) {
			if s.LastGC != "" {
				last, _ = time.Parse(time.RFC3339, s.LastGC)
			}
		})

		wait := time.Duration(0)
		if !last.IsZero() {
			if elapsed := time.Since(last); elapsed < gcBackstopInterval {
				wait = gcBackstopInterval - elapsed
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if ctx.Err() != nil {
			return
		}

		result := runGCSweep(kv, vb, gcSweepOptions{
			VignobleFilter: vb.Name,
			Grace:          defaultGCGrace,
			Backstop:       true,
		})
		gcState.Update(func(s *state.GCState) {
			s.LastGC = time.Now().UTC().Format(time.RFC3339)
		})
		log.Printf("[gc-backstop] swept: %d reaped, %d sockets removed", len(result.reaped), len(result.sockets))
	}
}
