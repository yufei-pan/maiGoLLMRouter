package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maiGoLLMRouter/provider"
)

// TestGeminiSignatureRoundTripThroughRouter verifies a client that drops the
// thought signature from a Gemini tool call still gets it sent back on the
// next turn: the router remembers it by tool-call id.
func TestGeminiSignatureRoundTripThroughRouter(t *testing.T) {
	var secondBody string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if calls == 1 {
			fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"s1"}]},"finishReason":"STOP"}]}`)
			return
		}
		secondBody = string(b)
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	r := New(loadCfg(t, fmt.Sprintf(`
[[provider]]
name = "google"
kind = "gemini"
base_url = %q
keys = ["k"]

[model."m"]
targets = ["google/gemini-3.5-flash"]
`, srv.URL)))

	first := r.Execute(context.Background(), provider.OpChat, "m", chatReq())
	if !first.Success {
		t.Fatalf("first turn failed: %+v", first.Attempts)
	}
	var reply struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(first.Body, &reply); err != nil || len(reply.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("first reply = %s", first.Body)
	}
	id := reply.Choices[0].Message.ToolCalls[0].ID

	// The client sends the call back without its signature, as annai does.
	second := map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
			"id": id, "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"},
		}}},
		map[string]any{"role": "tool", "tool_call_id": id, "content": "ok"},
	}}
	if res := r.Execute(context.Background(), provider.OpChat, "m", second); !res.Success {
		t.Fatalf("second turn failed: %+v", res.Attempts)
	}
	if !strings.Contains(secondBody, `"thoughtSignature":"s1"`) {
		t.Errorf("second request did not carry the signature: %s", secondBody)
	}
}
