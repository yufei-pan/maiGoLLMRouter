package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"maiGoLLMRouter/provider"
)

// recordingSink records what a streamed request sends to the client. With
// failAfter > 0, writes after that many succeed fail, as for a client gone.
type recordingSink struct {
	mu        sync.Mutex
	commits   int
	frames    []provider.Frame
	failAfter int
	writes    int
	early     bool // a write came before the commit
}

func (s *recordingSink) Commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits++
	return nil
}

func (s *recordingSink) Write(f provider.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commits == 0 {
		s.early = true
	}
	s.writes++
	if s.failAfter > 0 && s.writes > s.failAfter {
		return errors.New("client gone")
	}
	s.frames = append(s.frames, f)
	return nil
}

func (s *recordingSink) data() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.frames))
	for i, f := range s.frames {
		out[i] = string(f.Data)
	}
	return out
}

// writeSSE writes each item as one SSE data event and flushes.
func writeSSE(w http.ResponseWriter, items ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, it := range items {
		fmt.Fprintf(w, "data: %s\n\n", it)
		w.(http.Flusher).Flush()
	}
}

const (
	sRole    = `{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`
	sContent = `{"choices":[{"index":0,"delta":{"content":"Hi"}}]}`
	sFinish  = `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	sFilter  = `{"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`
)

// goodStream answers with a complete, normal stream.
func goodStream(w http.ResponseWriter) { writeSSE(w, sRole, sContent, sFinish, "[DONE]") }

// streamCfg is one OpenAI-kind provider with a normal key n1 and a fallback
// key f1, serving model "m"; extra goes into [server].
func streamCfg(t *testing.T, url, extra string) *Router {
	t.Helper()
	return New(loadCfg(t, fmt.Sprintf(`
[server]
%s

[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["n1"]
fallback_keys = ["f1"]

[model."m"]
targets = ["openai/real-model"]
`, extra, url)))
}

func streamReq() map[string]any {
	b := chatReq()
	b["stream"] = true
	return b
}

func TestStreamCommitsOnFirstOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { goodStream(w) }))
	defer srv.Close()
	sink := &recordingSink{}
	res := streamCfg(t, srv.URL, "").ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), sink)
	if !res.Committed || !res.Success || sink.commits != 1 || sink.early {
		t.Fatalf("committed=%v success=%v commits=%d early=%v attempts=%+v", res.Committed, res.Success, sink.commits, sink.early, res.Attempts)
	}
	got := sink.data()
	if len(got) != 4 || got[0] != sRole || got[1] != sContent || got[3] != "[DONE]" {
		t.Errorf("frames = %q", got)
	}
}

func TestStreamFailsOverBeforeOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) == "n1" {
			writeSSE(w, sRole, `{"error":{"code":503,"message":"busy"}}`)
			return
		}
		goodStream(w)
	}))
	defer srv.Close()
	r := streamCfg(t, srv.URL, "")
	sink := &recordingSink{}
	res := r.ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), sink)
	if !res.Success || len(res.Attempts) != 2 || res.Attempts[0].Outcome != "provider_error" {
		t.Fatalf("success=%v attempts=%+v", res.Success, res.Attempts)
	}
	if !r.blackout.Blocked("n1", "real-model") {
		t.Error("n1 should be blacked out")
	}
	for _, d := range sink.data() {
		if strings.Contains(d, "busy") {
			t.Errorf("the failed attempt's error reached the client: %s", d)
		}
	}
}

func TestStreamErrorChunk400DoesNotBlackOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"error":{"code":400,"message":"bad request"}}`)
	}))
	defer srv.Close()
	r := streamCfg(t, srv.URL, "")
	res := r.ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), &recordingSink{})
	if res.Committed || res.Status != 400 {
		t.Fatalf("committed=%v status=%d", res.Committed, res.Status)
	}
	if r.blackout.Blocked("n1", "real-model") {
		t.Error("a 400 must not black out the key")
	}
}

func TestStreamNoFailoverAfterCommit(t *testing.T) {
	errChunk := `{"error":{"code":502,"message":"upstream died"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) == "f1" {
			t.Error("the fallback key must not be tried after commit")
		}
		writeSSE(w, sRole, sContent, errChunk)
	}))
	defer srv.Close()
	r := streamCfg(t, srv.URL, "")
	sink := &recordingSink{}
	res := r.ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), sink)
	if !res.Committed || res.Success || len(res.Attempts) != 1 || res.Attempts[0].Outcome != "provider_error" {
		t.Fatalf("committed=%v success=%v attempts=%+v", res.Committed, res.Success, res.Attempts)
	}
	got := sink.data()
	if got[len(got)-1] != errChunk {
		t.Errorf("last frame = %s, want the downstream error", got[len(got)-1])
	}
	for _, d := range got {
		if d == "[DONE]" {
			t.Error("no [DONE] after an error")
		}
	}
	if !r.blackout.Blocked("n1", "real-model") {
		t.Error("n1 should be blacked out after failing mid-stream")
	}
}

func TestStreamBrokenAfterCommitWritesRouterError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sRole, sContent)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer srv.Close()
	sink := &recordingSink{}
	res := streamCfg(t, srv.URL, "").ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), sink)
	if !res.Committed || res.Success {
		t.Fatalf("committed=%v success=%v", res.Committed, res.Success)
	}
	got := sink.data()
	var last struct {
		Error struct {
			Type string `json:"type"`
			Code int    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(got[len(got)-1]), &last); err != nil || last.Error.Type != "upstream_error" || last.Error.Code != 502 {
		t.Errorf("last frame = %s, want the router's 502 upstream_error", got[len(got)-1])
	}
}

func TestStreamEmptyStreamIsBadOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) == "n1" {
			writeSSE(w, sRole, "[DONE]")
			return
		}
		goodStream(w)
	}))
	defer srv.Close()
	r := streamCfg(t, srv.URL, "")
	res := r.ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), &recordingSink{})
	if !res.Success || len(res.Attempts) != 2 || res.Attempts[0].Outcome != "bad_output" {
		t.Fatalf("success=%v attempts=%+v", res.Success, res.Attempts)
	}
	if r.blackout.Blocked("n1", "real-model") {
		t.Error("an empty stream must not black out the key")
	}
}

func TestStreamExhaustedNeverReturns2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeSSE(w, sRole, "[DONE]") }))
	defer srv.Close()
	res := streamCfg(t, srv.URL, "").ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), &recordingSink{})
	if res.Committed || res.Status != 502 || !strings.Contains(string(res.Body), "upstream_error") {
		t.Fatalf("committed=%v status=%d body=%s", res.Committed, res.Status, res.Body)
	}
}

func TestStreamExhaustedUsesDownstreamErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"error":{"code":429,"message":"Rate limit reached"}}`)
	}))
	defer srv.Close()
	res := streamCfg(t, srv.URL, "").ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), &recordingSink{})
	if res.Committed || res.Status != 429 || !strings.Contains(string(res.Body), "Rate limit reached") {
		t.Fatalf("committed=%v status=%d body=%s", res.Committed, res.Status, res.Body)
	}
}

func TestStreamAllProhibitedSendsRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeSSE(w, sRole, sFilter, "[DONE]") }))
	defer srv.Close()
	sink := &recordingSink{}
	res := streamCfg(t, srv.URL, "").ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), sink)
	if !res.Committed || res.Success || res.Status != 200 {
		t.Fatalf("committed=%v success=%v status=%d attempts=%+v", res.Committed, res.Success, res.Status, res.Attempts)
	}
	got := sink.data()
	if len(got) != 3 || !strings.Contains(got[1], `"finish_reason":"content_filter"`) || got[2] != "[DONE]" {
		t.Errorf("frames = %q", got)
	}
}

func TestStreamJSONReplyIsCheckedThenStreamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if bearer(r) == "n1" {
			fmt.Fprint(w, chatBadFinish)
			return
		}
		fmt.Fprint(w, chatStop)
	}))
	defer srv.Close()
	sink := &recordingSink{}
	res := streamCfg(t, srv.URL, "").ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), sink)
	if !res.Success || len(res.Attempts) != 2 || res.Attempts[0].Outcome != "bad_output" {
		t.Fatalf("success=%v attempts=%+v", res.Success, res.Attempts)
	}
	if got := sink.data(); got[len(got)-1] != "[DONE]" {
		t.Errorf("frames = %q", got)
	}
}

func TestStreamingOffUsesBufferedPath(t *testing.T) {
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, chatStop)
	}))
	defer srv.Close()
	sink := &recordingSink{}
	res := streamCfg(t, srv.URL, "streaming = false").ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), sink)
	if !res.Success || !res.Committed {
		t.Fatalf("success=%v committed=%v attempts=%+v", res.Success, res.Committed, res.Attempts)
	}
	if _, has := outbound["stream"]; has {
		t.Errorf("buffered path sent stream downstream: %v", outbound)
	}
	if got := sink.data(); got[len(got)-1] != "[DONE]" {
		t.Errorf("frames = %q", got)
	}
}

func TestStreamClientGoneStopsDownstream(t *testing.T) {
	gone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sRole, sContent)
		for {
			select {
			case <-r.Context().Done():
				close(gone)
				return
			case <-time.After(20 * time.Millisecond):
				fmt.Fprintf(w, "data: %s\n\n", sContent)
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer srv.Close()
	r := streamCfg(t, srv.URL, "")
	res := r.ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), &recordingSink{failAfter: 2})
	select {
	case <-gone:
	case <-time.After(time.Second):
		t.Fatal("the downstream kept streaming after the client went away")
	}
	if len(res.Attempts) != 1 || res.Attempts[0].Outcome != "canceled" {
		t.Errorf("attempts = %+v", res.Attempts)
	}
	if r.blackout.Blocked("n1", "real-model") {
		t.Error("a client going away must not black out the key")
	}
}

type commitRecorder struct {
	provider, model string
}

func (c *commitRecorder) OnAttempt(Attempt)               {}
func (c *commitRecorder) OnCommit(provider, model string) { c.provider, c.model = provider, model }

func TestStreamCommitNotifiesObserver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { goodStream(w) }))
	defer srv.Close()
	obs := &commitRecorder{}
	ctx := WithAttemptObserver(context.Background(), obs)
	streamCfg(t, srv.URL, "").ExecuteStream(ctx, provider.OpChat, "m", streamReq(), &recordingSink{})
	if obs.provider != "openai" || obs.model != "real-model" {
		t.Errorf("OnCommit(%q, %q)", obs.provider, obs.model)
	}
}

func TestStreamFirstOutputTimeoutFailsOver(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) == "n1" {
			writeSSE(w, sRole)
			<-r.Context().Done()
			return
		}
		goodStream(w)
	}))
	defer srv.Close()
	r := New(loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "openai"
kind = "openai"
base_url = %q
timeout = "100ms"
keys = ["n1"]
fallback_keys = ["f1"]

[model."m"]
targets = ["openai/real-model"]
`, srv.URL)))
	res := r.ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), &recordingSink{})
	if !res.Success || len(res.Attempts) != 2 {
		t.Fatalf("success=%v attempts=%+v", res.Success, res.Attempts)
	}
	if a := res.Attempts[0]; a.Outcome != "provider_error" || a.HTTPStatus != 504 {
		t.Errorf("first attempt = %+v, want provider_error 504", a)
	}
}

// TestStreamErrorsDoNotLeakProviderAddress verifies a transport failure's
// details (which name the provider's URL) stay in the attempt log: the client
// gets a fixed message, before and after commit.
func TestStreamErrorsDoNotLeakProviderAddress(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing listens there any more: every call fails to connect
	res := streamCfg(t, deadURL, "").ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), &recordingSink{})
	host := strings.TrimPrefix(deadURL, "http://")
	if res.Committed || strings.Contains(string(res.Body), host) {
		t.Errorf("exhaustion body leaks the provider address: %s", res.Body)
	}
	if !strings.Contains(res.Attempts[0].Error, host) {
		t.Errorf("the attempt log should keep the detail, got %q", res.Attempts[0].Error)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sRole, sContent)
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	}))
	defer srv.Close()
	sink := &recordingSink{}
	streamCfg(t, srv.URL, "").ExecuteStream(context.Background(), provider.OpChat, "m", streamReq(), sink)
	got := sink.data()
	if last := got[len(got)-1]; strings.Contains(last, strings.TrimPrefix(srv.URL, "http://")) {
		t.Errorf("error frame leaks the provider address: %s", last)
	}
}

// TestStreamCanceledBeforeCommit verifies a caller that goes away while the
// provider has sent no output stops the run at once: nothing is committed,
// nothing blacked out, no other key tried.
func TestStreamCanceledBeforeCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) == "f1" {
			t.Error("no other key should be tried after the caller went away")
		}
		writeSSE(w, sRole)
		<-r.Context().Done()
	}))
	defer srv.Close()
	r := streamCfg(t, srv.URL, "")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	done := make(chan *Result, 1)
	go func() { done <- r.ExecuteStream(ctx, provider.OpChat, "m", streamReq(), &recordingSink{}) }()
	var res *Result
	select {
	case res = <-done:
	case <-time.After(time.Second):
		t.Fatal("the run did not stop when the caller went away")
	}
	if res.Committed || len(res.Attempts) != 1 || res.Attempts[0].Outcome != "canceled" {
		t.Fatalf("committed=%v attempts=%+v", res.Committed, res.Attempts)
	}
	if r.blackout.Blocked("n1", "real-model") {
		t.Error("a caller going away must not black out the key")
	}
}

// cancelAfterFirstWrite cancels the request once the first frame went out.
type cancelAfterFirstWrite struct {
	recordingSink
	cancel context.CancelFunc
}

func (s *cancelAfterFirstWrite) Write(f provider.Frame) error {
	err := s.recordingSink.Write(f)
	s.cancel()
	return err
}

// TestStreamCanceledAfterCommit verifies a caller that goes away while the
// router waits for the next piece stops the stream at once, without blackout.
func TestStreamCanceledAfterCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, sContent)
		<-r.Context().Done()
	}))
	defer srv.Close()
	r := streamCfg(t, srv.URL, "")
	ctx, cancel := context.WithCancel(context.Background())
	sink := &cancelAfterFirstWrite{cancel: cancel}
	done := make(chan *Result, 1)
	go func() { done <- r.ExecuteStream(ctx, provider.OpChat, "m", streamReq(), sink) }()
	var res *Result
	select {
	case res = <-done:
	case <-time.After(time.Second):
		t.Fatal("the stream did not stop when the caller went away")
	}
	if !res.Committed || len(res.Attempts) != 1 || res.Attempts[0].Outcome != "canceled" {
		t.Fatalf("committed=%v attempts=%+v", res.Committed, res.Attempts)
	}
	if r.blackout.Blocked("n1", "real-model") {
		t.Error("a caller going away must not black out the key")
	}
}
