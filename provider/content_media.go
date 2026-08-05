package provider

import (
	"encoding/json"
	"strings"
)

// parseImageURL extracts a usable image reference from an OpenAI image_url part
// value (string URL, or {"url": "..."}). Data URLs are split into media type and
// base64 payload; http(s) URLs are returned as-is.
func parseImageURL(v any) (url, mediaType, base64Data string, ok bool) {
	switch t := v.(type) {
	case string:
		url = t
	case map[string]any:
		url = asString(t, "url")
	default:
		return "", "", "", false
	}
	url = strings.TrimSpace(url)
	if url == "" {
		return "", "", "", false
	}
	if strings.HasPrefix(url, "data:") {
		// data:[<mediatype>][;base64],<data>
		rest := strings.TrimPrefix(url, "data:")
		comma := strings.Index(rest, ",")
		if comma < 0 {
			return "", "", "", false
		}
		meta, data := rest[:comma], rest[comma+1:]
		mediaType = "image/png"
		if semi := strings.Index(meta, ";"); semi >= 0 {
			if mt := strings.TrimSpace(meta[:semi]); mt != "" {
				mediaType = mt
			}
		} else if mt := strings.TrimSpace(meta); mt != "" {
			mediaType = mt
		}
		if data == "" {
			return "", "", "", false
		}
		return "", mediaType, data, true
	}
	return url, "", "", true
}

// openAIContentParts returns the content value as a slice of part maps. A plain
// string becomes a single text part. Non-array/non-string content yields nil.
func openAIContentParts(content any) []map[string]any {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": c}}
	case []any:
		parts := make([]map[string]any, 0, len(c))
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			parts = append(parts, pm)
		}
		return parts
	default:
		return nil
	}
}

// contentHasImage reports whether OpenAI-style content includes an image part.
func contentHasImage(content any) bool {
	for _, p := range openAIContentParts(content) {
		if asString(p, "type") == "image_url" {
			return true
		}
	}
	return false
}

// jsonObjectOrWrap unmarshals text as a JSON object, or wraps it under "result".
func jsonObjectOrWrap(text string) map[string]any {
	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil || obj == nil {
		return map[string]any{"result": text}
	}
	return obj
}
