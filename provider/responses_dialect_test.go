package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func TestResponsesViaAnthropicPassesInputFile(t *testing.T) {
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{
		  "id":"msg_1","type":"message","role":"assistant","model":"claude",
		  "content":[{"type":"text","text":"ok"}],
		  "stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}
		}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "anthropic", srv.URL, "k", Request{
		Op: OpResponses, Model: "claude",
		Body: map[string]any{"input": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_file", "filename": "a.pdf", "file_data": "data:application/pdf;base64,JVBERi0="},
				map[string]any{"type": "input_file", "filename": "notes.txt", "file_data": "data:text/plain;base64,aGVsbG8="},
				map[string]any{"type": "input_file", "file_url": "https://example.com/b.pdf"},
				map[string]any{"type": "input_file", "filename": "scan.png", "file_data": "data:image/png;base64,iVBORw=="},
			},
		}}},
	})
	if err != nil || !resp.OK() {
		t.Fatalf("err=%v resp=%+v", err, resp)
	}

	msgs, _ := outbound["messages"].([]any)
	user, _ := msgs[0].(map[string]any)
	want := []any{
		map[string]any{"type": "document", "title": "a.pdf", "source": map[string]any{
			"type": "base64", "media_type": "application/pdf", "data": "JVBERi0=",
		}},
		map[string]any{"type": "document", "title": "notes.txt", "source": map[string]any{
			"type": "text", "media_type": "text/plain", "data": "hello",
		}},
		map[string]any{"type": "document", "source": map[string]any{
			"type": "url", "url": "https://example.com/b.pdf",
		}},
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "iVBORw==",
		}},
	}
	if !reflect.DeepEqual(user["content"], want) {
		raw, _ := json.Marshal(user["content"])
		t.Fatalf("content=%s", raw)
	}
}
