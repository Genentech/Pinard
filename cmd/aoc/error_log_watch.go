package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/pnats"
	"github.com/nats-io/nats.go"
)

// errorLogWatcher writes a durable, append-only record of abnormal turn ends
// to the daemon log (#337). The pinard-agents KV's lastError/erroredAt fields
// are transient — the worker clears them optimistically on the next
// turn_start (#323) — so without this watcher an errored turn leaves no trace
// once the agent starts a new turn. It watches the pinard-agents KV bucket
// continuously; on a transition into tempo=errored it logs exactly one line,
// keyed on erroredAt so a persistently errored agent doesn't re-log on every
// KV touch, and a distinct new error after recovery logs again.
type errorLogWatcher struct {
	Vignoble *config.Vignoble

	// watch is injected so the reconnect loop can be exercised without a real
	// NATS connection; production wires it to kv.WatchAll("pinard-agents", ...).
	watch func(ctx context.Context) (nats.KeyWatcher, error)

	mu     sync.Mutex
	logged map[string]string // KV key -> last-logged erroredAt
}

func newErrorLogWatcher(kv *pnats.KV, vb *config.Vignoble) *errorLogWatcher {
	return &errorLogWatcher{
		Vignoble: vb,
		watch: func(ctx context.Context) (nats.KeyWatcher, error) {
			return kv.WatchAll("pinard-agents", nats.Context(ctx))
		},
		logged: make(map[string]string),
	}
}

// Run watches pinard-agents until ctx is canceled, re-subscribing on
// disconnect/expiry. Mirrors blockedGraceWatcher.Run's reconnect pattern.
func (w *errorLogWatcher) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		watchCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		watcher, err := w.watch(watchCtx)
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
			w.handle(entry)
		}
		watcher.Stop()
		cancel()

		if ctx.Err() != nil {
			return
		}
		time.Sleep(time.Second)
	}
}

// handle inspects a single KV entry and logs exactly one line per new
// tempo=errored episode (keyed on erroredAt), scoped to this daemon's
// vignoble.
func (w *errorLogWatcher) handle(entry nats.KeyValueEntry) {
	key := entry.Key()

	if entry.Operation() == nats.KeyValueDelete || entry.Operation() == nats.KeyValuePurge {
		w.forget(key)
		return
	}

	var data map[string]any
	if err := json.Unmarshal(entry.Value(), &data); err != nil {
		return
	}

	if vb, _ := data["vignoble"].(string); vb != "" && vb != w.Vignoble.Name {
		return
	}

	tempo, _ := data["tempo"].(string)
	erroredAt, _ := data["erroredAt"].(string)
	if tempo != "errored" || erroredAt == "" {
		// Not currently errored (including the optimistic clear on the next
		// turn_start) — forget so a later recurrence, even one that happens to
		// reuse a stale erroredAt, is treated as fresh.
		w.forget(key)
		return
	}

	if !w.shouldLog(key, erroredAt) {
		return
	}

	name, _ := data["name"].(string)
	if name == "" {
		name = key
	}
	runID, _ := data["runId"].(string)
	vignoble, _ := data["vignoble"].(string)
	if vignoble == "" {
		vignoble = w.Vignoble.Name
	}
	project, _ := data["project"].(string)
	parcelle, _ := data["parcelle"].(string)
	lastError, _ := data["lastError"].(string)

	log.Printf("[error-log] agent=%s key=%s runId=%s vignoble=%s project=%s parcelle=%s erroredAt=%s lastError=%q",
		name, key, runID, vignoble, project, parcelle, erroredAt, lastError)
}

// shouldLog reports whether erroredAt is new for key, recording it as logged
// if so.
func (w *errorLogWatcher) shouldLog(key, erroredAt string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.logged[key] == erroredAt {
		return false
	}
	w.logged[key] = erroredAt
	return true
}

func (w *errorLogWatcher) forget(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.logged, key)
}
