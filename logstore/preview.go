package logstore

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// DefaultRequestPreviewLen is how many characters of inbound request content
// are stored in the index row for the web UI list view.
const DefaultRequestPreviewLen = 16

// RequestContentPreview is RequestPreviewFromBody for a raw request body.
// Invalid JSON has no preview.
func RequestContentPreview(req json.RawMessage, maxLen int) string {
	if len(req) == 0 || maxLen <= 0 {
		return ""
	}
	var body map[string]any
	if err := json.Unmarshal(req, &body); err != nil {
		return ""
	}
	return RequestPreviewFromBody(body, maxLen)
}

// RequestPreviewFromBody extracts a short plain-text preview from a decoded
// inbound request body: the first message content for chat, the first
// Responses input item with text, or the input field for embeddings. Returns
// empty when nothing recognizable is present.
func RequestPreviewFromBody(body map[string]any, maxLen int) string {
	return truncateRunes(requestText(body), maxLen)
}

// EstimateInTokens is EstimateInTokensFromBody for a raw request body. Invalid
// JSON is estimated from its size.
func EstimateInTokens(req json.RawMessage) int {
	if len(req) == 0 {
		return 0
	}
	var body map[string]any
	if err := json.Unmarshal(req, &body); err != nil {
		return (len(req) + 3) / 4
	}
	return EstimateInTokensFromBody(body)
}

// EstimateInTokensFromBody returns a rough inbound token count for live UI
// previews: message and input text at ~4 runes per token, plus a small
// per-item overhead. A body with no text estimates 0.
func EstimateInTokensFromBody(body map[string]any) int {
	messages, _ := body["messages"].([]any)
	chars := 0
	for _, msg := range messages {
		m, _ := msg.(map[string]any)
		chars += utf8.RuneCountInString(contentText(m["content"]))
	}
	chars += inputTextRunes(body["input"])
	if chars == 0 {
		return 0
	}
	items, _ := body["input"].([]any)
	chars += (len(messages) + len(items)) * 4 // rough per-item overhead
	return (chars + 3) / 4
}

func requestText(body map[string]any) string {
	if text := messagesText(body["messages"]); text != "" {
		return text
	}
	if text := inputText(body["input"]); text != "" {
		return text
	}
	return contentText(body["content"])
}

// messagesText is the first non-empty message content.
func messagesText(v any) string {
	messages, _ := v.([]any)
	for _, msg := range messages {
		m, _ := msg.(map[string]any)
		if content := contentText(m["content"]); content != "" {
			return content
		}
	}
	return ""
}

// contentText flattens an OpenAI-style "content" value (string or array of
// text parts) into plain text.
func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			p, _ := part.(map[string]any)
			if t, ok := p["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	}
	return ""
}

// allStrings reports whether items is a non-empty array of strings: an
// embeddings input.
func allStrings(items []any) bool {
	for _, item := range items {
		if _, ok := item.(string); !ok {
			return false
		}
	}
	return len(items) > 0
}

func inputText(v any) string {
	if text := contentText(v); text != "" {
		return text
	}
	items, _ := v.([]any)
	if allStrings(items) {
		// Embeddings: array of strings, concatenated.
		var b strings.Builder
		for _, item := range items {
			b.WriteString(item.(string))
		}
		return b.String()
	}
	// Responses: array of role/typed items — first non-empty content, matching
	// messagesText (chat) so the UI preview is not blank.
	for _, item := range items {
		if text := responsesInputItemText(item); text != "" {
			return text
		}
	}
	return ""
}

// inputTextRunes sums text across an embeddings string/array or Responses
// input item list for live in-token estimates.
func inputTextRunes(v any) int {
	if text := contentText(v); text != "" {
		return utf8.RuneCountInString(text)
	}
	items, _ := v.([]any)
	n := 0
	for _, item := range items {
		if s, ok := item.(string); ok {
			n += utf8.RuneCountInString(s)
			continue
		}
		n += utf8.RuneCountInString(responsesInputItemText(item))
	}
	return n
}

// responsesInputItemText extracts plain text from one Responses input item
// (role message with content string/parts, or function_call_output.output).
func responsesInputItemText(v any) string {
	m, _ := v.(map[string]any)
	if text := contentText(m["content"]); text != "" {
		return text
	}
	s, _ := m["output"].(string)
	return s
}

func truncateRunes(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	n := 0
	for i := range s {
		if n == maxLen {
			return s[:i]
		}
		n++
	}
	return s
}
