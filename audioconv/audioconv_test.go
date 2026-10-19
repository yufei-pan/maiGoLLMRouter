package audioconv

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os/exec"
	"testing"
	"time"
)

func needFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	return path
}

// sine is seconds of a 440 Hz tone as 24 kHz mono s16le PCM.
func sine(seconds int) []byte {
	n := 24000 * seconds
	b := make([]byte, 2*n)
	for i := range n {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/24000))
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

func TestMatches(t *testing.T) {
	cases := []struct {
		ct, format string
		want       bool
	}{
		{"audio/ogg", "opus", true},
		{"audio/mpeg", "mp3", true},
		{"audio/L16;codec=pcm;rate=24000", "pcm", true},
		{"audio/wav", "wav", true},
		{"audio/wav", "opus", false},
		{"audio/L16;rate=16000", "pcm", false},
	}
	for _, c := range cases {
		if got := Matches(c.ct, c.format); got != c.want {
			t.Errorf("Matches(%q, %q) = %v, want %v", c.ct, c.format, got, c.want)
		}
	}
}

func TestWrapPCM(t *testing.T) {
	w := WrapPCM(make([]byte, 4800), 24000)
	if len(w) != 4844 || string(w[:4]) != "RIFF" || string(w[8:12]) != "WAVE" {
		t.Fatalf("header: len %d, %q %q", len(w), w[:4], w[8:12])
	}
	channels := binary.LittleEndian.Uint16(w[22:])
	rate := binary.LittleEndian.Uint32(w[24:])
	bits := binary.LittleEndian.Uint16(w[34:])
	size := binary.LittleEndian.Uint32(w[40:])
	if channels != 1 || rate != 24000 || bits != 16 || size != 4800 {
		t.Errorf("fmt: channels %d rate %d bits %d data %d", channels, rate, bits, size)
	}
}

func TestPCMRate(t *testing.T) {
	if r := PCMRate("audio/L16;codec=pcm;rate=16000"); r != 16000 {
		t.Errorf("rate = %d, want 16000", r)
	}
	if r := PCMRate("audio/pcm"); r != 24000 {
		t.Errorf("rate = %d, want 24000", r)
	}
}

func TestConvertWavToOpus(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	in := WrapPCM(sine(1), 24000)
	out, ct, err := Convert(context.Background(), ffmpeg, in, "audio/wav", "opus")
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if ct != "audio/ogg" || !bytes.HasPrefix(out, []byte("OggS")) || !bytes.Contains(out, []byte("OpusHead")) {
		t.Fatalf("type %q, starts %q", ct, out[:min(8, len(out))])
	}
	if len(out) >= len(in) {
		t.Errorf("opus (%d bytes) is not smaller than wav (%d bytes)", len(out), len(in))
	}
}

func TestConvertPCMToFormats(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	in := sine(1)
	for _, f := range []string{"mp3", "aac", "flac", "wav", "pcm"} {
		out, ct, err := Convert(context.Background(), ffmpeg, in, "audio/pcm", f)
		if err != nil || len(out) == 0 || ct != ContentTypeFor(f) {
			t.Errorf("%s: %d bytes, type %q, err %v", f, len(out), ct, err)
			continue
		}
		if f == "pcm" && (len(out) < 47998 || len(out) > 48002) {
			t.Errorf("pcm: %d bytes, want 48000 ± 2", len(out))
		}
	}
}

func TestConvertMissingFFmpeg(t *testing.T) {
	if _, _, err := Convert(context.Background(), "/nonexistent/ffmpeg", sine(1), "audio/pcm", "opus"); err == nil {
		t.Error("want an error without ffmpeg")
	}
}

func TestConvertStopsWhenCanceled(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := Convert(ctx, ffmpeg, sine(600), "audio/pcm", "flac")
	if err == nil {
		t.Fatal("want an error once canceled")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v to stop", d)
	}
}
