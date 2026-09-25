package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/state"
	"github.com/nats-io/nats.go"
)

// mockKVStore implements pnats.KVWriter for testing, mirroring
// internal/watcher/dispatch_test.go's helper of the same shape.
type mockKVStore struct {
	data map[string]map[string]map[string]any
}

func newMockKV() *mockKVStore {
	return &mockKVStore{data: make(map[string]map[string]map[string]any)}
}

func (m *mockKVStore) Get(bucket, key string) (map[string]any, error) {
	if m.data[bucket] == nil {
		return nil, nil
	}
	return m.data[bucket][key], nil
}

func (m *mockKVStore) Keys(bucket string) ([]string, error) {
	if m.data[bucket] == nil {
		return nil, nil
	}
	keys := make([]string, 0, len(m.data[bucket]))
	for k := range m.data[bucket] {
		keys = append(keys, k)
	}
	return keys, nil
}

func (m *mockKVStore) Put(bucket, key string, value any) error {
	if m.data[bucket] == nil {
		m.data[bucket] = make(map[string]map[string]any)
	}
	if v, ok := value.(map[string]any); ok {
		m.data[bucket][key] = v
	}
	return nil
}

func (m *mockKVStore) Del(bucket, key string) error {
	if m.data[bucket] != nil {
		delete(m.data[bucket], key)
	}
	return nil
}

func (m *mockKVStore) setAgent(key string, data map[string]any) {
	m.Put("pinard-agents", key, data)
}

// fakeKVEntry implements nats.KeyValueEntry for feeding synthetic updates
// into blockedGraceWatcher.handle without a real NATS connection.
type fakeKVEntry struct {
	key   string
	value []byte
	op    nats.KeyValueOp
}

func (f *fakeKVEntry) Bucket() string             { return "pinard-agents" }
func (f *fakeKVEntry) Key() string                { return f.key }
func (f *fakeKVEntry) Value() []byte              { return f.value }
func (f *fakeKVEntry) Revision() uint64           { return 1 }
func (f *fakeKVEntry) Created() time.Time         { return time.Now() }
func (f *fakeKVEntry) Delta() uint64              { return 0 }
func (f *fakeKVEntry) Operation() nats.KeyValueOp { return f.op }

func putEntry(key, json string) *fakeKVEntry {
	return &fakeKVEntry{key: key, value: []byte(json), op: nats.KeyValuePut}
}

func delEntry(key string) *fakeKVEntry {
	return &fakeKVEntry{key: key, op: nats.KeyValueDelete}
}

func newTestWatcher(t *testing.T, kv *mockKVStore) *blockedGraceWatcher {
	t.Helper()
	dir := t.TempDir()
	mrState, err := state.Load[state.MRWatcherState](filepath.Join(dir, "mr-watcher.yaml"))
	if err != nil {
		t.Fatalf("load mr state: %v", err)
	}
	return &blockedGraceWatcher{
		KV:       kv,
		Vignoble: &config.Vignoble{Name: "testv", StateDir: dir, Config: &config.VignobleConfig{}},
		MRState:  mrState,
		Grace:    10 * time.Millisecond,
		pending:  make(map[string]*time.Timer),
	}
}

func TestBlockedGraceWatcher_SchedulesTimerOnBlocked(t *testing.T) {
	kv := newMockKV()
	w := newTestWatcher(t, kv)

	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","ephemeral":true,"tempo":"blocked"}`))

	w.mu.Lock()
	_, pending := w.pending["w1"]
	w.mu.Unlock()
	if !pending {
		t.Fatal("expected a pending grace timer for a blocked ephemeral worker")
	}
}

func TestBlockedGraceWatcher_ActiveCancelsTimer(t *testing.T) {
	kv := newMockKV()
	w := newTestWatcher(t, kv)

	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","ephemeral":true,"tempo":"blocked"}`))
	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","ephemeral":true,"tempo":"active"}`))

	w.mu.Lock()
	_, pending := w.pending["w1"]
	w.mu.Unlock()
	if pending {
		t.Fatal("expected tempo=active to cancel the pending grace timer")
	}
}

func TestBlockedGraceWatcher_DeleteCancelsTimer(t *testing.T) {
	kv := newMockKV()
	w := newTestWatcher(t, kv)

	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","ephemeral":true,"tempo":"blocked"}`))
	w.handle(delEntry("w1"))

	w.mu.Lock()
	_, pending := w.pending["w1"]
	w.mu.Unlock()
	if pending {
		t.Fatal("expected KV delete to cancel the pending grace timer")
	}
}

func TestBlockedGraceWatcher_IgnoresNonEphemeralNonScheduled(t *testing.T) {
	kv := newMockKV()
	w := newTestWatcher(t, kv)

	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","ephemeral":false,"tempo":"blocked"}`))

	w.mu.Lock()
	_, pending := w.pending["w1"]
	w.mu.Unlock()
	if pending {
		t.Fatal("a non-ephemeral, non-scheduled-name worker must not get a grace timer")
	}
}

func TestBlockedGraceWatcher_IgnoresOtherVignoble(t *testing.T) {
	kv := newMockKV()
	w := newTestWatcher(t, kv)

	w.handle(putEntry("w1", `{"vignoble":"other","name":"w1","ephemeral":true,"tempo":"blocked"}`))

	w.mu.Lock()
	_, pending := w.pending["w1"]
	w.mu.Unlock()
	if pending {
		t.Fatal("a worker from another vignoble must not be scheduled")
	}
}

func TestBlockedGraceWatcher_OpenMRCancelsAndBlocksScheduling(t *testing.T) {
	kv := newMockKV()
	w := newTestWatcher(t, kv)
	w.MRState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			// State "" is the realistic "still open" shape (see cmd_gc_test.go's
			// TestGCBackstopWorkers_SkipsOpenMR comment) — not a placeholder.
			"w1": {Name: "w1", MR: 5, State: ""},
		}
	})

	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","ephemeral":true,"tempo":"blocked"}`))

	w.mu.Lock()
	_, pending := w.pending["w1"]
	w.mu.Unlock()
	if pending {
		t.Fatal("a worker with an open MR must not be scheduled for reap")
	}
}

func TestBlockedGraceWatcher_FireReapsAfterGrace(t *testing.T) {
	kv := newMockKV()
	kv.setAgent("w1", map[string]any{
		"vignoble":  "testv",
		"name":      "w1",
		"ephemeral": true,
		"tempo":     "blocked",
	})
	w := newTestWatcher(t, kv)

	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","ephemeral":true,"tempo":"blocked"}`))

	deadline := time.After(2 * time.Second)
	for {
		if data, _ := kv.Get("pinard-agents", "w1"); data == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("expected the worker's KV entry to be reaped after the grace period elapsed")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestBlockedGraceWatcher_FireSkipsIfNoLongerBlocked(t *testing.T) {
	kv := newMockKV()
	w := newTestWatcher(t, kv)

	// Schedule a timer, then let the underlying KV state move to active before
	// the timer fires — fire() must re-verify and skip.
	w.schedule("w1")
	kv.setAgent("w1", map[string]any{
		"vignoble":  "testv",
		"name":      "w1",
		"ephemeral": true,
		"tempo":     "active",
	})
	w.fire("w1")

	if data, _ := kv.Get("pinard-agents", "w1"); data == nil {
		t.Fatal("fire() must not reap a worker that is no longer tempo=blocked")
	}
}

// TestGCState_RoundTrips confirms the persisted backstop timestamp survives a
// load/update/reload cycle — the gate that keeps the backstop from re-firing
// on every daemon restart within the interval.
func TestGCState_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gc-state.yaml")

	gcState, err := state.Load[state.GCState](path)
	if err != nil {
		t.Fatalf("load gc state: %v", err)
	}
	if gcState.Data.LastGC != "" {
		t.Fatalf("expected empty LastGC on first load, got %q", gcState.Data.LastGC)
	}

	want := time.Now().UTC().Format(time.RFC3339)
	if err := gcState.Update(func(s *state.GCState) { s.LastGC = want }); err != nil {
		t.Fatalf("update gc state: %v", err)
	}

	reloaded, err := state.Load[state.GCState](path)
	if err != nil {
		t.Fatalf("reload gc state: %v", err)
	}
	if reloaded.Data.LastGC != want {
		t.Errorf("LastGC = %q, want %q", reloaded.Data.LastGC, want)
	}
}
