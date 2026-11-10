package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"maiGoLLMRouter/provider"
)

const (
	anStart     = `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-opus-5-5","usage":{"input_tokens":3,"output_tokens":1}}}`
	anTextStart = `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	anHi        = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`
	anThere     = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" there"}}`
	anTextStop  = `{"type":"content_block_stop","index":0}`
	anEndTurn   = `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`
	anRefusal   = `{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":0}}`
	anStop      = `{"type":"message_stop"}`
	anPing      = `{"type":"ping"}`
	anOverload  = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`
)

// writeAnthropic writes each event under its own event name, as Anthropic does.
// The first call sends the event-stream headers.
func writeAnthropic(w http.ResponseWriter, events ...string) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}
	for _, e := range events {
		var m struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &m)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.Type, e)
		w.(http.Flusher).Flush()
	}
}

// anthropicCfg is one Anthropic-kind provider with a normal key n1 and a
// fallback key f1, serving model "m".
func anthropicCfg(t *testing.T, url string) *Router {
	t.Helper()
	return New(loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "claude"
kind = "anthropic"
base_url = %q
keys = ["n1"]
fallback_keys = ["f1"]

[model."m"]
targets = ["claude/claude-opus-5-5"]
`, url)))
}

// textSink records like recordingSink and closes text when the "Hi" chunk is written.
type textSink struct {
	recordingSink
	once sync.Once
	text chan struct{}
}

func (s *textSink) Write(f provider.Frame) error {
	err := s.recordingSink.Write(f)
	if strings.Contains(string(f.Data), `"content":"Hi"`) {
		s.once.Do(func() { close(s.text) })
	}
	return err
}

// TestStreamAnthropicTextArrivesBeforeTheReplyEnds verifies a Claude reply is
// streamed, not buffered: the first text reaches the client while Anthropic is
// still holding the stream open.
func TestStreamAnthropicTextArrivesBeforeTheReplyEnds(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeAnthropic(w, anStart, anTextStart, anHi)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		writeAnthropic(w, anThere, anTextStop, anEndTurn, anStop)
	}))
	defer srv.Close()
	defer letGo()

	sink := &textSink{text: make(chan struct{})}
	done := make(chan *Result, 1)
	go func() {
		done <- anthropicCfg(t, srv.URL).ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), sink)
	}()
	select {
	case <-sink.text:
	case res := <-done:
		t.Fatalf("the run ended before the provider finished: %+v", res.Attempts)
	case <-time.After(5 * time.Second):
		t.Fatal("the first text did not reach the client while the provider held the stream open")
	}
	letGo()
	var res *Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not finish after the provider did")
	}
	if !res.Success || !res.Committed || len(res.Attempts) != 1 || res.Attempts[0].Outcome != "success" {
		t.Fatalf("success=%v committed=%v attempts=%+v", res.Success, res.Committed, res.Attempts)
	}
	got := sink.data()
	if len(got) != 5 || !strings.Contains(got[1], `"Hi"`) || !strings.Contains(got[2], `" there"`) ||
		!strings.Contains(got[3], `"finish_reason":"stop"`) || got[4] != "[DONE]" {
		t.Errorf("frames = %q", got)
	}
}

// TestStreamAnthropicErrorEventBeforeOutputFailsOver verifies an error event
// before any output (Anthropic overloaded after answering 200) is failed over
// like a 529 reply, and the client never sees it.
func TestStreamAnthropicErrorEventBeforeOutputFailsOver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "n1" {
			writeAnthropic(w, anStart, anPing, anOverload)
			return
		}
		writeAnthropic(w, anStart, anTextStart, anHi, anTextStop, anEndTurn, anStop)
	}))
	defer srv.Close()
	r := anthropicCfg(t, srv.URL)
	sink := &recordingSink{}
	res := r.ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), sink)
	if !res.Success || len(res.Attempts) != 2 {
		t.Fatalf("success=%v attempts=%+v", res.Success, res.Attempts)
	}
	if a := res.Attempts[0]; a.Outcome != "provider_error" || a.HTTPStatus != 529 {
		t.Errorf("first attempt = %+v, want provider_error 529", a)
	}
	if !r.blackout.Blocked("n1", "claude-opus-5-5") {
		t.Error("n1 should be blacked out")
	}
	for _, d := range sink.data() {
		if strings.Contains(d, "Overloaded") {
			t.Errorf("the failed attempt's error reached the client: %s", d)
		}
	}
}

// TestStreamAnthropicRefusalIsAPolicyBlock verifies a refusal before any output
// skips the target without blackout, and the client gets a refusal.
func TestStreamAnthropicRefusalIsAPolicyBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeAnthropic(w, anStart, anRefusal, anStop)
	}))
	defer srv.Close()
	r := anthropicCfg(t, srv.URL)
	sink := &recordingSink{}
	res := r.ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), sink)
	if len(res.Attempts) != 1 || res.Attempts[0].Outcome != "prohibited_content" {
		t.Fatalf("attempts = %+v, want one prohibited_content (the fallback key is skipped too)", res.Attempts)
	}
	if r.blackout.Blocked("n1", "claude-opus-5-5") {
		t.Error("a refusal must not black out the key")
	}
	if got := sink.data(); !res.Committed || len(got) < 2 || !strings.Contains(got[len(got)-2], `"content_filter"`) {
		t.Errorf("committed=%v frames=%q, want a refusal stream", res.Committed, got)
	}
}
