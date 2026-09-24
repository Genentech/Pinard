package state

// GCState tracks when the daemon's low-frequency GC backstop sweep last ran,
// so restarts don't re-trigger it more often than the configured interval.
type GCState struct {
	LastGC string `yaml:"last_gc,omitempty"`
}
