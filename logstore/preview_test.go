package logstore

import (
	"encoding/json"
	"testing"
)

func TestRequestContentPreview(t *testing.T) {
	long := "abcdefghijklmnopqrstuvwxyz"
	tests := []struct {
		name string
		req  string
		want string
	}{
		{"chat user message", `{"messages":[{"role":"user","content":"hello world"}]}`, "hello world"},
		{"first message wins", `{"messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"hi"}]}`, "You are helpful"},
		{"skip empty first", `{"messages":[{"role":"system","content":""},{"role":"user","content":"hi"}]}`, "hi"},
		{"assistant only", `{"messages":[{"role":"assistant","content":"hi"}]}`, "hi"},
		{"embed input string", `{"input":"embed me"}`, "embed me"},
		{"embed input array", `{"input":["a","b"]}`, "ab"},
		{"content parts", `{"messages":[{"role":"user","content":[{"type":"text","text":"part"}]}]}`, "part"},
		{"truncate", `{"messages":[{"role":"user","content":"` + long + `"}]}`, long[:16]},
		{"empty", `{}`, ""},
		{"invalid json", `{`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RequestContentPreview(json.RawMessage(tt.req), DefaultRequestPreviewLen)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequestContentPreviewUnicodeTruncate(t *testing.T) {
	got := RequestContentPreview(json.RawMessage(`{"messages":[{"role":"user","content":"你好世界你好世界"}]}`), 4)
	if got != "你好世界" {
		t.Errorf("got %q, want %q", got, "你好世界")
	}
}

func TestEstimateInTokens(t *testing.T) {
	tests := []struct {
		name string
		req  string
		min  int
		max  int
	}{
		{"empty", `{}`, 0, 0},
		{"short chat", `{"messages":[{"role":"user","content":"hello"}]}`, 1, 10},
		{"embed", `{"input":"embed me please"}`, 2, 10},
		{"fallback json size", `{`, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateInTokens(json.RawMessage(tt.req))
			if got < tt.min || got > tt.max {
				t.Errorf("got %d, want between %d and %d", got, tt.min, tt.max)
			}
		})
	}
}
