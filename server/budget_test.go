package server

import (
	"context"
	"sync"
	"testing"
	"time"
)

// grant is one acquire's answer.
type grant struct {
	release func()
	ok      bool
}

// acquireAsync runs acquire in the background and sends its answer.
func acquireAsync(ctx context.Context, b *budget, n int64, wait time.Duration) <-chan grant {
	got := make(chan grant, 1)
	go func() {
		release, ok := b.acquire(ctx, n, wait)
		got <- grant{release, ok}
	}()
	return got
}

// within is the answer from got, failing the test when none arrives within d.
func within(t *testing.T, got <-chan grant, d time.Duration) grant {
	t.Helper()
	select {
	case g := <-got:
		return g
	case <-time.After(d):
		t.Fatalf("acquire did not answer within %v", d)
		return grant{}
	}
}

// queued waits until b has n waiters.
func queued(t *testing.T, b *budget, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		got := len(b.waiters)
		b.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d waiters, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// held is how much of b is taken.
func held(b *budget) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func TestBudgetWaitsThenRuns(t *testing.T) {
	b := newBudget(10)
	releaseA, ok := b.acquire(t.Context(), 6, 0)
	if !ok {
		t.Fatal("A was refused on an empty budget")
	}
	gotB := acquireAsync(t.Context(), b, 6, 10*time.Second)
	queued(t, b, 1)
	releaseA()
	g := within(t, gotB, time.Second)
	if !g.ok {
		t.Fatal("B was refused after A released")
	}
	g.release()
}

func TestBudgetTimeout(t *testing.T) {
	b := newBudget(10)
	release, _ := b.acquire(t.Context(), 10, 0)
	start := time.Now()
	if _, ok := b.acquire(t.Context(), 1, 50*time.Millisecond); ok {
		t.Fatal("admitted while the budget was full")
	}
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Errorf("refused after %v, before the 50 ms wait ran out", d)
	}
	release()
	// With no wait allowed, only room that is free right now admits it.
	r, ok := b.acquire(t.Context(), 10, 0)
	if !ok {
		t.Fatal("the whole budget was refused after its holder released it")
	}
	r()
}

func TestBudgetLargerThanSize(t *testing.T) {
	b := newBudget(10)
	release, ok := b.acquire(t.Context(), 20, 0)
	if !ok {
		t.Fatal("a body larger than the budget was refused on an empty budget")
	}
	defer release()
	if _, ok := b.acquire(t.Context(), 1, 10*time.Millisecond); ok {
		t.Error("admitted beside a body that takes the whole budget")
	}
}

// Review Focus 5: the budget is not FIFO.
func TestBudgetSmallPassesWaitingLarge(t *testing.T) {
	b := newBudget(10)
	release, _ := b.acquire(t.Context(), 6, 0)
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	large := acquireAsync(ctx, b, 10, 10*time.Second)
	queued(t, b, 1)
	small, ok := b.acquire(t.Context(), 1, 0)
	if !ok {
		t.Fatal("a request that fits waited behind a larger one")
	}
	small()
	cancel()
	if g := within(t, large, time.Second); g.ok {
		t.Error("the large waiter was admitted although 6 of 10 stayed held")
	}
}

func TestBudgetCanceledWaiterFreesQueue(t *testing.T) {
	b := newBudget(10)
	// 7, not 6: with 6 held, B's 4 would fit and get in at once.
	release, _ := b.acquire(t.Context(), 7, 0)
	ctxA, cancelA := context.WithCancel(t.Context())
	gotA := acquireAsync(ctxA, b, 10, 10*time.Second)
	queued(t, b, 1)
	gotB := acquireAsync(t.Context(), b, 4, 10*time.Second)
	queued(t, b, 2)
	cancelA()
	if g := within(t, gotA, time.Second); g.ok {
		t.Fatal("A was admitted after its context ended")
	}
	release()
	g := within(t, gotB, time.Second)
	if !g.ok {
		t.Fatal("B was refused")
	}
	if u := held(b); u != 4 {
		t.Errorf("%d held, want B's 4", u)
	}
	g.release()
}

// A release grants every waiter that now fits, not only up to the first that
// doesn't.
func TestBudgetReleaseGrantsPastLargeWaiter(t *testing.T) {
	b := newBudget(10)
	release5, _ := b.acquire(t.Context(), 5, 0)
	defer release5()
	release4, _ := b.acquire(t.Context(), 4, 0)
	ctxA, cancelA := context.WithCancel(t.Context())
	defer cancelA()
	acquireAsync(ctxA, b, 10, 10*time.Second)
	queued(t, b, 1)
	gotB := acquireAsync(t.Context(), b, 4, 10*time.Second)
	queued(t, b, 2)
	release4()
	g := within(t, gotB, time.Second)
	if !g.ok {
		t.Fatal("B was refused")
	}
	g.release()
}

// Ordinary requests never wait: 16 at once with a 2 MiB image or clip each.
func TestBudgetFitsOrdinaryLoad(t *testing.T) {
	if memoryBudget < 32<<20 {
		t.Errorf("memoryBudget is %d MiB, so one maximum-size body (32 MiB) can't fit", memoryBudget>>20)
	}
	b := newBudget(memoryBudget)
	grants := make([]grant, 16)
	var wg sync.WaitGroup
	for i := range grants {
		wg.Go(func() { grants[i].release, grants[i].ok = b.acquire(t.Context(), 2<<20, 0) })
	}
	wg.Wait()
	for i, g := range grants {
		if !g.ok {
			t.Errorf("2 MiB body %d of 16 was not admitted at once", i)
			continue
		}
		g.release()
	}
}

// Waits short enough to run out while releases grant them: whichever wins,
// nothing stays held and no waiter stays queued.
func TestBudgetChurnLeavesNothingHeld(t *testing.T) {
	b := newBudget(10)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 2000 {
				n := int64(1 + (g+i)%10)
				if release, ok := b.acquire(t.Context(), n, time.Duration(i%4)*20*time.Microsecond); ok {
					release()
				}
			}
		})
	}
	wg.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used != 0 || len(b.waiters) != 0 {
		t.Errorf("after every request ended: %d held, %d waiters", b.used, len(b.waiters))
	}
}
