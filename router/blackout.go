package router

import (
	"sync"
	"time"
)

// Blackout tracks API keys that recently failed. A blacked-out key is skipped
// during normal-key selection until its global blackout window expires.
type Blackout struct {
	mu    sync.Mutex
	until map[string]time.Time
	dur   time.Duration
}

// NewBlackout creates a tracker with the given global blackout duration.
func NewBlackout(d time.Duration) *Blackout {
	return &Blackout{until: make(map[string]time.Time), dur: d}
}

// Fail marks a key as blacked out for the configured duration.
func (b *Blackout) Fail(key string) {
	if b.dur <= 0 {
		return
	}
	b.mu.Lock()
	b.until[key] = time.Now().Add(b.dur)
	b.mu.Unlock()
}

// Blocked reports whether a key is currently within its blackout window.
func (b *Blackout) Blocked(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t, ok := b.until[key]
	if !ok {
		return false
	}
	if time.Now().After(t) {
		delete(b.until, key)
		return false
	}
	return true
}
