package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResponsesViaAnthropicPortable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		fmt.Fprint(w, `{
		  "id":"msg_1","type":"message","role":"assistant","model":"claude",
		  "content":[{"type":"text","text":"hi"}],
		  "stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}
		}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "anthropic", srv.URL, "k", Request{
		Op: OpResponses, Model: "claude-3-5-sonnet",
		Body: map[string]any{"model": "in", "input": "hello"},
	})
	if err != nil || !resp.OK() || !resp.HasContent {
		t.Fatalf("err=%v resp=%+v body=%s", err, resp, resp.OpenAIBody)
	}
	var parsed map[string]any
	if err := json.Unmarshal(resp.OpenAIBody, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["object"] != "response" || parsed["status"] != "completed" {
		t.Fatalf("body=%s", resp.OpenAIBody)
	}
}

func TestResponsesViaAnthropicNonPortable(t *testing.T) {
	resp, err := Call(context.Background(), http.DefaultClient, "anthropic", "http://127.0.0.1:0", "k", Request{
		Op: OpResponses, Model: "claude",
		Body: map[string]any{
			"input": "hi",
			"tools": []any{map[string]any{"type": "web_search"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Incompatible {
		t.Fatalf("want incompatible, got %+v", resp)
	}
}
