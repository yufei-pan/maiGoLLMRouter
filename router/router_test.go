package router

import (
	"context"
	"encoding/json"
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
	// Both normal keys should now be blacked out for the failed model.
	if !r.blackout.Blocked("n1", "real-model") || !r.blackout.Blocked("n2", "real-model") {
		t.Errorf("normal keys should be blacked out")
	}
	// The same keys must remain usable for a different model.
	if r.blackout.Blocked("n1", "other-model") || r.blackout.Blocked("n2", "other-model") {
		t.Errorf("normal keys should not be blacked out for a different model")
	}
	if r.blackout.Blocked("f1", "real-model") {
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

// TestBadOutputDoesNotRetrySameKey verifies a bad output is treated like a
// provider error: the same key is never retried in place (which would burn the
// key's rate limit), and the normal key is blacked out before moving on.
func TestBadOutputDoesNotRetrySameKey(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, chatBad) // finish_reason not in good set => bad output
	}))
	defer srv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["n1"]

[model."m"]
targets = ["openai/real-model"]
`, srv.URL))

	r := New(cfg)
	res := r.Execute(context.Background(), provider.OpChat, "m", chatReq())
	if res.Success {
		t.Fatal("expected non-success for bad finish reason")
	}
	if calls != 1 { // the single key is tried exactly once, never retried
		t.Errorf("calls = %d, want 1 (no same-key retry)", calls)
	}
	badOutputs := 0
	for _, a := range res.Attempts {
		if a.Outcome == "bad_output" {
			badOutputs++
		}
	}
	if badOutputs != 1 {
		t.Errorf("bad_output attempts = %d, want 1", badOutputs)
	}
	if !r.blackout.Blocked("n1", "real-model") {
		t.Errorf("bad output should black out the normal key like a provider error")
	}
}

func TestExecuteStripsStreamingControlsForOpenAIProvider(t *testing.T) {
	var sawStream bool
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var outbound map[string]any
		if err := json.NewDecoder(r.Body).Decode(&outbound); err != nil {
			t.Fatalf("decode outbound body: %v", err)
		}
		_, sawStream = outbound["stream"]
		if sawStream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
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

[model."m"]
targets = ["openai/real-model"]
`, srv.URL))

	req := chatReq()
	req["stream"] = true
	req["stream_options"] = map[string]any{"include_usage": true}
	res := New(cfg).Execute(context.Background(), provider.OpChat, "m", req)
	if !res.Success {
		t.Fatalf("expected success for streaming-style inbound request, attempts=%+v", res.Attempts)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if sawStream {
		t.Fatalf("downstream request still included stream field")
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
	if len(cfg.FallbackProviders) != 0 {
		t.Fatalf("undefined fallback provider should be cleared, got %v", cfg.FallbackProviders)
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

func TestResolveFallbackProvidersSequential(t *testing.T) {
	// fallback_providers is a list tried in order by default; an unmapped name
	// expands to one target per defined fallback provider, in config order.
	cfg := loadCfg(t, `
[[provider]]
name = "a"
kind = "openai"
base_url = "https://a/v1"
keys = ["k"]

[[provider]]
name = "b"
kind = "openai"
base_url = "https://b/v1"
keys = ["k"]

[routing]
fallback_providers = ["a", "b"]
`)
	if got := cfg.FallbackSelection; got != config.TargetSelectionSequential {
		t.Fatalf("selection = %q, want sequential", got)
	}
	r := New(cfg)
	for i := 0; i < 10; i++ {
		targets, err := r.Resolve("free")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		want := []config.Target{{Provider: "a", Model: "free"}, {Provider: "b", Model: "free"}}
		if len(targets) != len(want) {
			t.Fatalf("targets = %d, want %d", len(targets), len(want))
		}
		for j := range want {
			if targets[j] != want[j] {
				t.Fatalf("iter %d target[%d] = %+v, want %+v", i, j, targets[j], want[j])
			}
		}
	}
	// Unknown provider prefix routes the FULL name to each fallback provider.
	got, err := r.Resolve("nvidia/nemotron-3")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 || got[0] != (config.Target{Provider: "a", Model: "nvidia/nemotron-3"}) {
		t.Errorf("unexpected fallback targets: %+v", got)
	}
}

func TestResolveFallbackProvidersShuffle(t *testing.T) {
	cfg := loadCfg(t, `
[[provider]]
name = "a"
kind = "openai"
base_url = "https://a/v1"
keys = ["k"]

[[provider]]
name = "b"
kind = "openai"
base_url = "https://b/v1"
keys = ["k"]

[[provider]]
name = "c"
kind = "openai"
base_url = "https://c/v1"
keys = ["k"]

[routing]
fallback_providers = ["a", "b", "c"]
fallback_selection = "random"
`)
	if got := cfg.FallbackSelection; got != config.TargetSelectionShuffle {
		t.Fatalf("selection = %q, want shuffle", got)
	}
	r := New(cfg)
	first := ""
	same := 0
	for i := 0; i < 30; i++ {
		targets, err := r.Resolve("free")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(targets) != 3 {
			t.Fatalf("targets = %d, want 3", len(targets))
		}
		head := targets[0].Provider
		if first == "" {
			first = head
			continue
		}
		if head == first {
			same++
		}
	}
	if same == 29 {
		t.Errorf("shuffle fallback selection never changed first provider across 30 resolves")
	}
}

func TestFallbackProvidersBackwardCompatSingular(t *testing.T) {
	// The deprecated singular fallback_provider still works and is merged into
	// the providers list.
	cfg := loadCfg(t, `
[[provider]]
name = "openrouter"
kind = "openai"
base_url = "https://o/api/v1"
keys = ["k"]

[routing]
fallback_provider = "openrouter"
`)
	if len(cfg.FallbackProviders) != 1 || cfg.FallbackProviders[0] != "openrouter" {
		t.Fatalf("FallbackProviders = %v, want [openrouter]", cfg.FallbackProviders)
	}
	got := mustResolve(t, New(cfg), "free")
	if got.Provider != "openrouter" || got.Model != "free" {
		t.Errorf("Resolve(free) = %s/%s, want openrouter/free", got.Provider, got.Model)
	}
}

func TestFallbackProvidersMergeDedup(t *testing.T) {
	// Plural list comes first; the singular is appended; duplicates and
	// undefined providers are dropped.
	cfg := loadCfg(t, `
[[provider]]
name = "a"
kind = "openai"
base_url = "https://a/v1"
keys = ["k"]

[[provider]]
name = "b"
kind = "openai"
base_url = "https://b/v1"
keys = ["k"]

[routing]
fallback_providers = ["a", "b", "undefined", "a"]
fallback_provider = "b"
`)
	want := []string{"a", "b"}
	if len(cfg.FallbackProviders) != len(want) {
		t.Fatalf("FallbackProviders = %v, want %v", cfg.FallbackProviders, want)
	}
	for i := range want {
		if cfg.FallbackProviders[i] != want[i] {
			t.Fatalf("FallbackProviders = %v, want %v", cfg.FallbackProviders, want)
		}
	}
}

func TestExecuteFallbackProvidersSequenceUntilSuccess(t *testing.T) {
	// Provider "a" always fails; provider "b" succeeds. A bare unmapped name
	// should fall through "a" to "b" and succeed.
	aSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer aSrv.Close()
	bSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, chatStop)
	}))
	defer bSrv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "a"
kind = "openai"
base_url = %q
keys = ["ka"]

[[provider]]
name = "b"
kind = "openai"
base_url = %q
keys = ["kb"]

[routing]
fallback_providers = ["a", "b"]
`, aSrv.URL, bSrv.URL))

	res := New(cfg).Execute(context.Background(), provider.OpChat, "some-unmapped-model", chatReq())
	if !res.Success {
		t.Fatalf("expected success via second fallback provider, attempts=%+v", res.Attempts)
	}
	if res.Provider != "b" {
		t.Errorf("served by %q, want b", res.Provider)
	}
}

// prohibitedBody mimics a downstream that blocks content and returns it with an
// HTTP 200, the case the router must treat as a deterministic policy decision.
const prohibitedBody = `{"error":{"message":"PROHIBITED_CONTENT","code":403}}`

func TestProhibitedContentSkipsKeysAndFallbackWithoutBlackout(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, prohibitedBody) // HTTP 200 with a content-policy block
	}))
	defer srv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[server]
max_retries = 2

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
	if res.Success {
		t.Fatal("expected non-success for prohibited content")
	}
	// Exactly one downstream call: no retries, no other normal key, no fallback key.
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retries, no other keys, no fallback)", calls)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].Outcome != "prohibited_content" {
		t.Fatalf("unexpected attempts: %+v", res.Attempts)
	}
	// The block is not a provider/key fault, so nothing is blacked out.
	if r.blackout.Blocked("n1", "real-model") || r.blackout.Blocked("n2", "real-model") {
		t.Errorf("prohibited content must not black out normal keys")
	}
	// The downstream error body is returned to the caller.
	if !strings.Contains(string(res.Body), "PROHIBITED_CONTENT") {
		t.Errorf("expected prohibited body returned, got %s", res.Body)
	}
}

// openRouterProhibitedBody mimics OpenRouter/Google AI Studio blocking content
// with HTTP 200, content_filter finish reason, and native PROHIBITED_CONTENT.
const openRouterProhibitedBody = `{"id":"gen-test","object":"chat.completion","choices":[{"index":0,"finish_reason":"content_filter","native_finish_reason":"PROHIBITED_CONTENT","message":{"role":"assistant","content":null}}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`

func TestProhibitedContentOpenRouterBodySkipsOtherKeys(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, openRouterProhibitedBody)
	}))
	defer srv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "openrouter"
kind = "openai"
base_url = %q
keys = ["k1","k2","k3"]

[model."m"]
targets = ["openrouter/google/gemma-4-31b-it:free"]
`, srv.URL))

	r := New(cfg)
	res := r.Execute(context.Background(), provider.OpChat, "m", chatReq())
	if res.Success {
		t.Fatal("expected non-success for prohibited content")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no other keys for same model)", calls)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].Outcome != "prohibited_content" {
		t.Fatalf("unexpected attempts: %+v", res.Attempts)
	}
	if !strings.Contains(string(res.Body), "content_filter") {
		t.Errorf("expected upstream body returned as-is, got %s", res.Body)
	}
}

func TestProhibitedContentReturnsAsIsWhenNoMoreModels(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, openRouterProhibitedBody)
	}))
	defer srv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "openrouter"
kind = "openai"
base_url = %q
keys = ["k1","k2"]

[model."m"]
targets = ["openrouter/google/gemma-4-31b-it:free"]
`, srv.URL))

	res := New(cfg).Execute(context.Background(), provider.OpChat, "m", chatReq())
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d, want 200 (passthrough)", res.Status)
	}
	if !strings.Contains(string(res.Body), "PROHIBITED_CONTENT") {
		t.Errorf("expected prohibited body returned as-is, got %s", res.Body)
	}
}

func TestProhibitedContentAdvancesToNextTarget(t *testing.T) {
	var blockCalls, okCalls int
	blockSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockCalls++
		fmt.Fprint(w, prohibitedBody)
	}))
	defer blockSrv.Close()
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		fmt.Fprint(w, chatStop)
	}))
	defer okSrv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "blocker"
kind = "openai"
base_url = %q
keys = ["b1","b2"]

[[provider]]
name = "good"
kind = "openai"
base_url = %q
keys = ["g1"]

[model."m"]
targets = ["blocker/real-model", "good/real-model"]
`, blockSrv.URL, okSrv.URL))

	res := New(cfg).Execute(context.Background(), provider.OpChat, "m", chatReq())
	if !res.Success {
		t.Fatalf("expected success via next target, attempts=%+v", res.Attempts)
	}
	if res.Provider != "good" {
		t.Errorf("served by %q, want good", res.Provider)
	}
	// The blocked combo is tried once, then skipped entirely (no second key).
	if blockCalls != 1 {
		t.Errorf("blocker calls = %d, want 1", blockCalls)
	}
	if okCalls != 1 {
		t.Errorf("good calls = %d, want 1", okCalls)
	}
}

func TestResolveSequentialPreservesOrder(t *testing.T) {
	cfg := loadCfg(t, `
[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com"
keys = ["k"]

[model."m"]
targets = ["openai/first", "openai/second", "openai/third"]
`)
	r := New(cfg)
	want := []string{"openai/first", "openai/second", "openai/third"}
	for i := 0; i < 10; i++ {
		targets, err := r.Resolve("m")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		for j, tgt := range targets {
			got := tgt.Provider + "/" + tgt.Model
			if got != want[j] {
				t.Fatalf("iteration %d: target[%d] = %q, want %q", i, j, got, want[j])
			}
		}
	}
}

func TestResolveShuffleVariesOrder(t *testing.T) {
	cfg := loadCfg(t, `
[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com"
keys = ["k"]

[model."m"]
selection = "shuffle"
targets = ["openai/first", "openai/second", "openai/third"]
`)
	r := New(cfg)
	first := ""
	same := 0
	for i := 0; i < 30; i++ {
		targets, err := r.Resolve("m")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(targets) != 3 {
			t.Fatalf("targets = %d, want 3", len(targets))
		}
		head := targets[0].Model
		if first == "" {
			first = head
			continue
		}
		if head == first {
			same++
		}
	}
	if same == 29 {
		t.Errorf("shuffle selection never changed first target across 30 resolves")
	}
}
