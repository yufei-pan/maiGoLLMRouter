package logstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ScrubAudio returns a chat or Responses request body with every input_audio
// clip replaced by its size and type ("<N bytes audio/wav>"), for the log:
// recordings are kept out of it, and a client that resends earlier clips each
// turn would otherwise store them again and again. A body without audio comes
// back unchanged.
func ScrubAudio(raw []byte) []byte {
	if !bytes.Contains(raw, []byte(`"input_audio"`)) {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep numbers exactly as sent
	var body map[string]any
	if dec.Decode(&body) != nil {
		return raw
	}
	changed := false
	for _, key := range []string{"messages", "input"} {
		items, _ := body[key].([]any)
		for _, item := range items {
			m, _ := item.(map[string]any)
			parts, _ := m["content"].([]any)
			for _, part := range parts {
				pm, _ := part.(map[string]any)
				if pm["type"] != "input_audio" {
					continue
				}
				if ia, ok := pm["input_audio"].(map[string]any); ok {
					if data, ok := ia["data"].(string); ok {
						ia["data"] = describeAudio(data, ia["format"])
						changed = true
					}
				}
			}
		}
	}
	if !changed {
		return raw
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false) // keep "<N bytes …>" readable in the log
	if enc.Encode(body) != nil {
		return raw
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}

// describeAudio is "<N bytes audio/<format>>" for base64 audio, possibly a
// data URI (whose MIME type is used when no format is given).
func describeAudio(data string, format any) string {
	mime := ""
	if rest, ok := strings.CutPrefix(data, "data:"); ok {
		if meta, b64, ok := strings.Cut(rest, ","); ok {
			mime, _, _ = strings.Cut(meta, ";")
			data = b64
		}
	}
	if f, _ := format.(string); f != "" {
		mime = "audio/" + f
	}
	if mime == "" {
		mime = "audio"
	}
	return fmt.Sprintf("<%d bytes %s>", base64Len(data), mime)
}

// base64Len is the decoded length of standard base64 text.
func base64Len(s string) int {
	s = strings.TrimSpace(s)
	n := len(s) / 4 * 3
	switch {
	case strings.HasSuffix(s, "=="):
		n -= 2
	case strings.HasSuffix(s, "="):
		n--
	}
	return n
}
