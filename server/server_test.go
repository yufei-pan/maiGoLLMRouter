package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"maiGoLLMRouter/config"
	"maiGoLLMRouter/inflight"
	"maiGoLLMRouter/logstore"
	"maiGoLLMRouter/router"
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

func TestResponsesRouteRegistered(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"id":"r1","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`)
	}))
	defer backend.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[server]
client_keys = ["sk-test"]

[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["sk-upstream"]
supports_responses = true

[model."m"]
targets = ["openai/real-model"]
`, backend.URL))

	rt := router.New(cfg)
	srv := New(cfg, rt, nil, nil)
	mux := http.NewServeMux()
	srv.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
	var parsed map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed["object"] != "response" || parsed["status"] != "completed" {
		t.Fatalf("unexpected Responses JSON: %#v", parsed)
	}
}

func TestIngestCoercesTrailingAssistantWhenEnabled(t *testing.T) {
	var outbound map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer backend.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[server]
client_keys = ["sk-test"]
# coerce_trailing_assistant omitted => true

[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["sk-upstream"]

[model."m"]
targets = ["openai/gpt-4o"]
`, backend.URL))

	rt := router.New(cfg)
	srv := New(cfg, rt, nil, nil)
	mux := http.NewServeMux()
	srv.Register(mux)

	reqBody := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"I 我"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.Bytes())
	}
	last := outbound["messages"].([]any)[1].(map[string]any)
	if last["role"] != "user" || last["content"] != "you 你" {
		t.Fatalf("outbound last=%#v", last)
	}
}

func TestIngestSkipsCoercionWhenDisabled(t *testing.T) {
	var outbound map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer backend.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[server]
client_keys = ["sk-test"]
coerce_trailing_assistant = false

[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["sk-upstream"]

[model."m"]
targets = ["openai/gpt-4o"]
`, backend.URL))

	rt := router.New(cfg)
	srv := New(cfg, rt, nil, nil)
	mux := http.NewServeMux()
	srv.Register(mux)

	reqBody := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"I"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	last := outbound["messages"].([]any)[1].(map[string]any)
	if last["role"] != "assistant" || last["content"] != "I" {
		t.Fatalf("outbound last=%#v", last)
	}
}

// corsServer is the router's handler as main.go wires it, with the API and a stand-in web UI route.
func corsServer(t *testing.T, server string) http.Handler {
	t.Helper()
	cfg := loadCfg(t, "[server]\nclient_keys = [\"sk-test\"]\n"+server+`
[[provider]]
name = "openai"
kind = "openai"
base_url = "http://127.0.0.1:1"
keys = ["sk-upstream"]

[model."m"]
targets = ["openai/real-model"]
`)
	srv := New(cfg, router.New(cfg), nil, nil)
	mux := http.NewServeMux()
	srv.Register(mux)
	mux.HandleFunc("/ui/logs", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "[]") })
	return srv.WithCORS(mux)
}

func preflight(h http.Handler, path, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodOptions, path, nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	req.Header.Set("Access-Control-Request-Private-Network", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCORSOnByDefaultForTheAPI(t *testing.T) {
	h := corsServer(t, "")
	rec := preflight(h, "/v1/chat/completions", "https://annai.guide")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("allow origin %q", got)
	}
	if got := strings.ToLower(rec.Header().Get("Access-Control-Allow-Headers")); !strings.Contains(got, "authorization") || !strings.Contains(got, "content-type") {
		t.Errorf("allow headers %q", got)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("allow methods %q", rec.Header().Get("Access-Control-Allow-Methods"))
	}
	if rec.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Error("a browser on a public site asking for a private network gets a yes")
	}
	// A real request, even one refused for its key, carries the header, so the page can read the refusal.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://annai.guide")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("models without a key: %d, allow origin %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestCORSNeverCoversTheWebUI(t *testing.T) {
	h := corsServer(t, "")
	req := httptest.NewRequest(http.MethodGet, "/ui/logs", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("the web UI has no key, so other sites must not read it")
	}
	if rec := preflight(h, "/ui/logs", "https://evil.example"); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("no preflight answer for the web UI either")
	}
}

func TestCORSCanBeTurnedOffOrLimited(t *testing.T) {
	off := corsServer(t, "cors = false\n")
	if rec := preflight(off, "/v1/chat/completions", "https://annai.guide"); rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Code == http.StatusNoContent {
		t.Errorf("cors = false: status %d, allow origin %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	limited := corsServer(t, "cors_origins = [\"https://annai.guide\"]\n")
	rec := preflight(limited, "/v1/chat/completions", "https://annai.guide")
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://annai.guide" || !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("listed origin: status %d, allow origin %q, vary %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"), rec.Header().Get("Vary"))
	}
	if rec := preflight(limited, "/v1/chat/completions", "https://other.example"); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("an origin not listed gets no allow header")
	}
}

// The in-flight view and the index show a chat request's text, taken from the
// body the handler decoded.
func TestChatPreviewFromDecodedBody(t *testing.T) {
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case arrived <- struct{}{}:
		default:
		}
		<-release
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer backend.Close()
	var once sync.Once
	releaseBackend := func() { once.Do(func() { close(release) }) }
	defer releaseBackend() // before backend.Close, which waits for the handler

	cfg := loadCfg(t, fmt.Sprintf(`
[server]
client_keys = ["sk-test"]

[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["sk-upstream"]

[model."m"]
targets = ["openai/real-model"]
`, backend.URL))
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	active := inflight.New()
	mux := http.NewServeMux()
	New(cfg, router.New(cfg), logs, active).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	status := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(raw))
		req.Header.Set("Authorization", "Bearer sk-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			status <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		status <- resp.StatusCode
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the provider")
	}
	live := active.Snapshot()
	if len(live) != 1 {
		t.Fatalf("%d requests in flight, want 1", len(live))
	}
	if want := logstore.EstimateInTokens(json.RawMessage(raw)); live[0].RequestPreview != "hi" || live[0].InTokens != want {
		t.Errorf("in flight: preview %q, in tokens %d; want %q, %d", live[0].RequestPreview, live[0].InTokens, "hi", want)
	}
	releaseBackend()
	if code := <-status; code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	lastLog(t, logs)
	if got := logs.Recent()[0].RequestPreview; got != "hi" {
		t.Errorf("index preview %q, want %q", got, "hi")
	}
}

// A speech request's index row shows the start of its input, as before.
func TestSpeechIndexPreviewUnchanged(t *testing.T) {
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := audioServer(t, `ffmpeg = "/nonexistent/ffmpeg"`, openAIBlock(pcmBackend(t).URL), logs)
	resp := post(t, ts, "/v1/audio/speech", `{"model":"m","input":"hello world, this is long"}`)
	_, _ = io.ReadAll(resp.Body)
	lastLog(t, logs)
	if got := logs.Recent()[0].RequestPreview; got != "hello world, thi" {
		t.Errorf("index preview %q, want %q", got, "hello world, thi")
	}
}

// A chat request with an audio clip is logged with the clip described, not
// its bytes, and its index row shows the request's text.
func TestAudioChatLogScrubbedFromBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"heard"}}]}`)
	}))
	defer backend.Close()
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}]}]}`
	resp := post(t, routerServer(t, backend.URL, "", logs), "/v1/chat/completions", body)
	_, _ = io.ReadAll(resp.Body)
	logged, _ := json.Marshal(lastLog(t, logs)["request"])
	var req struct {
		Messages []struct {
			Content []struct {
				InputAudio struct {
					Data string `json:"data"`
				} `json:"input_audio"`
			} `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(logged, &req)
	if strings.Contains(string(logged), "QUJD") || len(req.Messages) != 1 || len(req.Messages[0].Content) != 2 || req.Messages[0].Content[1].InputAudio.Data != "<3 bytes audio/wav>" {
		t.Errorf("logged request = %s", logged)
	}
	if got := logs.Recent()[0].RequestPreview; got != "what is this" {
		t.Errorf("index preview %q, want %q", got, "what is this")
	}
}

// The log and the index preview hold the request as the client sent it, not
// the copy coercion rewrites for the provider.
func TestLogKeepsRequestBeforeCoercion(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer backend.Close()
	send := func(messages string) (*logstore.Store, map[string]any) {
		logs, err := logstore.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ts := audioServer(t, "coerce_trailing_assistant = true", openAIBlock(backend.URL), logs)
		resp := post(t, ts, "/v1/chat/completions", `{"model":"m","messages":`+messages+`}`)
		_, _ = io.ReadAll(resp.Body)
		return logs, lastLog(t, logs)
	}

	_, entry := send(`[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}]},{"role":"assistant","content":"I am"}]`)
	req, _ := entry["request"].(map[string]any)
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 2 || !reflect.DeepEqual(msgs[1], map[string]any{"role": "assistant", "content": "I am"}) {
		t.Errorf("logged messages = %v, want the last one as sent", msgs)
	}

	logs, _ := send(`[{"role":"assistant","content":"I said"}]`)
	if got := logs.Recent()[0].RequestPreview; got != "I said" {
		t.Errorf("index preview %q, want %q", got, "I said")
	}
}
