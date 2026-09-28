package router

import (
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
