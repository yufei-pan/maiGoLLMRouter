package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIResponsesPassThrough(t *testing.T) {
	var path string
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"id":"r1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", Request{
		Op: OpResponses, Model: "m", ResponsesMode: ResponsesModeForce,
		Body: map[string]any{"model": "in", "input": "hi", "stream": true},
	})
	if err != nil || !resp.OK() || !resp.HasContent {
		t.Fatalf("err=%v resp=%+v", err, resp)
	}
	if path != "/responses" {
		t.Fatalf("path=%s", path)
	}
	if _, ok := outbound["stream"]; ok {
		t.Fatal("stream should be stripped")
	}
	if outbound["model"] != "m" {
		t.Fatalf("model=%v, want the resolved downstream model", outbound["model"])
	}
	if resp.LearnChatOnly || resp.Incompatible {
		t.Fatalf("unexpected flags: %+v", resp)
	}
}

func TestOpenAIResponsesProbeFallsBackToChat(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/responses" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"message":"not found"}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", Request{
		Op: OpResponses, Model: "m", ResponsesMode: ResponsesModeProbe,
		Body: map[string]any{"model": "in", "input": "hi"},
	})
	if err != nil || !resp.OK() || !resp.HasContent || !resp.LearnChatOnly {
		t.Fatalf("err=%v resp=%+v body=%s", err, resp, resp.OpenAIBody)
	}
	if len(paths) != 2 || paths[0] != "/responses" || paths[1] != "/chat/completions" {
		t.Fatalf("paths=%v", paths)
	}
	var parsed map[string]any
	_ = json.Unmarshal(resp.OpenAIBody, &parsed)
	if parsed["object"] != "response" {
		t.Fatalf("client body not Responses-shaped: %s", resp.OpenAIBody)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish=%q", resp.FinishReason)
	}
	// Logging fields must describe the Chat call that actually produced the answer.
	if !strings.HasSuffix(resp.OutboundURL, "/chat/completions") {
		t.Fatalf("outbound url=%s", resp.OutboundURL)
	}
	var outbound map[string]any
	if err := json.Unmarshal(resp.OutboundBody, &outbound); err != nil {
		t.Fatal(err)
	}
	if _, ok := outbound["messages"]; !ok {
		t.Fatalf("outbound body is not a chat request: %s", resp.OutboundBody)
	}
	if !strings.Contains(string(resp.RawResponse), "choices") {
		t.Fatalf("raw response should be the chat body: %s", resp.RawResponse)
	}
}

func TestOpenAIResponsesProbeKeepsRealErrors(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"invalid input"}}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", Request{
		Op: OpResponses, Model: "m", Body: map[string]any{"input": "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK() || resp.LearnChatOnly || len(paths) != 1 {
		t.Fatalf("validation error must not trigger fallback: resp=%+v paths=%v", resp, paths)
	}
	if resp.HTTPStatus != 400 {
		t.Fatalf("status=%d", resp.HTTPStatus)
	}
}

func TestOpenAIResponsesForceNoChatFallback(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(404)
		fmt.Fprint(w, `{"error":"nope"}`)
	}))
	defer srv.Close()
	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", Request{
		Op: OpResponses, Model: "m", ResponsesMode: ResponsesModeForce,
		Body: map[string]any{"input": "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK() || resp.LearnChatOnly {
		t.Fatalf("force mode must not fall back: %+v", resp)
	}
	if len(paths) != 1 || paths[0] != "/responses" {
		t.Fatalf("paths=%v", paths)
	}
}

func TestOpenAIResponsesChatOnlySkipsResponsesRoute(t *testing.T) {
	var paths []string
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", Request{
		Op: OpResponses, Model: "claude-3-5-sonnet", ResponsesMode: ResponsesModeChatOnly,
		Body: map[string]any{"input": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "prefill"},
		}},
	})
	if err != nil || !resp.OK() || !resp.HasContent {
		t.Fatalf("err=%v resp=%+v", err, resp)
	}
	if len(paths) != 1 || paths[0] != "/chat/completions" {
		t.Fatalf("paths=%v", paths)
	}
	if resp.LearnChatOnly {
		t.Fatal("chat-only mode learns nothing new")
	}
	var parsed map[string]any
	_ = json.Unmarshal(resp.OpenAIBody, &parsed)
	if parsed["object"] != "response" {
		t.Fatalf("client body not Responses-shaped: %s", resp.OpenAIBody)
	}
	msgs, _ := outbound["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("outbound messages=%v", outbound["messages"])
	}
	last, _ := msgs[1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("claude coercion not applied to the chat request: %v", last)
	}
}

func TestOpenAIResponsesChatOnlyNonPortable(t *testing.T) {
	resp, err := Call(context.Background(), http.DefaultClient, "openai", "http://127.0.0.1:0", "k", Request{
		Op: OpResponses, Model: "m", ResponsesMode: ResponsesModeChatOnly,
		Body: map[string]any{"input": "hi", "tools": []any{map[string]any{"type": "web_search"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Incompatible {
		t.Fatalf("want incompatible, got %+v", resp)
	}
	if resp.HTTPStatus != 0 || resp.OK() {
		t.Fatalf("no request should have been sent: %+v", resp)
	}
}

func TestOpenAIResponsesProbeFallbackNonPortable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected chat call for a non-portable body")
		}
		w.WriteHeader(404)
		fmt.Fprint(w, `{"error":{"message":"not found"}}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", Request{
		Op: OpResponses, Model: "m", ResponsesMode: ResponsesModeProbe,
		Body: map[string]any{"input": "hi", "tools": []any{map[string]any{"type": "web_search"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Incompatible || !resp.LearnChatOnly {
		t.Fatalf("want incompatible + learned chat-only, got %+v", resp)
	}
	if resp.HTTPStatus != 0 {
		t.Fatalf("incompatible must keep HTTPStatus 0, got %d", resp.HTTPStatus)
	}
	if !strings.HasSuffix(resp.OutboundURL, "/responses") {
		t.Fatalf("want failed /responses outbound url, got %s", resp.OutboundURL)
	}
	if len(resp.OutboundBody) == 0 || len(resp.RawResponse) == 0 {
		t.Fatalf("want preserved /responses outbound/raw, got outbound=%q raw=%q", resp.OutboundBody, resp.RawResponse)
	}
}

func TestOpenAIResponsesChatToResponsesFailureClearsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"message":"not found"}}`)
			return
		}
		fmt.Fprint(w, `not-json-at-all`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", Request{
		Op: OpResponses, Model: "m", ResponsesMode: ResponsesModeProbe,
		Body: map[string]any{"input": "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OpenAIBody != nil || resp.HasContent {
		t.Fatalf("ChatToResponses failure must clear OpenAIBody and HasContent: body=%q has=%v", resp.OpenAIBody, resp.HasContent)
	}
	if !resp.OK() || !resp.LearnChatOnly {
		t.Fatalf("want OK chat status + LearnChatOnly, got %+v", resp)
	}
}

func TestOpenAIResponsesProbeChatNon2xxKeepsLearn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"message":"not found"}}`)
			return
		}
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":{"message":"unavailable"}}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", Request{
		Op: OpResponses, Model: "m", ResponsesMode: ResponsesModeProbe,
		Body: map[string]any{"input": "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK() || !resp.LearnChatOnly {
		t.Fatalf("want non-OK chat with LearnChatOnly, got %+v", resp)
	}
	if resp.HTTPStatus != 503 {
		t.Fatalf("status=%d", resp.HTTPStatus)
	}
}
