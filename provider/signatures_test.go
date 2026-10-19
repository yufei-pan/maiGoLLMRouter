package provider

import (
	"testing"
	"time"
)

func TestSignatureCache(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	c := NewSignatureCache(2, 24*time.Hour)
	c.now = func() time.Time { return now }

	c.Put("c1", "google/gemini-3.5-flash", "s")
	if got := c.Get("c1", "google/gemini-3.5-flash"); got != "s" {
		t.Errorf("same signer: got %q, want s", got)
	}
	if got := c.Get("c1", "google/gemini-3.6-flash"); got != "" {
		t.Errorf("other signer: got %q, want nothing", got)
	}

	now = now.Add(25 * time.Hour)
	if got := c.Get("c1", "google/gemini-3.5-flash"); got != "" {
		t.Errorf("after 25h: got %q, want nothing", got)
	}

	c.Put("a", "p/m", "1")
	c.Put("b", "p/m", "2")
	c.Put("d", "p/m", "3")
	if c.Get("a", "p/m") != "" && c.Get("c1", "google/gemini-3.5-flash") != "" {
		t.Error("a cache of 2 should have evicted its oldest entries")
	}
	if c.Get("d", "p/m") != "3" || c.Get("b", "p/m") != "2" {
		t.Error("the two newest entries should remain")
	}

	var nilCache *SignatureCache
	if nilCache.Get("x", "y") != "" {
		t.Error("a nil cache knows nothing")
	}
	nilCache.Put("x", "y", "z") // must not panic
}
