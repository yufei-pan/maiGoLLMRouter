package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A Gemini model name goes into the URL path, so one that would leave
// models/ (a ".." segment) or change what the URL means (?, #, %) is refused
// as an unknown model, and nothing is sent with the provider's key.
func TestGeminiModelPathRefused(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()
	ctx := context.Background()
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	for _, model := range []string{"../x", "a/../b", "..", "models/../files", "x?alt=1", "x#y", "x%2e%2e"} {
		req := Request{Op: OpChat, Model: model, Body: body}
		resp, err := Call(ctx, srv.Client(), "gemini", srv.URL, "k", req)
		if err != nil || resp == nil || resp.HTTPStatus != http.StatusNotFound ||
			!strings.Contains(string(resp.OpenAIBody), `"model_not_found"`) {
			t.Errorf("Call(%q) = %s, %v; want a 404 model_not_found", model, statusAndBody(resp), err)
		}
		stream, sresp, err := CallStream(ctx, srv.Client(), "gemini", srv.URL, "k", time.Minute, req)
		if stream != nil {
			stream.Close()
			t.Errorf("CallStream(%q) opened a stream", model)
		}
		if err != nil || sresp == nil || sresp.HTTPStatus != http.StatusNotFound ||
			!strings.Contains(string(sresp.OpenAIBody), `"model_not_found"`) {
			t.Errorf("CallStream(%q) = %s, %v; want a 404 model_not_found", model, statusAndBody(sresp), err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("refused model names reached the server %d times, want 0", n)
	}

	for _, model := range []string{"gemini-3.5-flash", "models/gemini-3.5-flash", "a..b"} {
		req := Request{Op: OpChat, Model: model, Body: body}
		before := hits.Load()
		if _, err := Call(ctx, srv.Client(), "gemini", srv.URL, "k", req); err != nil {
			t.Errorf("Call(%q): %v", model, err)
		}
		if hits.Load() == before {
			t.Errorf("Call(%q) sent nothing", model)
		}
		before = hits.Load()
		stream, _, err := CallStream(ctx, srv.Client(), "gemini", srv.URL, "k", time.Minute, req)
		if stream != nil {
			stream.Close()
		}
		if err != nil {
			t.Errorf("CallStream(%q): %v", model, err)
		}
		if hits.Load() == before {
			t.Errorf("CallStream(%q) sent nothing", model)
		}
	}
}

func statusAndBody(r *Response) string {
	if r == nil {
		return "nil"
	}
	return fmt.Sprintf("status %d body %s", r.HTTPStatus, r.OpenAIBody)
}
