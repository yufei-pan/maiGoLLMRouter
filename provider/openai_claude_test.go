package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsClaudeModelName(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"claude-3-5-sonnet", true},
		{"anthropic.claude-3-5-sonnet", true},
		{"openrouter/anthropic/claude-opus-4", true},
		{"a/b/anthropic.claude-sonnet-4", true},
		{"gpt-4o", false},
		{"anthropic/claude", false}, // leaf is "claude", not "claude-"
		{"claude", false},
		{"proxy/gpt-4o", false},
	}
	for _, c := range cases {
		if got := isClaudeModelName(c.model); got != c.want {
			t.Errorf("isClaudeModelName(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestCallCoercesTrailingAssistantForClaudeAllDialects(t *testing.T) {
	cases := []struct {
		kind     string
		wantPath string
	}{
		{"openai", "/chat/completions"},
		{"anthropic", "/messages"},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			var outbound map[string]any
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				defer r.Body.Close()
				if err := json.NewDecoder(r.Body).Decode(&outbound); err != nil {
					t.Fatalf("decode outbound body: %v", err)
				}
				switch c.kind {
				case "anthropic":
					fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				default:
					fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
				}
			}))
			defer srv.Close()

			inboundMsgs := []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"role": "assistant", "content": "prefill"},
			}
			req := Request{
				Op:    OpChat,
				Model: "proxy/anthropic/claude-3-5-sonnet",
				Body: map[string]any{
					"model":    "inbound",
					"messages": inboundMsgs,
				},
			}

			if _, err := Call(context.Background(), srv.Client(), c.kind, srv.URL, "key", req); err != nil {
				t.Fatalf("Call error: %v", err)
			}
			if gotPath != c.wantPath {
				t.Fatalf("path = %q, want %q", gotPath, c.wantPath)
			}

			msgs, ok := outbound["messages"].([]any)
			if !ok || len(msgs) != 2 {
				t.Fatalf("outbound messages = %#v", outbound["messages"])
			}
			last, ok := msgs[1].(map[string]any)
			if !ok || last["role"] != "user" || last["content"] != "prefill" {
				t.Fatalf("trailing message = %#v, want user/prefill", msgs[1])
			}
			origLast := inboundMsgs[1].(map[string]any)
			if origLast["role"] != "assistant" {
				t.Fatalf("inbound last role mutated to %v", origLast["role"])
			}
		})
	}
}

func TestCallLeavesTrailingAssistantForNonClaude(t *testing.T) {
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	req := Request{
		Op:    OpChat,
		Model: "gpt-4o",
		Body: map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"role": "assistant", "content": "prefill"},
			},
		},
	}
	if _, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "key", req); err != nil {
		t.Fatalf("Call error: %v", err)
	}
	last := outbound["messages"].([]any)[1].(map[string]any)
	if last["role"] != "assistant" {
		t.Fatalf("trailing role = %v, want assistant", last["role"])
	}
}

func TestCallCoercesTrailingAssistantForClaudeResponsesPassThrough(t *testing.T) {
	var outbound map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&outbound); err != nil {
			t.Fatalf("decode outbound body: %v", err)
		}
		fmt.Fprint(w, `{"id":"r1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`)
	}))
	defer srv.Close()

	inboundInput := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "prefill"},
	}
	req := Request{
		Op:            OpResponses,
		Model:         "claude-opus-5(max)",
		ResponsesMode: ResponsesModeForce,
		Body: map[string]any{
			"model": "inbound",
			"input": inboundInput,
		},
	}
	if _, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "key", req); err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want /responses", gotPath)
	}
	inp, ok := outbound["input"].([]any)
	if !ok || len(inp) != 2 {
		t.Fatalf("outbound input = %#v", outbound["input"])
	}
	last, ok := inp[1].(map[string]any)
	if !ok || last["role"] != "user" || last["content"] != "prefill" {
		t.Fatalf("trailing input = %#v, want user/prefill", inp[1])
	}
	origLast := inboundInput[1].(map[string]any)
	if origLast["role"] != "assistant" {
		t.Fatalf("inbound last role mutated to %v", origLast["role"])
	}
}
