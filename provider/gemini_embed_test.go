package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// embedViaFakeGemini sends an OpenAI embeddings body through callGemini and
// returns the batchEmbedContents requests the router sent downstream.
func embedViaFakeGemini(t *testing.T, input any) ([]any, *Response) {
	t.Helper()
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models/gemini-embedding-2:batchEmbedContents") {
			t.Fatalf("path=%s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Fatalf("bad outbound json: %v", err)
		}
		reqs, _ := sent["requests"].([]any)
		embs := make([]string, len(reqs))
		for i := range reqs {
			embs[i] = fmt.Sprintf(`{"values":[%d,0.5]}`, i+1)
		}
		fmt.Fprintf(w, `{"embeddings":[%s]}`, strings.Join(embs, ","))
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", Request{
		Op: OpEmbed, Model: "gemini-embedding-2",
		Body: map[string]any{"model": "google/gemini-embedding-2", "input": input},
	})
	if err != nil || !resp.OK() {
		t.Fatalf("err=%v resp=%+v", err, resp)
	}
	reqs, _ := sent["requests"].([]any)
	return reqs, resp
}

func embedContentParts(t *testing.T, req any) []any {
	t.Helper()
	m, _ := req.(map[string]any)
	if m["model"] != "models/gemini-embedding-2" {
		t.Fatalf("request model=%v", m["model"])
	}
	content, _ := m["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	return parts
}

func TestGeminiEmbedTextInputUnchanged(t *testing.T) {
	reqs, _ := embedViaFakeGemini(t, []any{"a", "b"})
	if len(reqs) != 2 {
		t.Fatalf("requests=%v", reqs)
	}
	for i, want := range []string{"a", "b"} {
		got := embedContentParts(t, reqs[i])
		if !reflect.DeepEqual(got, []any{map[string]any{"text": want}}) {
			t.Fatalf("request %d parts=%v", i, got)
		}
	}
}

// MaiBot's image_embedding_input template {"image": "{data_uri}"} arrives as a
// single object (not an array) and must become one inlineData request.
func TestGeminiEmbedImageObjectInput(t *testing.T) {
	reqs, resp := embedViaFakeGemini(t, map[string]any{"image": "data:image/png;base64,iVBORw0K"})
	if len(reqs) != 1 {
		t.Fatalf("requests=%v", reqs)
	}
	want := []any{map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "iVBORw0K"}}}
	if got := embedContentParts(t, reqs[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("parts=%v", got)
	}
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.OpenAIBody, &out); err != nil || len(out.Data) != 1 || len(out.Data[0].Embedding) != 2 {
		t.Fatalf("openai body=%s err=%v", resp.OpenAIBody, err)
	}
	if !resp.HasContent {
		t.Fatal("HasContent=false for image embedding")
	}
}

func TestGeminiEmbedMixedArrayInput(t *testing.T) {
	reqs, _ := embedViaFakeGemini(t, []any{
		"caption",
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64,/9j/4AAQ"}},
		map[string]any{"text": "a cat", "image": "data:image/webp;base64,UklGR"},
	})
	if len(reqs) != 3 {
		t.Fatalf("requests=%v", reqs)
	}
	want := [][]any{
		{map[string]any{"text": "caption"}},
		{map[string]any{"inlineData": map[string]any{"mimeType": "image/jpeg", "data": "/9j/4AAQ"}}},
		{
			map[string]any{"text": "a cat"},
			map[string]any{"inlineData": map[string]any{"mimeType": "image/webp", "data": "UklGR"}},
		},
	}
	for i := range want {
		if got := embedContentParts(t, reqs[i]); !reflect.DeepEqual(got, want[i]) {
			t.Fatalf("request %d parts=%v", i, got)
		}
	}
}
