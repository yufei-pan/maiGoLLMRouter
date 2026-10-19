package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// sseChunks writes each item as one SSE event ("data: <item>\n\n"), flushing
// after each. An item starting with ":" is written as a comment line.
func sseChunks(w http.ResponseWriter, items ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, it := range items {
		if len(it) > 0 && it[0] == ':' {
			fmt.Fprintf(w, "%s\n\n", it)
		} else {
			fmt.Fprintf(w, "data: %s\n\n", it)
		}
		w.(http.Flusher).Flush()
	}
}

// streamServer records the outbound body and answers with handler.
func streamServer(t *testing.T, outbound *map[string]any, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if outbound != nil {
			_ = json.NewDecoder(r.Body).Decode(outbound)
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chatStreamBody(extra map[string]any) map[string]any {
	b := map[string]any{
		"model":    "inbound",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func openChatStream(t *testing.T, url string, body map[string]any, timeout time.Duration) (*Stream, *Response) {
	t.Helper()
	s, resp, err := CallStream(context.Background(), &http.Client{}, "openai", url, "k", timeout, Request{Op: OpChat, Model: "real", Body: body})
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	return s, resp
}

// drain reads frames until the stream ends, returning them and the final error
// (nil after a clean io.EOF).
func drain(t *testing.T, s *Stream) ([]Frame, error) {
	t.Helper()
	var frames []Frame
	for {
		f, err := s.Next()
		if err == io.EOF {
			return frames, nil
		}
		if err != nil {
			return frames, err
		}
		frames = append(frames, f)
	}
}

const (
	roleChunk    = `{"id":"c1","object":"chat.completion.chunk","model":"real","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`
	contentChunk = `{"id":"c1","object":"chat.completion.chunk","model":"real","choices":[{"index":0,"delta":{"content":"Hi"}}]}`
	finishChunk  = `{"id":"c1","object":"chat.completion.chunk","model":"real","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	usageChunk   = `{"id":"c1","object":"chat.completion.chunk","model":"real","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
)

func TestOpenAIChatStreamPassesChunksThrough(t *testing.T) {
	var outbound map[string]any
	sent := []string{roleChunk, contentChunk, finishChunk, usageChunk, "[DONE]"}
	srv := streamServer(t, &outbound, func(w http.ResponseWriter, r *http.Request) { sseChunks(w, sent...) })

	s, _ := openChatStream(t, srv.URL, chatStreamBody(map[string]any{"stream_options": map[string]any{"include_usage": true}}), time.Minute)
	frames, err := drain(t, s)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if len(frames) != len(sent) {
		t.Fatalf("got %d frames, want %d", len(frames), len(sent))
	}
	for i := range sent {
		if string(frames[i].Data) != sent[i] || frames[i].Event != "" {
			t.Errorf("frame %d = %q (event %q), want %q", i, frames[i].Data, frames[i].Event, sent[i])
		}
	}
	if outbound["stream"] != true || outbound["model"] != "real" {
		t.Errorf("outbound stream/model = %v/%v", outbound["stream"], outbound["model"])
	}
	if so, _ := outbound["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("outbound stream_options = %v, want include_usage", outbound["stream_options"])
	}
}

func TestOpenAIChatStreamStripsUsageClientDidNotAsk(t *testing.T) {
	var outbound map[string]any
	withUsage := `{"id":"c1","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`
	srv := streamServer(t, &outbound, func(w http.ResponseWriter, r *http.Request) {
		sseChunks(w, roleChunk, withUsage, usageChunk, "[DONE]")
	})

	s, _ := openChatStream(t, srv.URL, chatStreamBody(nil), time.Minute)
	frames, err := drain(t, s)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if so, _ := outbound["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("outbound must still ask for usage, got %v", outbound["stream_options"])
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want role, content, [DONE] (usage-only chunk dropped): %q", len(frames), frames)
	}
	second := frameJSON(t, frames[1])
	if _, has := second["usage"]; has {
		t.Errorf("forwarded chunk still has usage: %s", frames[1].Data)
	}
	if c := decodeCompletion(t, s.State().Body); c.Usage.PromptTokens != 3 {
		t.Errorf("State().Body usage.prompt_tokens = %d, want 3", c.Usage.PromptTokens)
	}
}

func TestOpenAIChatStreamErrorChunk(t *testing.T) {
	errData := `{"error":{"message":"Rate limit","code":429}}`
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) { sseChunks(w, roleChunk, errData) })

	s, _ := openChatStream(t, srv.URL, chatStreamBody(nil), time.Minute)
	_, err := drain(t, s)
	var se *StreamError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StreamError", err)
	}
	if se.Status != 429 || se.Frame == nil || string(se.Frame.Data) != errData {
		t.Errorf("StreamError = %+v", se)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(se.Body, &body) != nil || body.Error.Message != "Rate limit" {
		t.Errorf("Body = %s", se.Body)
	}
}

func TestOpenAIChatStreamDropsComments(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sseChunks(w, ": OPENROUTER PROCESSING", roleChunk, ": OPENROUTER PROCESSING", contentChunk, finishChunk, "[DONE]")
	})
	s, _ := openChatStream(t, srv.URL, chatStreamBody(nil), time.Minute)
	frames, err := drain(t, s)
	if err != nil || len(frames) != 4 {
		t.Fatalf("frames = %q, err = %v; want 4 frames", frames, err)
	}
}

func TestOpenAIChatStreamAddsMissingDone(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) { sseChunks(w, roleChunk, contentChunk, finishChunk) })
	s, _ := openChatStream(t, srv.URL, chatStreamBody(nil), time.Minute)
	frames, err := drain(t, s)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if last := frames[len(frames)-1]; string(last.Data) != "[DONE]" {
		t.Errorf("last frame = %q, want [DONE]", last.Data)
	}
}

func TestOpenAIChatStreamBrokenWithoutFinish(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) { sseChunks(w, roleChunk, contentChunk) })
	s, _ := openChatStream(t, srv.URL, chatStreamBody(nil), time.Minute)
	_, err := drain(t, s)
	var se *StreamError
	if !errors.As(err, &se) || se.Status != 502 {
		t.Fatalf("err = %v, want *StreamError with status 502", err)
	}
}

func TestOpenAIChatStreamSkipsNonJSONData(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sseChunks(w, roleChunk, "hello", contentChunk, finishChunk, "[DONE]")
	})
	s, _ := openChatStream(t, srv.URL, chatStreamBody(nil), time.Minute)
	frames, err := drain(t, s)
	if err != nil || len(frames) != 4 {
		t.Fatalf("frames = %q, err = %v; want 4 frames without the non-JSON one", frames, err)
	}
}

// blockAfter sends items, then holds the connection open until the client goes away.
func blockAfter(items ...string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		sseChunks(w, items...)
		<-r.Context().Done()
	}
}

func nextWithin(t *testing.T, s *Stream, d time.Duration) (Frame, error) {
	t.Helper()
	type result struct {
		f   Frame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := s.Next()
		ch <- result{f, err}
	}()
	select {
	case r := <-ch:
		return r.f, r.err
	case <-time.After(d):
		t.Fatalf("Next did not return within %v", d)
		return Frame{}, nil
	}
}

func TestOpenAIChatStreamFirstOutputTimeout(t *testing.T) {
	srv := streamServer(t, nil, blockAfter(roleChunk))
	s, _ := openChatStream(t, srv.URL, chatStreamBody(nil), 100*time.Millisecond)
	defer s.Close()
	if _, err := nextWithin(t, s, time.Second); err != nil {
		t.Fatalf("role chunk: %v", err)
	}
	_, err := nextWithin(t, s, time.Second)
	var se *StreamError
	if !errors.As(err, &se) || !se.Timeout || se.Status != 504 {
		t.Fatalf("err = %v, want a 504 timeout", err)
	}
	if string(se.Body) != `{"error":{"message":"no output from the provider in time","type":"upstream_error"}}` {
		t.Errorf("Body = %s", se.Body)
	}
}

func TestOpenAIChatStreamIdleTimeoutAfterOutput(t *testing.T) {
	srv := streamServer(t, nil, blockAfter(contentChunk))
	s, _ := openChatStream(t, srv.URL, chatStreamBody(nil), 100*time.Millisecond)
	defer s.Close()
	if _, err := nextWithin(t, s, time.Second); err != nil {
		t.Fatalf("content chunk: %v", err)
	}
	if !s.State().Output {
		t.Fatal("content should count as output")
	}
	_, err := nextWithin(t, s, time.Second)
	var se *StreamError
	if !errors.As(err, &se) || !se.Timeout {
		t.Fatalf("err = %v, want an idle timeout", err)
	}
}

func TestOpenAIChatStreamCommentsKeepIdleAlive(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sseChunks(w, contentChunk)
		for range 6 {
			time.Sleep(50 * time.Millisecond)
			fmt.Fprint(w, ": keep-alive\n\n")
			w.(http.Flusher).Flush()
		}
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", finishChunk)
	})
	s, _ := openChatStream(t, srv.URL, chatStreamBody(nil), 100*time.Millisecond)
	if _, err := drain(t, s); err != nil {
		t.Fatalf("keep-alive comments should hold the stream open: %v", err)
	}
}

func TestCallStreamJSONReplyComesBackAsResponse(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`)
	})
	s, resp := openChatStream(t, srv.URL, chatStreamBody(nil), time.Minute)
	if s != nil || resp == nil {
		t.Fatalf("want a Response, got stream=%v resp=%v", s, resp)
	}
	if !resp.OK() || resp.FinishReason != "stop" || !resp.HasContent {
		t.Errorf("Response = %+v", resp)
	}
}

func TestCallStreamHTTPErrorComesBackAsResponse(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	})
	s, resp := openChatStream(t, srv.URL, chatStreamBody(nil), time.Minute)
	if s != nil || resp == nil || resp.HTTPStatus != 500 {
		t.Fatalf("want a 500 Response, got stream=%v resp=%+v", s, resp)
	}
}
