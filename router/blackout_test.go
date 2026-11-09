package router

import (
	"strconv"
	"testing"
	"time"
)

func TestBlackoutSuccessClearsOnlyThatPair(t *testing.T) {
	b := NewBlackout(time.Hour)
	b.Fail("k1", "m1")
	b.Fail("k1", "m2")
	b.Fail("k2", "m1")

	if !b.Blocked("k1", "m1") || !b.Blocked("k1", "m2") || !b.Blocked("k2", "m1") {
		t.Fatal("setup: expected all three pairs to be blacked out")
	}

	b.Success("k1", "m1")

	if b.Blocked("k1", "m1") {
		t.Error("success must clear that key/model from the blackout list")
	}
	if !b.Blocked("k1", "m2") {
		t.Error("success must not clear a different model on the same key")
	}
	if !b.Blocked("k2", "m1") {
		t.Error("success must not clear a different key on the same model")
	}
}

func TestBlackoutSuccessOnUnknownPairIsNoop(t *testing.T) {
	b := NewBlackout(time.Hour)
	b.Success("k1", "m1")
	if b.Blocked("k1", "m1") {
		t.Error("clearing a pair that was never blacked out must not block it")
	}
}

func TestFailRemovesExpired(t *testing.T) {
	b := NewBlackout(time.Nanosecond)
	b.Fail("k", "old")
	b.SetDuration(time.Hour)
	b.Fail("k", "new")

	if len(b.until) != 1 {
		t.Errorf("entries = %d, want 1: Fail must remove the expired pair", len(b.until))
	}
	if !b.Blocked("k", "new") {
		t.Error("the pair that just failed must be blacked out")
	}
}

// failAndSweep calls Fail with a name not yet in the map and reports whether
// it swept: without a sweep the map grows by exactly that one entry.
func failAndSweep(b *Blackout, model string) bool {
	before := len(b.until)
	b.Fail("k", model)
	return len(b.until) <= before
}

// While the live entries grow, Fail sweeps only when the map has doubled since
// the last sweep, so the sweeps grow with log N, not one walk of the whole map
// per failure. Each round adds a live pair and one that expires at once, so a
// sweep shows as the expired pairs leaving.
func TestFailSweepsWhenMapDoubles(t *testing.T) {
	b := NewBlackout(time.Hour)
	const rounds = 4096
	sweeps := 0
	for i := 0; i < rounds; i++ {
		b.SetDuration(time.Hour)
		if failAndSweep(b, "live"+strconv.Itoa(i)) {
			sweeps++
		}
		b.SetDuration(time.Nanosecond)
		if failAndSweep(b, "gone"+strconv.Itoa(i)) {
			sweeps++
		}
	}
	// A sweep leaves L live entries and the next comes at 2L, after L/2 rounds
	// that leave 1.5L live: about log1.5(4096) = 21 sweeps. 19 show here; the
	// first few find nothing expired and leave no trace. Sweeping on every
	// Fail would show 4095.
	if sweeps > 24 {
		t.Errorf("%d sweeps over %d rounds, want at most 24", sweeps, rounds)
	}
	for i := 0; i < rounds; i++ {
		if !b.Blocked("k", "live"+strconv.Itoa(i)) {
			t.Fatalf("live entry %d was swept", i)
		}
	}
}

// A flood of names that expire at once stays within twice the live entries,
// and each sweep is paid for by about as many failures as there are live
// entries.
func TestFailKeepsMapWithinTwiceLive(t *testing.T) {
	b := NewBlackout(time.Hour)
	const live = 100
	for i := 0; i < live; i++ {
		b.Fail("k", "live"+strconv.Itoa(i))
	}
	b.SetDuration(time.Nanosecond)
	const n = 10000
	sweeps := 0
	for i := 0; i < n; i++ {
		if failAndSweep(b, "gone"+strconv.Itoa(i)) {
			sweeps++
		}
		// The live entries plus the one just failed, which lasts a nanosecond.
		if len(b.until) > 2*(live+1) {
			t.Fatalf("after %d expired failures the map holds %d entries, want at most %d", i+1, len(b.until), 2*(live+1))
		}
	}
	if sweeps > n/live+1 {
		t.Errorf("%d sweeps over %d failures, want at most %d", sweeps, n, n/live+1)
	}
	for i := 0; i < live; i++ {
		if !b.Blocked("k", "live"+strconv.Itoa(i)) {
			t.Fatalf("live entry %d was swept", i)
		}
	}
}
