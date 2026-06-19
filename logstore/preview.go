package logstore

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// DefaultRequestPreviewLen is how many characters of inbound request content
// are stored in the index row for the web UI list view.
const DefaultRequestPreviewLen = 16

// requestContentPreview extracts a short plain-text preview from an inbound
// request body: the first message content for chat, or the input field for
// embeddings. Returns empty when nothing recognizable is present.
func requestContentPreview(req json.RawMessage, maxLen int) string {
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
