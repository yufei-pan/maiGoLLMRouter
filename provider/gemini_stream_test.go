package provider

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func geminiStreamCall(t *testing.T, url string, op Operation, body map[string]any, cache *SignatureCache) (*Stream, *Response) {
	t.Helper()
	req := Request{Op: op, Model: "gemini-3.5-flash", Provider: "google", Signatures: cache, Body: body}
	s, resp, err := CallStream(context.Background(), &http.Client{}, "gemini", url, "k", time.Minute, req)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	return s, resp
}

func geminiChatBodyForStream(usage bool) map[string]any {
	b := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream": true}
	if usage {
		b["stream_options"] = map[string]any{"include_usage": true}
	}
	return b
}

// deltaOf returns the first choice's delta and finish_reason of a chunk frame.
func deltaOf(t *testing.T, f Frame) (map[string]any, any) {
	t.Helper()
	m := frameJSON(t, f)
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return nil, nil
	}
	c, _ := choices[0].(map[string]any)
	d, _ := c["delta"].(map[string]any)
	return d, c["finish_reason"]
}

func TestGeminiStreamTextAndThoughts(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sseChunks(w,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Let me","thought":true}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":" world"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}`)
	})
	s, _ := geminiStreamCall(t, srv.URL, OpChat, geminiChatBodyForStream(true), nil)
	frames, err := drain(t, s)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if len(frames) != 7 {
		t.Fatalf("got %d frames, want 7: %q", len(frames), frames)
	}
	if d, _ := deltaOf(t, frames[0]); d["role"] != "assistant" {
		t.Errorf("frame 0 = %s, want the role", frames[0].Data)
	}
	if d, _ := deltaOf(t, frames[1]); d["reasoning_content"] != "Let me" {
		t.Errorf("frame 1 = %s, want reasoning", frames[1].Data)
	}
	if d, _ := deltaOf(t, frames[2]); d["content"] != "Hello" {
		t.Errorf("frame 2 = %s", frames[2].Data)
	}
	if d, _ := deltaOf(t, frames[3]); d["content"] != " world" {
		t.Errorf("frame 3 = %s", frames[3].Data)
	}
	if _, fin := deltaOf(t, frames[4]); fin != "stop" {
		t.Errorf("frame 4 = %s, want finish stop", frames[4].Data)
	}
	if u, _ := frameJSON(t, frames[5])["usage"].(map[string]any); u["prompt_tokens"] != float64(1) {
		t.Errorf("frame 5 = %s, want the usage chunk", frames[5].Data)
	}
	if string(frames[6].Data) != "[DONE]" {
		t.Errorf("frame 6 = %s, want [DONE]", frames[6].Data)
	}
	c := decodeCompletion(t, s.State().Body)
	if *c.Choices[0].Message.Content != "Hello world" || c.Choices[0].Message.ReasoningContent != "Let me" {
		t.Errorf("Body = %s", s.State().Body)
	}
}

func TestGeminiStreamFunctionCallWhole(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sseChunks(w,
			`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"speak","args":{"text":"hi"}},"thoughtSignature":"sig1"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}]}`)
	})
	cache := NewSignatureCache(10, time.Hour)
	s, _ := geminiStreamCall(t, srv.URL, OpChat, geminiChatBodyForStream(false), cache)
	frames, err := drain(t, s)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	var call map[string]any
	var finish any
	for _, f := range frames {
		if string(f.Data) == "[DONE]" {
			continue
		}
		d, fin := deltaOf(t, f)
		if tcs, _ := d["tool_calls"].([]any); len(tcs) > 0 {
			if call != nil {
				t.Fatal("the call should arrive in one delta")
			}
			call, _ = tcs[0].(map[string]any)
		}
		if fin != nil {
			finish = fin
		}
	}
	fn, _ := call["function"].(map[string]any)
	id, _ := call["id"].(string)
	if call["index"] != float64(0) || !strings.HasPrefix(id, "call_") || fn["arguments"] != `{"text":"hi"}` || call["thought_signature"] != "sig1" {
		t.Errorf("tool call delta = %v", call)
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %v, want tool_calls", finish)
	}
	if got := cache.Get(id, "google/gemini-3.5-flash"); got != "sig1" {
		t.Errorf("cache = %q, want sig1", got)
	}
}

func TestGeminiStreamEmptyTextPartIsNotOutput(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sseChunks(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"","thoughtSignature":"s"}]},"finishReason":"STOP"}]}`)
	})
	s, _ := geminiStreamCall(t, srv.URL, OpChat, geminiChatBodyForStream(false), nil)
	frames, err := drain(t, s)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if s.State().Output {
		t.Error("an empty text part is not output")
	}
	for _, f := range frames {
		if string(f.Data) == "[DONE]" {
			continue
		}
		if d, _ := deltaOf(t, f); d["content"] != nil && d["content"] != "" {
			t.Errorf("unexpected content frame %s", f.Data)
		}
	}
}

func TestGeminiStreamPromptBlocked(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sseChunks(w, `{"promptFeedback":{"blockReason":"SAFETY"}}`)
	})
	s, _ := geminiStreamCall(t, srv.URL, OpChat, geminiChatBodyForStream(false), nil)
	if _, err := drain(t, s); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if st := s.State(); !st.Prohibited || st.Output {
		t.Errorf("state = %+v, want prohibited without output", st)
	}
}

func TestGeminiStreamSafetyAfterText(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sseChunks(w,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Once"}]}}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"SAFETY"}]}`)
	})
	s, _ := geminiStreamCall(t, srv.URL, OpChat, geminiChatBodyForStream(false), nil)
	if _, err := drain(t, s); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if st := s.State(); !st.Output || st.FinishReason != "content_filter" {
		t.Errorf("state = %+v, want output and a content_filter finish", st)
	}
}

func TestGeminiStreamURL(t *testing.T) {
	var path, query string
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		path, query = r.URL.Path, r.URL.RawQuery
		sseChunks(w, `{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"STOP"}]}`)
	})
	s, _ := geminiStreamCall(t, srv.URL, OpChat, geminiChatBodyForStream(false), nil)
	_, _ = drain(t, s)
	if !strings.HasSuffix(path, "/models/gemini-3.5-flash:streamGenerateContent") || query != "alt=sse" {
		t.Errorf("path = %s, query = %s", path, query)
	}
}

func TestCallStreamGeminiResponses(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sseChunks(w,
			`{"candidates":[{"content":{"parts":[{"text":"Hi"}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"text":" there"}]},"finishReason":"STOP"}]}`)
	})
	s, resp := geminiStreamCall(t, srv.URL, OpResponses, map[string]any{"input": "hi", "stream": true}, nil)
	if s == nil {
		t.Fatalf("want a stream, got %+v", resp)
	}
	evs, err := drainResponses(t, s)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if evs[0].m["type"] != "response.created" || evs[len(evs)-1].m["type"] != "response.completed" {
		t.Fatalf("event types = %v", eventTypes(evs))
	}
	out, _ := finalResponse(t, evs)["output"].([]any)
	msg, _ := out[0].(map[string]any)
	content, _ := msg["content"].([]any)
	if part, _ := content[0].(map[string]any); part["text"] != "Hi there" {
		t.Errorf("final output = %v", out)
	}
}

func TestCallStreamAnthropicIsBuffered(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	req := Request{Op: OpChat, Model: "claude", Body: geminiChatBodyForStream(false)}
	s, resp, err := CallStream(context.Background(), &http.Client{}, "anthropic", srv.URL, "k", time.Minute, req)
	if err != nil || s != nil || resp == nil || !resp.OK() {
		t.Fatalf("want a buffered Response, got stream=%v resp=%+v err=%v", s, resp, err)
	}
	if !reflect.DeepEqual(resp.FinishReason, "stop") {
		t.Errorf("finish = %q", resp.FinishReason)
	}
}
