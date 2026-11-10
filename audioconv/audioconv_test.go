package audioconv

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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

// TestConvertRefusesPlaylists verifies untrusted input can't make ffmpeg open
// anything but the audio it was handed: a playlist naming a local file fails
// instead of converting that file.
func TestConvertRefusesPlaylists(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	dir := t.TempDir()
	secret := dir + "/secret.wav"
	if err := os.WriteFile(secret, WrapPCM(sine(1), 24000), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir) // ffmpeg runs in the router's working directory: relative names resolve there
	for name, input := range map[string]string{
		"hls":             "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nfile://" + secret + "\n#EXT-X-ENDLIST\n",
		"hls relative":    "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nsecret.wav\n#EXT-X-ENDLIST\n",
		"concat":          "ffconcat version 1.0\nfile '" + secret + "'\n",
		"concat relative": "ffconcat version 1.0\nfile 'secret.wav'\n",
	} {
		if out, _, err := Convert(context.Background(), ffmpeg, []byte(input), "audio/wav", "opus"); err == nil {
			t.Errorf("%s: converted %d bytes from a file it was never given", name, len(out))
		}
	}
}

// TestConvertAcceptsAudioContainers verifies the input whitelist still lets
// every audio container a client may record in through.
func TestConvertAcceptsAudioContainers(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	dir := t.TempDir()
	for _, c := range []struct{ muxer, ct string }{
		{"ogg", "audio/ogg"}, {"webm", "audio/webm"}, {"mp3", "audio/mpeg"},
		{"flac", "audio/flac"}, {"adts", "audio/aac"}, {"mp4", "audio/mp4"},
	} {
		path := dir + "/clip." + c.muxer
		args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-ac", "1"}
		if c.muxer == "mp4" {
			args = append(args, "-c:a", "aac")
		}
		if out, err := exec.Command(ffmpeg, append(args, "-f", c.muxer, "-y", path)...).CombinedOutput(); err != nil {
			t.Fatalf("making a %s clip: %v %s", c.muxer, err, out)
		}
		in, _ := os.ReadFile(path)
		if _, ct, err := Convert(context.Background(), ffmpeg, in, c.ct, "opus"); err != nil || ct != "audio/ogg" {
			t.Errorf("%s: %v", c.muxer, err)
		}
	}
}

// TestConvertClampsPCMRate verifies a raw PCM rate from outside (a client's
// format string) can't stretch a little input into hours of output.
func TestConvertClampsPCMRate(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	out, _, err := Convert(context.Background(), ffmpeg, make([]byte, 200), "audio/pcm;rate=1", "pcm")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 10_000 {
		t.Errorf("100 samples became %d bytes: the rate was not clamped", len(out))
	}
}

// makeVideoClip writes a one-second clip with a video stream (and an audio
// stream when withAudio), as a client could upload it.
func makeVideoClip(t *testing.T, ffmpeg string, withAudio bool) []byte {
	t.Helper()
	path := t.TempDir() + "/clip.webm"
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=1"}
	if withAudio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "libopus", "-shortest")
	}
	args = append(args, "-c:v", "libvpx", "-f", "webm", "-y", path)
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		t.Skipf("can't make a video clip here: %v %s", err, out)
	}
	b, _ := os.ReadFile(path)
	return b
}

// TestConvertDropsVideo verifies only the audio of an upload is decoded and
// kept: a video stream never reaches a decoder or the output.
func TestConvertDropsVideo(t *testing.T) {
	ffmpeg := needFFmpeg(t)
	out, ct, err := Convert(context.Background(), ffmpeg, makeVideoClip(t, ffmpeg, true), "audio/webm", "opus")
	if err != nil {
		t.Fatal(err)
	}
	if ct != "audio/ogg" || bytes.Contains(out, []byte("theora")) || !bytes.Contains(out, []byte("OpusHead")) {
		t.Errorf("type %q; output carries video: %v", ct, bytes.Contains(out, []byte("theora")))
	}
	if _, _, err := Convert(context.Background(), ffmpeg, makeVideoClip(t, ffmpeg, false), "audio/webm", "opus"); err == nil {
		t.Error("a video-only upload should fail, not turn into a video")
	}
}

// withSlots runs a test with n conversion slots instead of one per core.
func withSlots(t *testing.T, n int) {
	t.Helper()
	old := convSlots
	convSlots = make(chan struct{}, n)
	t.Cleanup(func() { convSlots = old })
}

// TestConvertBoundsConcurrentRuns verifies no more than the slot count of
// ffmpeg processes run at once. Each fake ffmpeg records how many are alive
// when it starts, then stays alive until the test releases it, so the runs
// overlap however loaded the host is: the test waits for every slot to be
// taken, checks the extra Convert starts nothing, and only then lets go.
func TestConvertBoundsConcurrentRuns(t *testing.T) {
	const slots = 2
	withSlots(t, slots)
	runs, ctl := t.TempDir(), t.TempDir() // a run file per live fake; the release file and the counts
	release, logPath := ctl+"/release", ctl+"/counts"
	// 6000 polls of 10 ms cap a fake at about 60 s, so a broken test can't hang;
	// well past the test's own 10 s wait, so no fake gives up before its release.
	script := fmt.Sprintf(`#!/bin/sh
touch %[1]s/run.$$
ls %[1]s | wc -l >> %[2]s
i=0
while [ ! -e %[3]s ] && [ $i -lt 6000 ]; do sleep 0.01; i=$((i+1)); done
rm -f %[1]s/run.$$
echo audio
`, runs, logPath, release)
	fake := t.TempDir() + "/ffmpeg"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	running := func() int {
		entries, err := os.ReadDir(runs)
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}

	var wg sync.WaitGroup
	// However the test ends, let the fakes go and wait for the Converts.
	defer func() {
		os.WriteFile(release, nil, 0o600)
		wg.Wait()
	}()
	for range slots + 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := Convert(context.Background(), fake, sine(1), "audio/pcm", "opus"); err != nil {
				t.Error(err)
			}
		}()
	}

	// 1. Every slot gets a run.
	for deadline := time.Now().Add(10 * time.Second); running() < slots; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d ffmpeg runs started within 10 s", running(), slots)
		}
	}
	// 2. The extra Convert waits: no further run shows up while the slots are full.
	for end := time.Now().Add(200 * time.Millisecond); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if n := running(); n != slots {
			t.Fatalf("%d ffmpeg runs alive while all %d slots were busy", n, slots)
		}
	}
	// 3 and 4. Release the fakes and wait for every Convert.
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	peak, started := 0, 0
	for _, line := range strings.Fields(string(raw)) {
		n, _ := strconv.Atoi(line)
		peak, started = max(peak, n), started+1
	}
	if started != slots+1 {
		t.Errorf("%d runs recorded, want %d", started, slots+1)
	}
	if peak != slots {
		t.Errorf("peak of %d ffmpeg alive at once, want exactly %d", peak, slots)
	}
}

// TestConvertWaiterHonorsContext verifies a conversion waiting for a slot
// gives up with its context's error, without starting ffmpeg.
func TestConvertWaiterHonorsContext(t *testing.T) {
	withSlots(t, 1)
	convSlots <- struct{}{} // the only slot is busy
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := Convert(ctx, "/nonexistent/ffmpeg", sine(1), "audio/pcm", "opus")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context's deadline error and no attempt to start ffmpeg", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v to give up", d)
	}
	if len(convSlots) != 1 {
		t.Errorf("%d slots held, want only the busy one", len(convSlots))
	}
}

// TestConvertReleasesSlot verifies a finished conversion, failed or not, frees its slot.
func TestConvertReleasesSlot(t *testing.T) {
	withSlots(t, 1)
	for range 3 {
		if _, _, err := Convert(context.Background(), "/nonexistent/ffmpeg", sine(1), "audio/pcm", "opus"); err == nil {
			t.Fatal("want an error without ffmpeg")
		}
	}
	if len(convSlots) != 0 {
		t.Errorf("%d slots still held", len(convSlots))
	}
}
