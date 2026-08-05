package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"maiGoLLMRouter/config"
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
