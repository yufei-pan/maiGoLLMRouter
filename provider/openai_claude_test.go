package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCallDoesNotCoerceTrailingAssistant(t *testing.T) {
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	req := Request{
		Op:    OpChat,
		Model: "claude-3-5-sonnet",
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
		t.Fatalf("Call still coerced trailing role to %v", last["role"])
	}
}
