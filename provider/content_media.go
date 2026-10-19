package provider

import (
	"encoding/json"
	"mime"
	neturl "net/url"
	"path"
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
		mediaType, base64Data, ok = parseDataURL(url, "image/png")
		return "", mediaType, base64Data, ok
	}
	return url, "", "", true
}

// parseDataURL splits data:[<mediatype>][;base64],<data> into its media type
// (defaultType when absent) and payload.
func parseDataURL(s, defaultType string) (mediaType, data string, ok bool) {
	rest := strings.TrimPrefix(s, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", "", false
	}
	meta, data := rest[:comma], rest[comma+1:]
	mediaType = defaultType
	if semi := strings.Index(meta, ";"); semi >= 0 {
		if mt := strings.TrimSpace(meta[:semi]); mt != "" {
			mediaType = mt
		}
	} else if mt := strings.TrimSpace(meta); mt != "" {
		mediaType = mt
	}
	if data == "" {
		return "", "", false
	}
	return mediaType, data, true
}

// filePart is a file reference from an OpenAI Chat "file" part: either base64
// Data with its MediaType, or a remote URL (MediaType guessed, may be empty).
type filePart struct {
	URL, MediaType, Data, Filename string
}

// parseFilePart reads an OpenAI Chat "file" part value. file_data may be a data
// URL or raw base64 typed by the filename. A file_id alone yields ok=false: it
// names a file in the OpenAI file store that other dialects cannot read.
func parseFilePart(v any) (f filePart, ok bool) {
	m, isMap := v.(map[string]any)
	if !isMap {
		return filePart{}, false
	}
	f.Filename = asString(m, "filename")
	if data := strings.TrimSpace(asString(m, "file_data")); data != "" {
		if strings.HasPrefix(data, "data:") {
			f.MediaType, f.Data, ok = parseDataURL(data, mediaTypeByName(f.Filename))
			return f, ok
		}
		f.MediaType, f.Data = mediaTypeByName(f.Filename), data
		return f, true
	}
	if u := strings.TrimSpace(asString(m, "file_url")); u != "" {
		f.URL = u
		f.MediaType = mediaTypeByName(f.Filename)
		if f.MediaType == "" {
			if parsed, err := neturl.Parse(u); err == nil {
				f.MediaType = mediaTypeByName(parsed.Path)
			}
		}
		return f, true
	}
	return filePart{}, false
}

// mediaTypeByName guesses a bare media type (no parameters) from a file name's
// extension, or returns "" when the extension is unknown.
func mediaTypeByName(name string) string {
	t := mime.TypeByExtension(path.Ext(name))
	if semi := strings.Index(t, ";"); semi >= 0 {
		t = strings.TrimSpace(t[:semi])
	}
	return t
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
