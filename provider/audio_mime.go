package provider

import "strings"

// geminiAudioMIMEs maps an audio format name, as OpenAI's input_audio and
// transcription uploads name it, to the MIME type Gemini expects.
var geminiAudioMIMEs = map[string]string{
	"wav":  "audio/wav",
	"mp3":  "audio/mp3",
	"ogg":  "audio/ogg",
	"opus": "audio/ogg",
	"webm": "audio/webm",
	"flac": "audio/flac",
	"aac":  "audio/aac",
	"m4a":  "audio/mp4",
	"mp4":  "audio/mp4",
	"aiff": "audio/aiff",
}

// geminiAudioMIME is the MIME type Gemini expects for an audio format; WAV when
// the format is not given.
func geminiAudioMIME(format string) string {
	f := strings.ToLower(strings.TrimSpace(format))
	if m, ok := geminiAudioMIMEs[f]; ok {
		return m
	}
	if f == "" {
		return "audio/wav"
	}
	return "audio/" + f
}

// splitDataURI splits a base64 data URI into its MIME type and data. A value
// that is not a data URI comes back as ("", s).
func splitDataURI(s string) (mime, b64 string) {
	rest, ok := strings.CutPrefix(s, "data:")
	if !ok {
		return "", s
	}
	meta, data, ok := strings.Cut(rest, ",")
	if !ok {
		return "", s
	}
	mime, _, _ = strings.Cut(meta, ";")
	return mime, data
}

// audioFormatAliases maps other names for a format (MIME subtypes, file
// extensions) to the router's own.
var audioFormatAliases = map[string]string{
	"wave": "wav", "x-wav": "wav", "vnd.wave": "wav",
	"mpeg": "mp3", "mpga": "mp3", "mpeg3": "mp3", "x-mp3": "mp3",
	"oga": "ogg", "opus": "ogg", "vorbis": "ogg",
	"x-m4a": "m4a", "x-flac": "flac", "x-aac": "aac", "aif": "aiff", "x-aiff": "aiff",
	"l16": "pcm",
}

// NormalizeAudioFormat turns a format name, file extension or audio MIME type
// into one of the router's format names, dropping any parameters. It reports
// false for anything it doesn't know.
func NormalizeAudioFormat(s string) (string, bool) {
	f := strings.ToLower(strings.TrimSpace(s))
	f, _, _ = strings.Cut(f, ";")
	f = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(f, "audio/"), "."))
	if a, ok := audioFormatAliases[f]; ok {
		f = a
	}
	return f, knownAudioFormat(f)
}
