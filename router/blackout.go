package router

import (
	"sync"
	"time"
)

// blackoutKey identifies a blacked-out (API key, model) combination. A failure
// on one model does not black out the same API key for other models, since a
// key often keeps working for models other than the one that failed.
type blackoutKey struct {
	key   string
	model string
}

// Blackout tracks (API key, model) combinations that recently failed. A
// blacked-out combination is skipped during normal-key selection until its
// blackout window expires.
type Blackout struct {
	mu    sync.Mutex
	until map[blackoutKey]time.Time
	dur   time.Duration
}

// NewBlackout creates a tracker with the given global blackout duration.
func NewBlackout(d time.Duration) *Blackout {
	return &Blackout{until: make(map[blackoutKey]time.Time), dur: d}
}

// SetDuration updates the blackout window used for future failures.
func (b *Blackout) SetDuration(d time.Duration) {
	b.mu.Lock()
	b.dur = d
	b.mu.Unlock()
}

// Fail marks an (API key, model) combination as blacked out for the configured
// duration.
func (b *Blackout) Fail(key, model string) {
	if b.dur <= 0 {
		return
	}
	b.mu.Lock()
	b.until[blackoutKey{key: key, model: model}] = time.Now().Add(b.dur)
	b.mu.Unlock()
}

// Blocked reports whether an (API key, model) combination is currently within
// its blackout window.
func (b *Blackout) Blocked(key, model string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	bk := blackoutKey{key: key, model: model}
	t, ok := b.until[bk]
	if !ok {
		return false
	}
	if time.Now().After(t) {
		delete(b.until, bk)
		return false
	}
	return true
}
