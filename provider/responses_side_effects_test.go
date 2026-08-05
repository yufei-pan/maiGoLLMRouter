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

func TestIsProhibitedContentResponsesShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"incomplete content_filter", `{"status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}`, true},
		{"incomplete prohibited", `{"status":"incomplete","incomplete_details":{"reason":"PROHIBITED_CONTENT"},"output":[]}`, true},
		{"normal incomplete max tokens", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"x"}]}]}`, false},
		{"completed ok", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isProhibitedContent([]byte(c.body)); got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestAnthropicConsumesToolsAndImages(t *testing.T) {
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	req := Request{
		Op:    OpChat,
		Model: "claude-3-5-sonnet",
		Body: map[string]any{
			"messages": []any{map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "see"},
					map[string]any{"type": "image_url", "image_url": map[string]any{
						"url": "data:image/png;base64,AAAA",
					}},
				},
			}},
			"tools": []any{map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "f", "description": "d",
					"parameters": map[string]any{"type": "object"},
				},
			}},
			"tool_choice":      "auto",
			"response_format": map[string]any{"type": "json_object"},
		},
	}
	if _, err := Call(context.Background(), srv.Client(), "anthropic", srv.URL, "k", req); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if _, ok := outbound["response_format"]; ok {
		t.Fatalf("response_format leaked: %v", outbound["response_format"])
	}
	tools, _ := outbound["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", outbound["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "f" || tool["input_schema"] == nil {
		t.Fatalf("anthropic tool shape=%v", tool)
	}
	if _, ok := tool["type"]; ok {
		t.Fatalf("openai nested tool type leaked: %v", tool)
	}
	tc, _ := outbound["tool_choice"].(map[string]any)
	if tc["type"] != "auto" {
		t.Fatalf("tool_choice=%v", outbound["tool_choice"])
	}
	msgs := outbound["messages"].([]any)
	user := msgs[0].(map[string]any)
	content, ok := user["content"].([]any)
	if !ok {
		t.Fatalf("expected multipart content, got %T %v", user["content"], user["content"])
	}
	raw, _ := json.Marshal(content)
	if !strings.Contains(string(raw), `"type":"image"`) || !strings.Contains(string(raw), "AAAA") {
		t.Fatalf("image missing from anthropic content: %s", raw)
	}
}

func TestGeminiConsumesResponseFormatAndImages(t *testing.T) {
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)
	}))
	defer srv.Close()

	req := Request{
		Op:    OpChat,
		Model: "gemini-2.0-flash",
		Body: map[string]any{
			"messages": []any{map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "see"},
					map[string]any{"type": "image_url", "image_url": map[string]any{
						"url": "data:image/png;base64,BBBB",
					}},
				},
			}},
			"response_format": map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "out",
					"schema": map[string]any{"type": "object", "properties": map[string]any{}},
				},
			},
		},
	}
	if _, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", req); err != nil {
		t.Fatalf("Call: %v", err)
	}
	gen, _ := outbound["generationConfig"].(map[string]any)
	if gen["responseMimeType"] != "application/json" {
		t.Fatalf("generationConfig=%v", gen)
	}
	if _, ok := gen["response_format"]; ok {
		t.Fatalf("response_format leaked into generationConfig: %v", gen)
	}
	if gen["responseSchema"] == nil {
		t.Fatalf("expected responseSchema, got %v", gen)
	}
	contents := outbound["contents"].([]any)
	user := contents[0].(map[string]any)
	parts := user["parts"].([]any)
	raw, _ := json.Marshal(parts)
	if !strings.Contains(string(raw), "inlineData") || !strings.Contains(string(raw), "BBBB") {
		t.Fatalf("image missing from gemini parts: %s", raw)
	}
}

func TestOpenAIResponsesProbeIgnoresModelNotFound404(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(404)
		fmt.Fprint(w, `{"error":{"message":"The model does not exist","code":"model_not_found"}}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", Request{
		Op: OpResponses, Model: "missing-model", ResponsesMode: ResponsesModeProbe,
		Body: map[string]any{"input": "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.LearnChatOnly {
		t.Fatal("model_not_found must not learn Chat-only")
	}
	if len(paths) != 1 || paths[0] != "/responses" {
		t.Fatalf("paths=%v, want single /responses (no chat fallback)", paths)
	}
}
