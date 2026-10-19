// Package audioconv converts speech between the formats clients ask for, using
// ffmpeg, and wraps raw PCM in a WAV header when ffmpeg can't help.
package audioconv

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"mime"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultPCMRate is the sample rate of "pcm": 24 kHz, mono, 16-bit signed
// little-endian, as OpenAI, OpenRouter and Gemini produce it.
const DefaultPCMRate = 24000

// Limits on one conversion. The input may come from a client, so a stated PCM
// rate is clamped (a tiny rate would stretch a little input into hours of
// audio), and ffmpeg's output and run time are capped.
const (
	minPCMRate     = 8000
	maxPCMRate     = 48000
	maxOutputBytes = 64 << 20
	maxRunTime     = 60 * time.Second
)

type encoding struct {
	args        []string
	contentType string
}

// encodings are the ffmpeg output arguments and content type for each speech
// format a client may ask for.
var encodings = map[string]encoding{
	"opus": {[]string{"-c:a", "libopus", "-b:a", "32k", "-ac", "1", "-application", "voip", "-f", "ogg"}, "audio/ogg"},
	"mp3":  {[]string{"-c:a", "libmp3lame", "-b:a", "64k", "-ac", "1", "-f", "mp3"}, "audio/mpeg"},
	"aac":  {[]string{"-c:a", "aac", "-b:a", "48k", "-ac", "1", "-f", "adts"}, "audio/aac"},
	"flac": {[]string{"-c:a", "flac", "-f", "flac"}, "audio/flac"},
	"wav":  {[]string{"-c:a", "pcm_s16le", "-f", "wav"}, "audio/wav"},
	"pcm":  {[]string{"-f", "s16le", "-ar", strconv.Itoa(DefaultPCMRate), "-ac", "1"}, "audio/pcm"},
}

// inputFormats are the ffmpeg demuxers audio may arrive in.
const inputFormats = "wav,ogg,mp3,flac,aac,matroska,webm,mov,mp4,m4a,s16le"

// ContentTypeFor is the content type of a speech format, or "" for an unknown one.
func ContentTypeFor(format string) string {
	return encodings[format].contentType
}

func mediaType(contentType string) (string, map[string]string) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt = strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
	}
	return strings.ToLower(mt), params
}

// Matches reports whether audio of contentType is already in format.
func Matches(contentType, format string) bool {
	mt, _ := mediaType(contentType)
	switch format {
	case "opus":
		return mt == "audio/ogg" || mt == "audio/opus"
	case "mp3":
		return mt == "audio/mpeg" || mt == "audio/mp3"
	case "aac":
		return mt == "audio/aac"
	case "flac":
		return mt == "audio/flac"
	case "wav":
		return mt == "audio/wav" || mt == "audio/x-wav" || mt == "audio/wave"
	case "pcm":
		return IsRawPCM(contentType) && PCMRate(contentType) == DefaultPCMRate
	}
	return false
}

// IsRawPCM reports headerless 16-bit PCM (audio/pcm, or Gemini's audio/L16).
func IsRawPCM(contentType string) bool {
	mt, _ := mediaType(contentType)
	return mt == "audio/pcm" || mt == "audio/l16"
}

// PCMRate is the sample rate a raw PCM content type states, else 24 kHz.
func PCMRate(contentType string) int {
	_, params := mediaType(contentType)
	if r, err := strconv.Atoi(params["rate"]); err == nil && r > 0 {
		return r
	}
	return DefaultPCMRate
}

// WrapPCM puts raw mono 16-bit PCM in a WAV file, so any player can play it.
func WrapPCM(pcm []byte, rate int) []byte {
	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	le(uint32(36 + len(pcm)))
	b.WriteString("WAVEfmt ")
	le(uint32(16))       // fmt chunk size
	le(uint16(1))        // PCM
	le(uint16(1))        // mono
	le(uint32(rate))     // sample rate
	le(uint32(rate * 2)) // byte rate
	le(uint16(2))        // block align
	le(uint16(16))       // bits per sample
	b.WriteString("data")
	le(uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

// Convert re-encodes audio of contentType into format with ffmpeg, returning
// the audio and its content type. Raw PCM is read as mono s16le at its stated
// rate (Gemini's audio/L16 is little-endian too); anything else is probed. It
// stops ffmpeg when ctx ends.
func Convert(ctx context.Context, ffmpeg string, data []byte, contentType, format string) ([]byte, string, error) {
	enc, ok := encodings[format]
	if !ok {
		return nil, "", fmt.Errorf("unknown speech format %q", format)
	}
	// The input may come from a client: ffmpeg may read nothing but the pipe it
	// is handed, and only as an audio container, never a playlist or a list
	// of other files.
	args := []string{"-hide_banner", "-loglevel", "error",
		"-protocol_whitelist", "pipe", "-format_whitelist", inputFormats}
	if IsRawPCM(contentType) {
		rate := min(max(PCMRate(contentType), minPCMRate), maxPCMRate)
		args = append(args, "-f", "s16le", "-ar", strconv.Itoa(rate), "-ac", "1")
	}
	args = append(args, "-i", "pipe:0")
	args = append(args, enc.args...)
	args = append(args, "pipe:1")

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxRunTime)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	out := &cappedBuffer{max: maxOutputBytes}
	cmd.Stdout, cmd.Stderr = out, &stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, "", fmt.Errorf("ffmpeg: %w: %s", err, msg)
		}
		return nil, "", fmt.Errorf("ffmpeg: %w", err)
	}
	if out.Len() == 0 {
		return nil, "", errors.New("ffmpeg produced no audio")
	}
	return out.Bytes(), enc.contentType, nil
}

// cappedBuffer collects ffmpeg's output and refuses more than max bytes, which
// stops ffmpeg (its writes fail) and fails the conversion.
type cappedBuffer struct {
	bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		return 0, errors.New("ffmpeg output over the size limit")
	}
	return b.Buffer.Write(p)
}
