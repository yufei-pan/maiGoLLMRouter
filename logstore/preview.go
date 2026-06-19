package logstore

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// DefaultRequestPreviewLen is how many characters of inbound request content
// are stored in the index row for the web UI list view.
const DefaultRequestPreviewLen = 16

// RequestContentPreview extracts a short plain-text preview from an inbound
// request body: the first message content for chat, or the input field for
// embeddings. Returns empty when nothing recognizable is present.
func RequestContentPreview(req json.RawMessage, maxLen int) string {
	if len(req) == 0 || maxLen <= 0 {
		return ""
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(req, &body); err != nil {
		return ""
	}
	text := requestTextFromBody(body)
	if text == "" {
		return ""
	}
	return truncateRunes(text, maxLen)
}

// EstimateInTokens returns a rough inbound token count for live UI previews.
// It sums message/input text (~4 runes per token) and falls back to request
// JSON size when no text is found.
func EstimateInTokens(req json.RawMessage) int {
	if len(req) == 0 {
		return 0
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(req, &body); err != nil {
		return (len(req) + 3) / 4
	}
	chars := 0
	if raw, ok := body["messages"]; ok {
		chars += allMessageTextRunes(raw)
	}
	if raw, ok := body["input"]; ok {
		chars += utf8.RuneCountInString(jsonInputText(raw))
	}
	if chars == 0 {
		return 0
	}
	if raw, ok := body["messages"]; ok {
		var msgs []json.RawMessage
		if err := json.Unmarshal(raw, &msgs); err == nil {
			chars += len(msgs) * 4 // rough per-message overhead
		}
	}
	return (chars + 3) / 4
}

func allMessageTextRunes(raw json.RawMessage) int {
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return 0
	}
	n := 0
	for _, msg := range messages {
		n += utf8.RuneCountInString(jsonContentText(msg["content"]))
	}
	return n
}

func requestTextFromBody(body map[string]json.RawMessage) string {
	if raw, ok := body["messages"]; ok {
		if text := textFromMessages(raw); text != "" {
			return text
		}
	}
	if raw, ok := body["input"]; ok {
		if text := jsonInputText(raw); text != "" {
			return text
		}
	}
	if raw, ok := body["content"]; ok {
		return jsonContentText(raw)
	}
	return ""
}

func textFromMessages(raw json.RawMessage) string {
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return ""
	}
	for _, msg := range messages {
		if content := jsonContentText(msg["content"]); content != "" {
			return content
		}
	}
	return ""
}

func parseJSONString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// jsonContentText flattens an OpenAI-style "content" value (string or array of
// text parts) into plain text.
func jsonContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range parts {
		if t, ok := parseJSONString(part["text"]); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}

func jsonInputText(raw json.RawMessage) string {
	if text := jsonContentText(raw); text != "" {
		return text
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	var b strings.Builder
	for _, item := range items {
		if s, ok := parseJSONString(item); ok {
			b.WriteString(s)
		}
	}
	return b.String()
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
