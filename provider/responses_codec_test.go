package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPortableForChatAcceptsBasicInput(t *testing.T) {
	body := map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "hi"},
				map[string]any{"type": "input_image", "image_url": "https://example.com/a.png"},
			}},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "fn", "arguments": `{"a":1}`},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"},
		},
		"tools": []any{
			map[string]any{"type": "function", "name": "fn", "parameters": map[string]any{"type": "object"}},
		},
	}
	if err := PortableForChat(body); err != nil {
		t.Fatalf("portable: %v", err)
	}
}

func TestPortableForChatRejectsNativeTools(t *testing.T) {
	body := map[string]any{
		"input": "hi",
		"tools": []any{map[string]any{"type": "web_search"}},
	}
	if err := PortableForChat(body); err == nil {
		t.Fatal("expected non-portable")
	}
}

func TestPortableForChatRejectsReasoningItem(t *testing.T) {
	body := map[string]any{
		"input": []any{map[string]any{"type": "reasoning", "summary": []any{}}},
	}
	if err := PortableForChat(body); err == nil {
		t.Fatal("expected non-portable")
	}
}

func TestResponsesToChatMapsFields(t *testing.T) {
	body := map[string]any{
		"model":             "inbound",
		"max_output_tokens": float64(100),
		"temperature":       0.2,
		"text":              map[string]any{"format": map[string]any{"type": "json_object"}},
		"input": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		"tools": []any{
			map[string]any{
				"type": "function", "name": "fn", "description": "d",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
		"stream": true,
	}
	out, err := ResponsesToChat(body, "real-model")
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	if out["model"] != "real-model" {
		t.Fatalf("model=%v", out["model"])
	}
	if out["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens=%v", out["max_tokens"])
	}
	rf, _ := out["response_format"].(map[string]any)
	if rf["type"] != "json_object" {
		t.Fatalf("response_format=%v", out["response_format"])
	}
	if _, ok := out["stream"]; ok {
		t.Fatal("stream must not be present")
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%v", out["messages"])
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", out["tools"])
	}
	raw, _ := json.Marshal(out["tools"])
	if !strings.Contains(string(raw), `"name":"fn"`) {
		t.Fatalf("chat tools shape unexpected: %s", raw)
	}
}

func TestResponsesToChatStringifiesNonStringToolOutput(t *testing.T) {
	body := map[string]any{
		"input": []any{
			map[string]any{
				"type": "function_call_output", "call_id": "c1",
				"output": map[string]any{"ok": true, "n": float64(2)},
			},
		},
	}
	out, err := ResponsesToChat(body, "m")
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%v", out["messages"])
	}
	msg, _ := msgs[0].(map[string]any)
	content, ok := msg["content"].(string)
	if !ok {
		t.Fatalf("content type %T: %v", msg["content"], msg["content"])
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("content not JSON text %q: %v", content, err)
	}
	if parsed["ok"] != true || parsed["n"] != float64(2) {
		t.Fatalf("parsed=%v", parsed)
	}
	if strings.Contains(content, "map[") {
		t.Fatalf("content looks like Go %%v dump: %q", content)
	}
}

func TestResponsesToChatMapsToolCallsAndMultipart(t *testing.T) {
	body := map[string]any{
		"input": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "hi"},
				map[string]any{"type": "input_image", "image_url": "https://example.com/a.png"},
			}},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "fn", "arguments": `{"a":1}`},
		},
	}
	out, err := ResponsesToChat(body, "m")
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", out["messages"])
	}
	user, _ := msgs[0].(map[string]any)
	parts, _ := user["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("user content=%v", user["content"])
	}
	asst, _ := msgs[1].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%v", asst["tool_calls"])
	}
	tc, _ := tcs[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if tc["id"] != "c1" || fn["name"] != "fn" || fn["arguments"] != `{"a":1}` {
		t.Fatalf("tool_call=%v", tc)
	}
}
