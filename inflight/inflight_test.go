package inflight

import (
	"sync"
	"testing"
	"time"

	"maiGoLLMRouter/router"
)

func TestRegistryRegisterUnregister(t *testing.T) {
	r := New()
	e := r.Register(Meta{
		ID:           "req-abc",
		StartedAt:    "2026-06-16T12:00:00Z",
		Endpoint:     "/v1/chat/completions",
		InboundModel: "gpt-4",
	})
	if e == nil {
		t.Fatal("Register returned nil")
	}
	snap := r.Snapshot()
	if len(snap) != 1 || snap[0].ID != "req-abc" {
		t.Fatalf("snapshot after register: %+v", snap)
	}
	r.Unregister("req-abc")
	if len(r.Snapshot()) != 0 {
		t.Fatal("expected empty snapshot after unregister")
	}
}

func TestSnapshotOrdering(t *testing.T) {
	r := New()
	r.Register(Meta{ID: "old", StartedAt: "2026-06-16T10:00:00Z"})
	r.Register(Meta{ID: "new", StartedAt: "2026-06-16T12:00:00Z"})
	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("len=%d", len(snap))
	}
	if snap[0].ID != "new" || snap[1].ID != "old" {
		t.Fatalf("order: %+v", snap)
	}
}

func TestOnAttempt(t *testing.T) {
	e := &Entry{ID: "req-1"}
	e.OnAttempt(router.Attempt{Provider: "openai", Model: "gpt-4", Outcome: "provider_error"})
	e.OnAttempt(router.Attempt{Provider: "anthropic", Model: "claude", Outcome: "success"})
	s := e.snapshot()
	if s.Attempts != 2 {
		t.Fatalf("attempts=%d", s.Attempts)
	}
	if s.CurrentTarget != "anthropic/claude" {
		t.Fatalf("current_target=%q", s.CurrentTarget)
	}
	if s.LastOutcome != "success" {
		t.Fatalf("last_outcome=%q", s.LastOutcome)
	}
}

func TestConcurrentSnapshot(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := "req-" + string(rune('a'+n))
			live := r.Register(Meta{ID: id, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)})
			live.OnAttempt(router.Attempt{Provider: "p", Model: "m", Outcome: "provider_error"})
			_ = r.Snapshot()
			r.Unregister(id)
		}(i)
	}
	wg.Wait()
}
