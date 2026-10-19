package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// historyWithCalls is a chat history whose assistant turn made the given tool
// calls (ids call_1, call_2, ...) without signatures, plus a tool result each.
func historyWithCalls(calls ...map[string]any) map[string]any {
	tcs := make([]any, 0, len(calls))
	msgs := []any{map[string]any{"role": "user", "content": "hi"}}
	for i, extra := range calls {
		tc := map[string]any{
			"id":       fmt.Sprintf("call_%d", i+1),
			"type":     "function",
			"function": map[string]any{"name": "f", "arguments": "{}"},
		}
		for k, v := range extra {
			tc[k] = v
		}
		tcs = append(tcs, tc)
	}
	msgs = append(msgs, map[string]any{"role": "assistant", "content": nil, "tool_calls": tcs})
	for i := range calls {
		msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i+1), "content": "ok"})
	}
	return map[string]any{"messages": msgs}
}

// modelParts returns the parts of the first model content in a Gemini body.
func modelParts(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	contents, _ := body["contents"].([]map[string]any)
	for _, c := range contents {
		if c["role"] == "model" {
			parts, _ := c["parts"].([]map[string]any)
			return parts
		}
	}
	t.Fatalf("no model content in %v", body)
	return nil
}

func geminiReq(model string, cache *SignatureCache, body map[string]any) Request {
	return Request{Op: OpChat, Model: model, Provider: "google", Signatures: cache, Body: body}
}

func TestGeminiRestoresSignatureFromCache(t *testing.T) {
	cache := NewSignatureCache(10, time.Hour)
	cache.Put("call_1", "google/gemini-3.5-flash", "sig")
	parts := modelParts(t, buildGeminiChatBody(geminiReq("gemini-3.5-flash", cache, historyWithCalls(nil))))
	if parts[0]["thoughtSignature"] != "sig" {
		t.Errorf("first part = %v, want thoughtSignature sig", parts[0])
	}
}

func TestGeminiPlaceholderOnMiss(t *testing.T) {
	cache := NewSignatureCache(10, time.Hour)
	parts := modelParts(t, buildGeminiChatBody(geminiReq("gemini-3.5-flash", cache, historyWithCalls(nil, nil))))
	if parts[0]["thoughtSignature"] != geminiPlaceholderSignature {
		t.Errorf("first call = %v, want the placeholder", parts[0])
	}
	if _, has := parts[1]["thoughtSignature"]; has {
		t.Errorf("second call = %v, want no signature", parts[1])
	}

	gemma := modelParts(t, buildGeminiChatBody(geminiReq("gemma-4-31b-it", cache, historyWithCalls(nil))))
	if _, has := gemma[0]["thoughtSignature"]; has {
		t.Errorf("gemma call = %v, want no signature", gemma[0])
	}

	prefixed := modelParts(t, buildGeminiChatBody(geminiReq("models/gemini-3.5-flash", cache, historyWithCalls(nil))))
	if prefixed[0]["thoughtSignature"] != geminiPlaceholderSignature {
		t.Errorf("models/gemini call = %v, want the placeholder", prefixed[0])
	}
}

func TestGeminiOtherModelGetsPlaceholder(t *testing.T) {
	cache := NewSignatureCache(10, time.Hour)
	cache.Put("call_1", "google/gemini-3.6-flash", "sig")
	parts := modelParts(t, buildGeminiChatBody(geminiReq("gemini-3.5-flash", cache, historyWithCalls(nil))))
	if parts[0]["thoughtSignature"] != geminiPlaceholderSignature {
		t.Errorf("first part = %v, want the placeholder, not another model's signature", parts[0])
	}
}

func TestGeminiClientSignatureWins(t *testing.T) {
	cache := NewSignatureCache(10, time.Hour)
	cache.Put("call_1", "google/gemini-3.5-flash", "cached")
	parts := modelParts(t, buildGeminiChatBody(geminiReq("gemini-3.5-flash", cache, historyWithCalls(map[string]any{"thought_signature": "own"}))))
	if parts[0]["thoughtSignature"] != "own" {
		t.Errorf("first part = %v, want the client's own signature", parts[0])
	}
}

func TestGeminiChatRemembersSignatures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"s1"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	cache := NewSignatureCache(10, time.Hour)
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	resp, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", geminiReq("gemini-3.5-flash", cache, body))
	if err != nil || !resp.OK() {
		t.Fatalf("Call: %v, %+v", err, resp)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.OpenAIBody, &parsed); err != nil || len(parsed.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("reply = %s", resp.OpenAIBody)
	}
	id := parsed.Choices[0].Message.ToolCalls[0].ID
	if got := cache.Get(id, "google/gemini-3.5-flash"); got != "s1" {
		t.Errorf("cache for %s = %q, want s1", id, got)
	}
}
