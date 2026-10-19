package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func audioMessage(part map[string]any) map[string]any {
	return map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "hi"},
		map[string]any{"type": "input_audio", "input_audio": part},
	}}}}
}

func userParts(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	contents, _ := body["contents"].([]map[string]any)
	if len(contents) == 0 {
		t.Fatalf("no contents in %v", body)
	}
	parts, _ := contents[0]["parts"].([]map[string]any)
	return parts
}

func TestGeminiChatInputAudio(t *testing.T) {
	body := buildGeminiChatBody(Request{Op: OpChat, Model: "gemini-3.5-flash", Body: audioMessage(map[string]any{"data": "QUJD", "format": "wav"})})
	want := []map[string]any{
		{"text": "hi"},
		{"inlineData": map[string]any{"mimeType": "audio/wav", "data": "QUJD"}},
	}
	if got := userParts(t, body); !reflect.DeepEqual(got, want) {
		t.Errorf("parts = %v\nwant %v", got, want)
	}
}

func TestGeminiChatInputAudioDataURI(t *testing.T) {
	body := buildGeminiChatBody(Request{Op: OpChat, Model: "gemini-3.5-flash", Body: audioMessage(map[string]any{"data": "data:audio/ogg;base64,QUJD"})})
	parts := userParts(t, body)
	want := map[string]any{"inlineData": map[string]any{"mimeType": "audio/ogg", "data": "QUJD"}}
	if len(parts) != 2 || !reflect.DeepEqual(parts[1], want) {
		t.Errorf("parts = %v, want the audio as %v", parts, want)
	}
}

func TestGeminiStreamCarriesInputAudio(t *testing.T) {
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		sseChunks(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()
	body := audioMessage(map[string]any{"data": "QUJD", "format": "wav"})
	body["stream"] = true
	s, _, err := CallStream(context.Background(), &http.Client{}, "gemini", srv.URL, "k", time.Minute, Request{Op: OpChat, Model: "gemini-3.5-flash", Body: body})
	if err != nil || s == nil {
		t.Fatalf("CallStream: %v", err)
	}
	_, _ = drain(t, s)
	if !strings.Contains(sent, `"inlineData"`) || !strings.Contains(sent, `"audio/wav"`) {
		t.Errorf("streamed request has no audio: %s", sent)
	}
}
