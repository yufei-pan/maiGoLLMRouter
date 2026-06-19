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
		{"last user wins", `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}]}`, "second"},
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
			got := requestContentPreview(json.RawMessage(tt.req), DefaultRequestPreviewLen)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRequestContentPreviewUnicodeTruncate(t *testing.T) {
	got := requestContentPreview(json.RawMessage(`{"messages":[{"role":"user","content":"你好世界你好世界"}]}`), 4)
	if got != "你好世界" {
		t.Errorf("got %q, want %q", got, "你好世界")
	}
}
