package logstore

import (
	"encoding/json"
	"testing"
)

const longPreviewText = "abcdefghijklmnopqrstuvwxyz"

var previewTests = []struct {
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
	{"responses role content", `{"input":[{"role":"user","content":"hello responses"}]}`, "hello responses"},
	{"responses input_text parts", `{"input":[{"role":"system","content":""},{"role":"user","content":[{"type":"input_text","text":"part hi"}]}]}`, "part hi"},
	{"responses skip empty then user", `{"input":[{"role":"assistant","content":""},{"role":"user","content":"hi"}]}`, "hi"},
	{"content parts", `{"messages":[{"role":"user","content":[{"type":"text","text":"part"}]}]}`, "part"},
	{"truncate", `{"messages":[{"role":"user","content":"` + longPreviewText + `"}]}`, longPreviewText[:16]},
	{"empty", `{}`, ""},
	{"invalid json", `{`, ""},
}

func TestRequestContentPreview(t *testing.T) {
	for _, tt := range previewTests {
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

var estimateTests = []struct {
	name string
	req  string
	min  int
	max  int
}{
	{"empty", `{}`, 0, 0},
	{"short chat", `{"messages":[{"role":"user","content":"hello"}]}`, 1, 10},
	{"embed", `{"input":"embed me please"}`, 2, 10},
	{"responses input items", `{"input":[{"role":"user","content":[{"type":"input_text","text":"hello world from responses"}]}]}`, 4, 20},
	{"fallback json size", `{`, 1, 1},
}

func TestEstimateInTokens(t *testing.T) {
	for _, tt := range estimateTests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateInTokens(json.RawMessage(tt.req))
			if got < tt.min || got > tt.max {
				t.Errorf("got %d, want between %d and %d", got, tt.min, tt.max)
			}
		})
	}
}

// decodeBody is how a handler holds a request: decoded once into a map.
func decodeBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return body
}

func TestRequestPreviewFromBody(t *testing.T) {
	for _, tt := range previewTests {
		if !json.Valid([]byte(tt.req)) {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			got := RequestPreviewFromBody(decodeBody(t, tt.req), DefaultRequestPreviewLen)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
	body := decodeBody(t, `{"messages":[{"role":"user","content":"你好世界你好世界"}]}`)
	if got := RequestPreviewFromBody(body, 4); got != "你好世界" {
		t.Errorf("unicode truncate: got %q, want %q", got, "你好世界")
	}
}

func TestEstimateInTokensFromBody(t *testing.T) {
	for _, tt := range estimateTests {
		if !json.Valid([]byte(tt.req)) {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateInTokensFromBody(decodeBody(t, tt.req))
			if got < tt.min || got > tt.max {
				t.Errorf("got %d, want between %d and %d", got, tt.min, tt.max)
			}
			if raw := EstimateInTokens(json.RawMessage(tt.req)); got != raw {
				t.Errorf("got %d, EstimateInTokens of the raw text gives %d", got, raw)
			}
		})
	}
}

// TestFromBodyDoesNotAllocatePerMessage pins the walkers to the decoded
// value: a body of many empty messages (the review's worst case) must not cost
// an allocation per message.
func TestFromBodyDoesNotAllocatePerMessage(t *testing.T) {
	const n = 100_000
	msgs := make([]any, 0, n+1)
	for range n {
		msgs = append(msgs, map[string]any{})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "hi"})
	body := map[string]any{"messages": msgs}

	var preview string
	if allocs := testing.AllocsPerRun(5, func() {
		preview = RequestPreviewFromBody(body, DefaultRequestPreviewLen)
	}); allocs > 2 {
		t.Errorf("RequestPreviewFromBody: %v allocations per run, want at most 2", allocs)
	}
	if preview != "hi" {
		t.Errorf("preview = %q, want hi", preview)
	}

	var tokens int
	if allocs := testing.AllocsPerRun(5, func() {
		tokens = EstimateInTokensFromBody(body)
	}); allocs > 2 {
		t.Errorf("EstimateInTokensFromBody: %v allocations per run, want at most 2", allocs)
	}
	if want := (len("hi") + (n+1)*4 + 3) / 4; tokens != want {
		t.Errorf("tokens = %d, want %d", tokens, want)
	}
}
