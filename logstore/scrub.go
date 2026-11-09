package logstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
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
	if out, ok := ScrubAudioBody(body); ok {
		return out
	}
	return raw
}

// ScrubAudioBody returns the JSON to log and true when body holds input_audio
// (each clip as "<N bytes audio/<format>>", HTML not escaped); else nil, false.
// body is never modified: the maps and slices on the way to a clip are copied.
func ScrubAudioBody(body map[string]any) ([]byte, bool) {
	var out map[string]any // body with its scrubbed lists, made at the first clip
	for _, key := range []string{"messages", "input"} {
		items, _ := body[key].([]any)
		var scrubbedItems []any
		for i, item := range items {
			msg, _ := item.(map[string]any)
			parts, _ := msg["content"].([]any)
			var scrubbedParts []any
			for j, part := range parts {
				clip, ok := scrubClip(part)
				if !ok {
					continue
				}
				if scrubbedParts == nil {
					scrubbedParts = slices.Clone(parts)
				}
				scrubbedParts[j] = clip
			}
			if scrubbedParts == nil {
				continue
			}
			if scrubbedItems == nil {
				scrubbedItems = slices.Clone(items)
			}
			scrubbedItems[i] = with(msg, "content", scrubbedParts)
		}
		if scrubbedItems == nil {
			continue
		}
		if out == nil {
			out = maps.Clone(body)
		}
		out[key] = scrubbedItems
	}
	if out == nil {
		return nil, false
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep "<N bytes …>" readable in the log
	if enc.Encode(out) != nil {
		return nil, false
	}
	return bytes.TrimRight(b.Bytes(), "\n"), true
}

// scrubClip returns a copy of an input_audio content part with its clip
// described, and true; false when part is not one.
func scrubClip(part any) (map[string]any, bool) {
	p, _ := part.(map[string]any)
	if p["type"] != "input_audio" {
		return nil, false
	}
	audio, _ := p["input_audio"].(map[string]any)
	data, ok := audio["data"].(string)
	if !ok {
		return nil, false
	}
	return with(p, "input_audio", with(audio, "data", describeAudio(data, audio["format"]))), true
}

// with returns a copy of m with key set to v.
func with(m map[string]any, key string, v any) map[string]any {
	c := maps.Clone(m)
	c[key] = v
	return c
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
