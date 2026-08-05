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

func TestResponsesToChatNestsJSONSchemaFormat(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	body := map[string]any{
		"input": "hi",
		"text": map[string]any{"format": map[string]any{
			"type": "json_schema", "name": "Result", "strict": true,
			"description": "d", "schema": schema,
		}},
	}
	out, err := ResponsesToChat(body, "m")
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	rf, _ := out["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Fatalf("response_format=%v", out["response_format"])
	}
	inner, ok := rf["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("json_schema must be nested for chat: %v", rf)
	}
	if inner["name"] != "Result" || inner["strict"] != true || inner["description"] != "d" {
		t.Fatalf("nested json_schema=%v", inner)
	}
	if _, ok := inner["schema"].(map[string]any); !ok {
		t.Fatalf("schema not carried over: %v", inner)
	}
	// The flat Responses fields must not linger beside the nested object.
	if _, ok := rf["name"]; ok {
		t.Fatalf("flat responses fields leaked into chat response_format: %v", rf)
	}
}

func TestResponsesToChatPassesThroughChatShapedFormats(t *testing.T) {
	for _, format := range []map[string]any{
		{"type": "json_object"},
		{"type": "text"},
		{"type": "json_schema", "json_schema": map[string]any{"name": "Already"}},
	} {
		out, err := ResponsesToChat(map[string]any{
			"input": "hi",
			"text":  map[string]any{"format": format},
		}, "m")
		if err != nil {
			t.Fatalf("ResponsesToChat(%v): %v", format, err)
		}
		rf, _ := out["response_format"].(map[string]any)
		if rf["type"] != format["type"] {
			t.Fatalf("format %v became %v", format, rf)
		}
		if format["type"] == "json_schema" {
			inner, _ := rf["json_schema"].(map[string]any)
			if inner["name"] != "Already" {
				t.Fatalf("chat-shaped json_schema was rewrapped: %v", rf)
			}
		}
	}
}

func TestPortableForChatRejectsPreviousResponseID(t *testing.T) {
	body := map[string]any{"input": "hi", "previous_response_id": "resp_123"}
	if err := PortableForChat(body); err == nil {
		t.Fatal("expected previous_response_id to be non-portable")
	}
	if _, err := ResponsesToChat(body, "m"); err == nil {
		t.Fatal("expected ResponsesToChat to reject previous_response_id")
	}
	// An absent/empty id stays portable.
	if err := PortableForChat(map[string]any{"input": "hi", "previous_response_id": nil}); err != nil {
		t.Fatalf("nil previous_response_id should be portable: %v", err)
	}
}

func TestResponsesToChatDropsResponsesOnlyExtras(t *testing.T) {
	body := map[string]any{
		"input":        []any{map[string]any{"role": "user", "content": "hi"}},
		"instructions": "be terse",
		"reasoning":    map[string]any{"effort": "high"},
		"include":      []any{"reasoning.encrypted_content"},
		"truncation":   "auto",
		"store":        false,
		"temperature":  0.3,
	}
	out, err := ResponsesToChat(body, "m")
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	for _, k := range []string{"instructions", "reasoning", "include", "truncation", "store", "input"} {
		if _, ok := out[k]; ok {
			t.Fatalf("%q must not reach the chat body: %v", k, out)
		}
	}
	if out["temperature"] != 0.3 {
		t.Fatalf("shared params should pass through: %v", out)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", out["messages"])
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be terse" {
		t.Fatalf("instructions should lead as a system message: %v", msgs[0])
	}
}

func TestResponsesToChatConvertsToolChoice(t *testing.T) {
	tools := []any{map[string]any{"type": "function", "name": "fn"}}

	out, err := ResponsesToChat(map[string]any{
		"input": "hi", "tools": tools,
		"tool_choice": map[string]any{"type": "function", "name": "fn"},
	}, "m")
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	tc, _ := out["tool_choice"].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if tc["type"] != "function" || fn["name"] != "fn" {
		t.Fatalf("tool_choice=%v", out["tool_choice"])
	}

	// Already Chat-shaped and plain string choices pass through untouched.
	out, err = ResponsesToChat(map[string]any{
		"input": "hi", "tools": tools,
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "fn"}},
	}, "m")
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	tc, _ = out["tool_choice"].(map[string]any)
	if fn, _ := tc["function"].(map[string]any); fn["name"] != "fn" {
		t.Fatalf("chat-shaped tool_choice=%v", out["tool_choice"])
	}
	out, err = ResponsesToChat(map[string]any{"input": "hi", "tool_choice": "required"}, "m")
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	if out["tool_choice"] != "required" {
		t.Fatalf("tool_choice=%v", out["tool_choice"])
	}

	if err := PortableForChat(map[string]any{
		"input": "hi", "tool_choice": map[string]any{"type": "web_search_preview"},
	}); err == nil {
		t.Fatal("hosted tool_choice should be non-portable")
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

func TestChatToResponsesTextAndTools(t *testing.T) {
	chat := []byte(`{
	  "id":"chatcmpl-1",
	  "model":"m",
	  "choices":[{
	    "finish_reason":"tool_calls",
	    "message":{
	      "role":"assistant",
	      "content":"hello",
	      "tool_calls":[{
	        "id":"c1","type":"function",
	        "function":{"name":"fn","arguments":"{}"}
	      }]
	    }
	  }],
	  "usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}
	}`)
	out, err := ChatToResponses(chat, "real-model", "tool_calls")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["object"] != "response" || parsed["status"] != "completed" {
		t.Fatalf("envelope=%v", parsed)
	}
	if parsed["model"] != "real-model" {
		t.Fatalf("model=%v", parsed["model"])
	}
	output, _ := parsed["output"].([]any)
	if len(output) < 2 {
		t.Fatalf("output=%v", parsed["output"])
	}
	usage, _ := parsed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(4) {
		t.Fatalf("usage=%v", usage)
	}
}

func TestChatToResponsesLengthBecomesIncomplete(t *testing.T) {
	chat := []byte(`{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"trunc"}}]}`)
	out, err := ChatToResponses(chat, "m", "length")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["status"] != "incomplete" {
		t.Fatalf("status=%v, want incomplete", parsed["status"])
	}
	details, _ := parsed["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details=%v", parsed["incomplete_details"])
	}
	// The truncated text is still delivered to the client.
	output, _ := parsed["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v", parsed["output"])
	}
}
