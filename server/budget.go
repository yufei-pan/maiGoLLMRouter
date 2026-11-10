package server

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"
)

// memoryBudget bounds the bodies being decoded at once, in raw bytes.
// Measured 2026-11-09 with TestMeasureWorstBodyPeak: the worst 32 MiB body the value cap
// lets through (ROUTER_MEASURE_PEAK=chat-anthropic-args: a tool call whose arguments string
// holds ~11 million empty objects as JSON text, on an Anthropic-kind target) raised peak RSS
// by 1556 MiB (four runs: 1547, 1556, 1375, 1388; GC timing spreads them ~180 MiB), so
// 1556/32 ≈ 49 bytes per body byte. A body over the value cap is refused after its decode,
// which peaks at ~1.2 GiB, below that. A full budget of the worst bodies must peak under
// 3 GiB, the memory this host usually has free: 3072 MiB * 32 / 1556 = 63.2 MiB, rounded
// down to 63 MiB. Never below 32 MiB, so one maximum-size body always fits.
const memoryBudget = 63 << 20

const budgetWait = 25 * time.Second // annai gives up after 30 s without a first event (D9)

const busyMessage = "the router is busy with other large requests; retry shortly"

// budget is a weighted semaphore over raw body bytes. A request that fits is
// admitted at once, even while larger ones wait, so it is not FIFO.
type budget struct {
	mu      sync.Mutex
	size    int64
	used    int64
	waiters []*waiter // in arrival order
}

type waiter struct {
	n     int64
	ready chan struct{} // closed once n is granted
}

func newBudget(size int64) *budget { return &budget{size: size} }

// acquire takes n bytes (n > size is taken as size), waiting at most wait or until ctx ends.
// On false nothing is held. release must be called exactly once, at request end.
func (b *budget) acquire(ctx context.Context, n int64, wait time.Duration) (release func(), ok bool) {
	n = min(n, b.size)
	release = func() { b.release(n) }
	b.mu.Lock()
	if b.used+n <= b.size {
		b.used += n
		b.mu.Unlock()
		return release, true
	}
	w := &waiter{n: n, ready: make(chan struct{})}
	b.waiters = append(b.waiters, w)
	b.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-w.ready:
		return release, true
	case <-timer.C:
	case <-ctx.Done():
	}
	b.mu.Lock()
	select {
	case <-w.ready: // granted as the wait ended: give it back
		b.mu.Unlock()
		release()
		return nil, false
	default:
	}
	b.waiters = slices.DeleteFunc(b.waiters, func(x *waiter) bool { return x == w })
	b.mu.Unlock()
	return nil, false
}

// release gives n bytes back and grants, in arrival order, every waiter that
// now fits; one that doesn't fit holds up none behind it.
func (b *budget) release(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= n
	kept := b.waiters[:0]
	for _, w := range b.waiters {
		if b.used+w.n <= b.size {
			b.used += w.n
			close(w.ready)
		} else {
			kept = append(kept, w)
		}
	}
	clear(b.waiters[len(kept):])
	b.waiters = kept
}

// admit waits for n bytes of the budget. If the wait runs out it answers 503 busy
// (Retry-After: 5) and returns ok=false; if the client went away it writes nothing.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, n int64) (release func(), ok bool) {
	if release, ok = s.budget.acquire(r.Context(), n, s.budgetWait); ok {
		return release, true
	}
	if r.Context().Err() == nil {
		// 5 s is the design's value ("Wait limit"); nothing was measured for
		// it. It is a hint for clients that retry; annai's own fallback moves
		// on at the 503 (D9).
		w.Header().Set("Retry-After", "5")
		writeCodedError(w, http.StatusServiceUnavailable, busyMessage, "server_error", "busy")
	}
	return nil, false
}
