package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maiGoLLMRouter/inflight"
	"maiGoLLMRouter/logstore"
	"maiGoLLMRouter/router"
)

const (
	tRole    = `{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`
	tContent = `{"choices":[{"index":0,"delta":{"content":"Hi"}}]}`
	tFinish  = `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
)

func writeEvents(w http.ResponseWriter, items ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, it := range items {
		fmt.Fprintf(w, "data: %s\n\n", it)
		w.(http.Flusher).Flush()
	}
}

// routerServer runs the API, as main does, against one OpenAI-kind provider
// at backendURL serving model "m". providerExtra goes into the provider block.
func routerServer(t *testing.T, backendURL, providerExtra string, logs *logstore.Store) *httptest.Server {
	t.Helper()
	cfg := loadCfg(t, fmt.Sprintf(`
[server]
client_keys = ["sk-test"]

[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["sk-upstream"]
%s

[model."m"]
targets = ["openai/real-model"]
`, backendURL, providerExtra))
	srv := New(cfg, router.New(cfg), logs, inflight.New())
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(srv.WithCORS(mux))
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, ts *httptest.Server, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

const streamChat = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func TestStreamRequestGetsEventStream(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEvents(w, tRole, tContent, tFinish, "[DONE]")
	}))
	defer backend.Close()
	resp := post(t, routerServer(t, backend.URL, "", nil), "/v1/chat/completions", streamChat)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for k, want := range map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no"} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
		t.Errorf("body = %q", body)
	}
}

func TestStreamFlushesEachEvent(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEvents(w, tRole, tContent)
		<-release
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", tFinish)
	}))
	defer backend.Close()
	defer close(release)
	resp := post(t, routerServer(t, backend.URL, "", nil), "/v1/chat/completions", streamChat)

	got := make(chan string, 1)
	go func() {
		rd := bufio.NewReader(resp.Body)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			if strings.Contains(line, `"content":"Hi"`) {
				got <- line
				return
			}
		}
	}()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the content event did not reach the client before the stream ended")
	}
}

func TestStreamExhaustedIsJSONError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	}))
	defer backend.Close()
	resp := post(t, routerServer(t, backend.URL, "", nil), "/v1/chat/completions", streamChat)
	if resp.StatusCode != 500 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d, content type = %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestStreamSkipsCoercion(t *testing.T) {
	var outbound map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{}}\n\n")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n")
		fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hi\"}]}]}}\n\n")
	}))
	defer backend.Close()
	// Coercion is on by default.
	body := `{"model":"m","stream":true,"input":[{"role":"user","content":"hi"},{"role":"developer","content":"my calendar"}]}`
	resp := post(t, routerServer(t, backend.URL, "supports_responses = true", nil), "/v1/responses", body)
	_, _ = io.ReadAll(resp.Body)
	input, _ := outbound["input"].([]any)
	last, _ := input[len(input)-1].(map[string]any)
	if last["role"] != "developer" || last["content"] != "my calendar" {
		t.Errorf("last input item = %v, want the developer message unchanged", last)
	}
}

func TestStreamStringTrueIsNotStreamed(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer backend.Close()
	resp := post(t, routerServer(t, backend.URL, "", nil), "/v1/chat/completions", `{"model":"m","stream":"true","messages":[{"role":"user","content":"hi"}]}`)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
}

func TestStreamOnEmbeddingsIgnored(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"embedding":[0.1]}]}`)
	}))
	defer backend.Close()
	resp := post(t, routerServer(t, backend.URL, "", nil), "/v1/embeddings", `{"model":"m","stream":true,"input":"hi"}`)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" || resp.StatusCode != 200 {
		t.Errorf("status = %d, content type = %q", resp.StatusCode, ct)
	}
}

func TestStreamLoggedWithStreamFlag(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeEvents(w, tRole, tContent, tFinish, "[DONE]")
	}))
	defer backend.Close()
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resp := post(t, routerServer(t, backend.URL, "", logs), "/v1/chat/completions", streamChat)
	_, _ = io.ReadAll(resp.Body)

	// The log is written once the handler returns, just after the stream ends.
	deadline := time.Now().Add(2 * time.Second)
	for len(logs.Recent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	recent := logs.Recent()
	if len(recent) == 0 {
		t.Fatal("no log entry written")
	}
	raw, err := logs.ReadEntry(recent[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Stream   bool `json:"stream"`
		Response struct {
			Object  string `json:"object"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
	if !entry.Stream || entry.Response.Object != "chat.completion" || entry.Response.Choices[0].Message.Content != "Hi" {
		t.Errorf("entry = %s", raw)
	}
}

func TestChatLogHasNoAudio(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"heard"}}]}`)
	}))
	defer backend.Close()
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clip := "QUJDREVGR0hJSktMTU5PUA=="
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"input_audio","input_audio":{"data":"` + clip + `","format":"wav"}}]}]}`
	resp := post(t, routerServer(t, backend.URL, "", logs), "/v1/chat/completions", body)
	_, _ = io.ReadAll(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	for len(logs.Recent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	recent := logs.Recent()
	if len(recent) == 0 {
		t.Fatal("no log entry")
	}
	raw, err := logs.ReadEntry(recent[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), clip) {
		t.Error("the log holds the audio bytes")
	}
	var entry struct {
		Request struct {
			Messages []struct {
				Content []struct {
					InputAudio struct {
						Data string `json:"data"`
					} `json:"input_audio"`
				} `json:"content"`
			} `json:"messages"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatal(err)
	}
	if got := entry.Request.Messages[0].Content[1].InputAudio.Data; got != "<16 bytes audio/wav>" {
		t.Errorf("logged audio = %q, want <16 bytes audio/wav>", got)
	}
	if recent[0].RequestPreview != "hi" {
		t.Errorf("preview = %q, want hi", recent[0].RequestPreview)
	}
}
