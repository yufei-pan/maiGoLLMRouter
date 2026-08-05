package provider

import (
	"regexp"
	"strings"
)

// englishPronounRE matches whole-word I/me/my/mine (case-insensitive).
// Longer tokens (mine) are listed before shorter ones that share a prefix (my)
// is unnecessary with word boundaries; order still keeps replacements clear.
var englishPronounRE = regexp.MustCompile(`(?i)\b(mine|my|me|i)\b`)

// CoerceTrailingAssistantTurn forces the last role-bearing item in body[field]
// to role "user" when it is not already user/tool/function, and rewrites
// first-person pronouns in that turn's text. field is "messages" or "input".
// Returns body unchanged when there is nothing to do. Never mutates the
// shared last-item map in place.
func CoerceTrailingAssistantTurn(body map[string]any, field string) map[string]any {
	if body == nil {
		return body
	}
	items, ok := body[field].([]any)
	if !ok || len(items) == 0 {
		return body
	}
	last, ok := items[len(items)-1].(map[string]any)
	if !ok {
		return body
	}
	role := asString(last, "role")
	if role == "" || role == "user" || role == "tool" || role == "function" {
		return body
	}

	copiedLast := make(map[string]any, len(last)+1)
	for k, v := range last {
		copiedLast[k] = v
	}
	copiedLast["role"] = "user"
	if c, exists := copiedLast["content"]; exists {
		copiedLast["content"] = rewritePronounContent(c)
	}

	copiedItems := append([]any(nil), items...)
	copiedItems[len(copiedItems)-1] = copiedLast

	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	out[field] = copiedItems
	return out
}

func rewritePronounContent(content any) any {
	switch c := content.(type) {
	case string:
		return rewritePronounText(c)
	case []any:
		out := make([]any, len(c))
		for i, part := range c {
			m, ok := part.(map[string]any)
			if !ok {
				out[i] = part
				continue
			}
			copied := make(map[string]any, len(m))
			for k, v := range m {
				copied[k] = v
			}
			for _, key := range []string{"text", "input_text", "output_text"} {
				if s, ok := copied[key].(string); ok {
					copied[key] = rewritePronounText(s)
				}
			}
			out[i] = copied
		}
		return out
	default:
		return content
	}
}

func rewritePronounText(s string) string {
	s = englishPronounRE.ReplaceAllStringFunc(s, func(m string) string {
		switch strings.ToLower(m) {
		case "i", "me":
			return "you"
		case "my":
			return "your"
		case "mine":
			return "yours"
		default:
			return m
		}
	})
	return strings.ReplaceAll(s, "我", "你")
}
