package pnats

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

type KV struct {
	client  *Client
	buckets map[string]nats.KeyValue
	mu      sync.Mutex
}

func NewKV(client *Client) *KV {
	return &KV{
		client:  client,
		buckets: make(map[string]nats.KeyValue),
	}
}

func (k *KV) getBucket(name string) (nats.KeyValue, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if kv, ok := k.buckets[name]; ok {
		return kv, nil
	}

	if err := k.client.Connect(); err != nil {
		return nil, err
	}

	kv, err := k.client.JS().KeyValue(name)
	if err != nil {
		return nil, err
	}
	k.buckets[name] = kv
	return kv, nil
}

// EnsureBucket creates the KV bucket if it does not already exist, and caches
// the handle. Safe to call repeatedly.
func (k *KV) EnsureBucket(name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.buckets[name]; ok {
		return nil
	}
	if err := k.client.Connect(); err != nil {
		return err
	}
	js := k.client.JS()
	kv, err := js.KeyValue(name)
	if err != nil {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: name})
		if err != nil {
			return err
		}
	}
	k.buckets[name] = kv
	return nil
}

func (k *KV) Get(bucket, key string) (map[string]any, error) {
	kv, err := k.getBucket(bucket)
	if err != nil {
		return nil, err
	}
	entry, err := kv.Get(key)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(entry.Value(), &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (k *KV) Put(bucket, key string, value any) error {
	kv, err := k.getBucket(bucket)
	if err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = kv.Put(key, data)
	return err
}

func (k *KV) Del(bucket, key string) error {
	kv, err := k.getBucket(bucket)
	if err != nil {
		return err
	}
	return kv.Delete(key)
}

// WatchAll returns a continuous watcher over every key in bucket, delivering
// the initial snapshot followed by live updates (a nil-value Put entry marks
// end-of-snapshot per the underlying nats.go semantics). Callers own the
// returned watcher's lifecycle (Stop()). See internal/dashboard/panel_workers.go
// for the recommended reconnect-on-expiry usage pattern.
func (k *KV) WatchAll(bucket string, opts ...nats.WatchOpt) (nats.KeyWatcher, error) {
	kv, err := k.getBucket(bucket)
	if err != nil {
		return nil, err
	}
	return kv.WatchAll(opts...)
}

// PurgeDeletes removes delete/purge markers older than olderThan from the
// named bucket. This prevents tombstones from accumulating in the JetStream
// stream that backs the bucket (e.g. from kv.Delete calls in orphan recovery
// and worker shutdown). Only markers are removed — live values are untouched.
func (k *KV) PurgeDeletes(bucket string, olderThan time.Duration) error {
	kv, err := k.getBucket(bucket)
	if err != nil {
		return err
	}
	return kv.PurgeDeletes(nats.DeleteMarkersOlderThan(olderThan))
}

// NumSubjects returns the number of subjects (live + tombstoned) in the
// JetStream stream backing the named KV bucket. Useful for observing compaction
// progress: call before and after PurgeDeletes to log the delta.
func (k *KV) NumSubjects(bucket string) (int, error) {
	if err := k.client.Connect(); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prefix := "$KV." + bucket + "."
	si, err := k.client.JS().StreamInfo(
		"KV_"+bucket,
		&nats.StreamInfoRequest{SubjectsFilter: prefix + ">"},
		nats.Context(ctx),
	)
	if err != nil {
		return 0, err
	}
	return int(si.State.NumSubjects), nil
}

// Keys lists the keys in a KV bucket.
//
// It deliberately does NOT use nats.go's KeyValue.Keys()/ListKeys(), which both
// enumerate via a WatchAll consumer/watcher. That watcher path was observed to
// fail deterministically for the webterm gateway over WSS — returning an empty
// set while direct Get worked fine — surfacing as a spurious "not an operator"
// 403 (issue #115). Instead we read the bucket's stream subjects via StreamInfo,
// a plain request/reply JS API call (same reliability class as Get), and derive
// the keys from the `$KV.<bucket>.<key>` subjects.
func (k *KV) Keys(bucket string) ([]string, error) {
	if err := k.client.Connect(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	prefix := "$KV." + bucket + "."
	si, err := k.client.JS().StreamInfo(
		"KV_"+bucket,
		&nats.StreamInfoRequest{SubjectsFilter: prefix + ">"},
		nats.Context(ctx),
	)
	if err != nil {
		return nil, err
	}
	var keys []string
	for subject := range si.State.Subjects {
		if key := strings.TrimPrefix(subject, prefix); key != subject && key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}
