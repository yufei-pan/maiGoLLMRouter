package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCallOpenAIStripsStreamingControls(t *testing.T) {
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&outbound); err != nil {
			t.Fatalf("decode outbound body: %v", err)
		}
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer srv.Close()

	req := Request{
		Op:    OpChat,
		Model: "real-model",
		Body: map[string]any{
			"model":          "inbound-model",
			"messages":       []any{map[string]any{"role": "user", "content": "hello"}},
			"stream":         true,
			"stream_options": map[string]any{"include_usage": true},
			"temperature":    0.5,
		},
	}

	resp, err := callOpenAI(context.Background(), srv.Client(), srv.URL, "test-key", req)
	if err != nil {
		t.Fatalf("callOpenAI error: %v", err)
	}
	if !resp.OK() || resp.FinishReason != "stop" || !resp.HasContent {
		t.Fatalf("unexpected response outcome: status=%d finish=%q hasContent=%v body=%s",
			resp.HTTPStatus, resp.FinishReason, resp.HasContent, string(resp.OpenAIBody))
	}
	if _, ok := outbound["stream"]; ok {
		t.Fatalf("outbound body includes unsupported stream field: %#v", outbound)
	}
	if _, ok := outbound["stream_options"]; ok {
		t.Fatalf("outbound body includes unsupported stream_options field: %#v", outbound)
	}
	if outbound["model"] != "real-model" {
		t.Fatalf("outbound model = %v, want real-model", outbound["model"])
	}
	if outbound["temperature"] != 0.5 {
		t.Fatalf("temperature passthrough = %v, want 0.5", outbound["temperature"])
	}
}
