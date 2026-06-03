package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"maiGoLLMRouter/config"
	"maiGoLLMRouter/provider"
)

func loadCfg(t *testing.T, content string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

const chatStop = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`
const chatBad = `{"choices":[{"finish_reason":"content_filter","message":{"role":"assistant","content":"hi"}}]}`

func chatReq() map[string]any {
	return map[string]any{
		"model":    "ignored",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
}

func TestExecuteSuccessNormalKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, chatStop)
	}))
	defer srv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["n1","n2"]

[model."m"]
targets = ["openai/real-model"]
`, srv.URL))

	res := New(cfg).Execute(context.Background(), provider.OpChat, "m", chatReq())
	if !res.Success {
		t.Fatalf("expected success, attempts=%+v", res.Attempts)
	}
	if res.Provider != "openai" || res.Model != "real-model" {
		t.Errorf("served by %s/%s", res.Provider, res.Model)
	}
}

func TestBlackoutThenFallbackKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Normal keys (n*) fail; fallback key (f1) succeeds.
		if strings.HasPrefix(bearer(r), "n") {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":"boom"}`)
			return
		}
		fmt.Fprint(w, chatStop)
	}))
	defer srv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["n1","n2"]
fallback_keys = ["f1"]

[model."m"]
targets = ["openai/real-model"]
`, srv.URL))

	r := New(cfg)
	res := r.Execute(context.Background(), provider.OpChat, "m", chatReq())
	if !res.Success {
		t.Fatalf("expected fallback success, attempts=%+v", res.Attempts)
	}
	// Both normal keys should now be blacked out.
	if !r.blackout.Blocked("n1") || !r.blackout.Blocked("n2") {
		t.Errorf("normal keys should be blacked out")
	}
	if r.blackout.Blocked("f1") {
		t.Errorf("fallback key must never be blacked out")
	}
	// Last attempt should be the successful fallback key.
	last := res.Attempts[len(res.Attempts)-1]
	if last.KeyType != "fallback" || last.Outcome != "success" {
		t.Errorf("unexpected last attempt: %+v", last)
	}
}

func TestFallbackModelsGating(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(bearer(r), "n") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, chatStop)
	}))
	defer srv.Close()

	// fallback_models excludes "real-model", so fallback key must be skipped.
	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["n1"]
fallback_keys = ["f1"]
fallback_models = ["other-model"]

[model."m"]
targets = ["openai/real-model"]
`, srv.URL))

	res := New(cfg).Execute(context.Background(), provider.OpChat, "m", chatReq())
	if res.Success {
		t.Fatal("expected failure: fallback gated out")
	}
	for _, a := range res.Attempts {
		if a.KeyType == "fallback" {
			t.Errorf("fallback key should not have been attempted: %+v", a)
		}
	}
}

func TestOutputVerificationRetry(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, chatBad) // finish_reason not in good set => bad output
	}))
	defer srv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[server]
max_retries = 2

[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["n1"]

[model."m"]
targets = ["openai/real-model"]
`, srv.URL))

	res := New(cfg).Execute(context.Background(), provider.OpChat, "m", chatReq())
	if res.Success {
		t.Fatal("expected non-success for bad finish reason")
	}
	if calls != 3 { // 1 initial + 2 retries
		t.Errorf("calls = %d, want 3", calls)
	}
	badOutputs := 0
	for _, a := range res.Attempts {
		if a.Outcome == "bad_output" {
			badOutputs++
		}
	}
	if badOutputs != 3 {
		t.Errorf("bad_output attempts = %d, want 3", badOutputs)
	}
}

func mustResolve(t *testing.T, r *Router, model string) config.Target {
	t.Helper()
	got, err := r.Resolve(model)
	if err != nil {
		t.Fatalf("Resolve(%q) error: %v", model, err)
	}
	return got[0]
}

func wantErr(t *testing.T, r *Router, model string) {
	t.Helper()
	if _, err := r.Resolve(model); err == nil {
		t.Errorf("Resolve(%q): expected error, got none", model)
	}
}

func TestResolveWithDefinedFallbackProvider(t *testing.T) {
	cfg := loadCfg(t, `
[[provider]]
name = "google"
kind = "gemini"
base_url = "https://g/v1beta"
keys = ["k"]

[[provider]]
name = "openrouter"
kind = "openai"
base_url = "https://o/api/v1"
keys = ["k"]

[routing]
fallback_provider = "openrouter"
`)
	r := New(cfg)

	cases := []struct{ in, prov, model string }{
		{"google/gemma-4", "google", "gemma-4"},
		{"openrouter/google/gemma-4", "openrouter", "google/gemma-4"},
		{"free", "openrouter", "free"},
		{"nvidia/nemotron-3", "openrouter", "nvidia/nemotron-3"},
	}
	for _, c := range cases {
		got := mustResolve(t, r, c.in)
		if got.Provider != c.prov || got.Model != c.model {
			t.Errorf("Resolve(%q) = %s/%s, want %s/%s", c.in, got.Provider, got.Model, c.prov, c.model)
		}
	}
}

func TestResolveWithUndefinedFallbackProvider(t *testing.T) {
	// fallback_provider points at "openrouter", which is NOT defined, so it is
	// treated as no fallback.
	cfg := loadCfg(t, `
[[provider]]
name = "google"
kind = "gemini"
base_url = "https://g/v1beta"
keys = ["k"]

[routing]
fallback_provider = "openrouter"
`)
	if cfg.FallbackProvider != "" {
		t.Fatalf("undefined fallback provider should be cleared, got %q", cfg.FallbackProvider)
	}
	r := New(cfg)

	got := mustResolve(t, r, "google/gemma-4")
	if got.Provider != "google" || got.Model != "gemma-4" {
		t.Errorf("Resolve(google/gemma-4) = %s/%s", got.Provider, got.Model)
	}
	wantErr(t, r, "openrouter/google/gemma-4")
	wantErr(t, r, "free")
	wantErr(t, r, "nvidia/nemotron-3")
}

func TestFallbackModelsUseNormalKeysWhenNoFallbackKeys(t *testing.T) {
	// Normal keys (n*) fail in phase 1; with fallback_models set but no
	// fallback_keys, the normal keys are reused in the fallback round and now
	// succeed (the mock allows the second time via a different key).
	var seen = map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		seen[tok]++
		// Fail on the phase-1 attempt, succeed on the phase-2 reuse.
		if seen[tok] == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, chatStop)
	}))
	defer srv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["n1"]
fallback_models = ["real-model"]

[model."m"]
targets = ["openai/real-model"]
`, srv.URL))

	res := New(cfg).Execute(context.Background(), provider.OpChat, "m", chatReq())
	if !res.Success {
		t.Fatalf("expected success via normal-key fallback round, attempts=%+v", res.Attempts)
	}
	last := res.Attempts[len(res.Attempts)-1]
	if last.KeyType != "fallback" {
		t.Errorf("expected success on fallback round, last=%+v", last)
	}
}

func TestFallbackModelsGatingWithReusedNormalKeys(t *testing.T) {
	// fallback_models excludes the routed model, so even though fallback would
	// reuse normal keys, the model is gated out and the request fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["n1"]
fallback_models = ["other-model"]

[model."m"]
targets = ["openai/real-model"]
`, srv.URL))

	res := New(cfg).Execute(context.Background(), provider.OpChat, "m", chatReq())
	if res.Success {
		t.Fatal("expected failure: model gated out of fallback")
	}
	// Only the single phase-1 attempt should exist (no fallback round for this model).
	if len(res.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1 (no fallback round)", len(res.Attempts))
	}
}
