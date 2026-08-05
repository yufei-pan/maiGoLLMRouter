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
const chatBadFinish = `{"choices":[{"finish_reason":"unexpected","message":{"role":"assistant","content":"hi"}}]}`
const chatContentFilter = `{"choices":[{"finish_reason":"content_filter","message":{"role":"assistant","content":"hi"}}]}`

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

// TestHTTP400DoesNotBlackout verifies a downstream HTTP 400 (client/request
// error) is not treated as a key/model/provider fault: normal keys stay usable.
func TestHTTP400DoesNotBlackout(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"bad request","type":"invalid_request_error"}}`)
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
	if res.Success {
		t.Fatal("expected non-success for HTTP 400")
	}
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.Status)
	}
	if r.blackout.Blocked("n1", "real-model") || r.blackout.Blocked("n2", "real-model") {
		t.Errorf("HTTP 400 must not black out normal keys for the model")
	}
	if r.blackout.Blocked("f1", "real-model") {
		t.Errorf("fallback key must never be blacked out")
	}
	for _, a := range res.Attempts {
		if a.Outcome == "skipped_blackout" {
			t.Errorf("unexpected blackout skip after HTTP 400: %+v", a)
		}
	}
	if calls < 1 {
		t.Errorf("expected at least one downstream call, got %d", calls)
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

// TestBadOutputDoesNotRetrySameKey verifies a bad output never retries the
// same key in place (which would burn the key's rate limit), and does not
// black out the key — empty/unfinished HTTP 200 bodies are not a key-health
// signal.
func TestBadOutputDoesNotRetrySameKey(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, chatBadFinish) // finish_reason not in good set => bad output
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
	if r.blackout.Blocked("n1", "real-model") {
		t.Errorf("bad output must not black out the normal key")
	}
}

func TestContentFilterTreatedAsProhibited(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		fmt.Fprint(w, chatContentFilter)
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
		t.Fatal("expected non-success for content_filter")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no same-key retry)", calls)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].Outcome != "prohibited_content" {
		t.Fatalf("unexpected attempts: %+v", res.Attempts)
	}
	if res.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (content_filter must not passthrough as 200)", res.Status)
	}
	if r.blackout.Blocked("n1", "real-model") {
		t.Errorf("content_filter is a policy block, must not black out the key")
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

func responsesReq() map[string]any {
	return map[string]any{"model": "ignored", "input": "hi"}
}

// responsesProbeServer answers /responses with a 404 (no such route) and
// /chat/completions with a normal completion, recording every path it served.
func responsesProbeServer(t *testing.T, paths *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"not found"}}`)
			return
		}
		fmt.Fprint(w, chatStop)
	}))
}

// TestResponsesProbeResultIsCached verifies the router probes /responses once
// per provider and reuses the learned chat-only verdict for later requests.
func TestResponsesProbeResultIsCached(t *testing.T) {
	var paths []string
	srv := responsesProbeServer(t, &paths)
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
	res := r.Execute(context.Background(), provider.OpResponses, "m", responsesReq())
	if !res.Success {
		t.Fatalf("expected success via chat fallback, attempts=%+v", res.Attempts)
	}
	want := []string{"/responses", "/chat/completions"}
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("first request paths = %v, want %v", paths, want)
	}
	var parsed map[string]any
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if parsed["object"] != "response" {
		t.Errorf("client body not Responses-shaped: %s", res.Body)
	}

	paths = nil
	res = r.Execute(context.Background(), provider.OpResponses, "m", responsesReq())
	if !res.Success {
		t.Fatalf("expected success on cached chat-only path, attempts=%+v", res.Attempts)
	}
	if len(paths) != 1 || paths[0] != "/chat/completions" {
		t.Fatalf("second request paths = %v, want [/chat/completions] (cache not used)", paths)
	}
}

// TestResponsesReloadClearsCapabilityCache verifies a config reload forgets the
// learned chat-only verdict so a provider that gained /responses is probed again.
func TestResponsesReloadClearsCapabilityCache(t *testing.T) {
	var paths []string
	srv := responsesProbeServer(t, &paths)
	defer srv.Close()

	content := fmt.Sprintf(`
[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["n1"]

[model."m"]
targets = ["openai/real-model"]
`, srv.URL)

	r := New(loadCfg(t, content))
	if res := r.Execute(context.Background(), provider.OpResponses, "m", responsesReq()); !res.Success {
		t.Fatalf("expected success via chat fallback, attempts=%+v", res.Attempts)
	}

	r.Reload(loadCfg(t, content))

	paths = nil
	if res := r.Execute(context.Background(), provider.OpResponses, "m", responsesReq()); !res.Success {
		t.Fatalf("expected success after reload, attempts=%+v", res.Attempts)
	}
	if len(paths) != 2 || paths[0] != "/responses" {
		t.Fatalf("after reload paths = %v, want a fresh /responses probe first", paths)
	}
}

// TestResponsesIncompatibleEverywhereReturns400 verifies a Responses body using
// Responses-only features against chat-only providers is reported as a client
// error rather than an upstream failure, and never blacks out a key.
func TestResponsesIncompatibleEverywhereReturns400(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		t.Errorf("unexpected downstream call to %s for a non-portable body", r.URL.Path)
	}))
	defer srv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["n1","n2"]
fallback_keys = ["f1"]
supports_responses = false

[model."m"]
targets = ["openai/real-model"]
`, srv.URL))

	body := responsesReq()
	body["tools"] = []any{map[string]any{"type": "web_search"}}

	r := New(cfg)
	res := r.Execute(context.Background(), provider.OpResponses, "m", body)
	if res.Success {
		t.Fatal("expected non-success for a non-portable Responses body")
	}
	if calls != 0 {
		t.Errorf("calls = %d, want 0 (nothing portable to send)", calls)
	}
	if res.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.Status)
	}
	if !strings.Contains(string(res.Body), "Responses") {
		t.Errorf("expected an explanatory Responses error body, got %s", res.Body)
	}
	if len(res.Attempts) != 3 {
		t.Fatalf("attempts = %d, want 3 (both normal keys and the fallback key)", len(res.Attempts))
	}
	for _, a := range res.Attempts {
		if a.Outcome != "incompatible" {
			t.Errorf("unexpected attempt outcome: %+v", a)
		}
	}
	if r.blackout.Blocked("n1", "real-model") || r.blackout.Blocked("n2", "real-model") {
		t.Errorf("an incompatible request is not a key fault, must not black out")
	}
}

// TestResponsesIncompatibleAdvancesToCapableProvider verifies an incompatible
// target does not end the run: a Responses-capable provider still gets a turn.
func TestResponsesIncompatibleAdvancesToCapableProvider(t *testing.T) {
	var okCalls int
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls++
		fmt.Fprint(w, `{"id":"r1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`)
	}))
	defer okSrv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "chatonly"
kind = "openai"
base_url = "http://127.0.0.1:0"
keys = ["c1"]
supports_responses = false

[[provider]]
name = "native"
kind = "openai"
base_url = %q
keys = ["g1"]
supports_responses = true

[model."m"]
targets = ["chatonly/real-model", "native/real-model"]
`, okSrv.URL))

	body := responsesReq()
	body["tools"] = []any{map[string]any{"type": "web_search"}}

	res := New(cfg).Execute(context.Background(), provider.OpResponses, "m", body)
	if !res.Success {
		t.Fatalf("expected success via the Responses-capable provider, attempts=%+v", res.Attempts)
	}
	if res.Provider != "native" {
		t.Errorf("served by %q, want native", res.Provider)
	}
	if okCalls != 1 {
		t.Errorf("native calls = %d, want 1", okCalls)
	}
}

// TestExhaustionPreservesProviderErrorOverIncompatible verifies that an
// incompatible second target (no HTTP call) must not overwrite the last
// real provider_error, so exhaustion returns the earlier 429 body/status
// instead of a generic 502.
func TestExhaustionPreservesProviderErrorOverIncompatible(t *testing.T) {
	const rateLimitBody = `{"error":{"message":"rate limited","type":"rate_limit_error"}}`
	rateSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, rateLimitBody)
	}))
	defer rateSrv.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "ratelimited"
kind = "openai"
base_url = %q
keys = ["r1"]
supports_responses = true

[[provider]]
name = "chatonly"
kind = "openai"
base_url = "http://127.0.0.1:0"
keys = ["c1"]
supports_responses = false

[model."m"]
targets = ["ratelimited/real-model", "chatonly/real-model"]
`, rateSrv.URL))

	body := responsesReq()
	body["tools"] = []any{map[string]any{"type": "web_search"}}

	res := New(cfg).Execute(context.Background(), provider.OpResponses, "m", body)
	if res.Success {
		t.Fatal("expected exhaustion, got success")
	}
	if res.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 (preserved provider_error), body=%s", res.Status, res.Body)
	}
	if string(res.Body) != rateLimitBody {
		t.Errorf("body = %s, want the 429 JSON from the first target", res.Body)
	}
	if len(res.Attempts) < 2 {
		t.Fatalf("attempts = %d, want at least provider_error then incompatible", len(res.Attempts))
	}
	if res.Attempts[0].Outcome != "provider_error" || res.Attempts[0].HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("first attempt = %+v, want provider_error 429", res.Attempts[0])
	}
	if res.Attempts[1].Outcome != "incompatible" {
		t.Errorf("second attempt = %+v, want incompatible", res.Attempts[1])
	}
}

// TestResponsesUnwrappableChatReplyIsBadOutput verifies a chat reply that cannot
// be wrapped into a Responses object is a bad output (an HTTP 200 with no usable
// body), not a success.
func TestResponsesUnwrappableChatReplyIsBadOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"not found"}}`)
			return
		}
		fmt.Fprint(w, `not-json-at-all`)
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
	res := r.Execute(context.Background(), provider.OpResponses, "m", responsesReq())
	if res.Success {
		t.Fatal("expected non-success: the chat reply could not be wrapped as a Responses object")
	}
	if len(res.Attempts) != 1 || res.Attempts[0].Outcome != "bad_output" {
		t.Fatalf("unexpected attempts: %+v", res.Attempts)
	}
	if r.blackout.Blocked("n1", "real-model") {
		t.Errorf("bad output must not black out the normal key")
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
	if res.Status != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (policy block must not passthrough as 200)", res.Status)
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
