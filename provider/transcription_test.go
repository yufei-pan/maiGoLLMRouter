package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func transcribeReq(model, audioAPI string, tr TranscriptionRequest) Request {
	return Request{Op: OpTranscribe, Model: model, AudioAPI: audioAPI, Transcription: &tr}
}

func TestOpenAITranscriptionMultipart(t *testing.T) {
	var form map[string]string
	var file []byte
	var filename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Errorf("path %s", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		form = map[string]string{"model": r.FormValue("model"), "language": r.FormValue("language"), "response_format": r.FormValue("response_format")}
		f, h, err := r.FormFile("file")
		if err == nil {
			file, _ = io.ReadAll(f)
			filename = h.Filename
		}
		fmt.Fprint(w, `{"text":"hi"}`)
	}))
	defer srv.Close()
	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k",
		transcribeReq("real", "openai", TranscriptionRequest{Audio: []byte("RIFFdata"), Format: "wav", Language: "en"}))
	if err != nil {
		t.Fatal(err)
	}
	if form["model"] != "real" || form["language"] != "en" || form["response_format"] != "json" || string(file) != "RIFFdata" || filename != "speech.wav" {
		t.Errorf("form %v, file %q named %q", form, file, filename)
	}
	if string(resp.OpenAIBody) != `{"text":"hi"}` || !resp.HasContent {
		t.Errorf("response = %+v", resp)
	}
}

// stepfunASR records the request JSON and answers with the given SSE events.
func stepfunASR(t *testing.T, sent *map[string]any, events ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/asr/sse" || r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("path %s, accept %q", r.URL.Path, r.Header.Get("Accept"))
		}
		if sent != nil {
			_ = json.NewDecoder(r.Body).Decode(sent)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestStepfunTranscriptionSSE(t *testing.T) {
	var sent map[string]any
	srv := stepfunASR(t, &sent,
		`{"type":"transcript.text.delta","delta":"Hel"}`,
		`{"type":"transcript.text.done","text":"Hello","usage":{}}`)
	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k",
		transcribeReq("stepaudio-2.5-asr", "stepfun", TranscriptionRequest{Audio: []byte("RIFF"), Format: "wav", Language: "en"}))
	if err != nil {
		t.Fatal(err)
	}
	audio, _ := sent["audio"].(map[string]any)
	input, _ := audio["input"].(map[string]any)
	tr, _ := input["transcription"].(map[string]any)
	format, _ := input["format"].(map[string]any)
	if tr["model"] != "stepaudio-2.5-asr" || tr["language"] != "en" || tr["enable_itn"] != true || format["type"] != "wav" || audio["data"] != "UklGRg==" {
		t.Errorf("request = %v", sent)
	}
	if string(resp.OpenAIBody) != `{"text":"Hello"}` || !resp.HasContent || !resp.OK() {
		t.Errorf("response = %+v", resp)
	}
}

func TestStepfunTranscriptionErrorEvents(t *testing.T) {
	for _, c := range []struct {
		events []string
		status int
	}{
		{[]string{`{"type":"error","message":"invalid audio format"}`}, 400},
		{[]string{`{"type":"error","message":"internal error"}`}, 502},
		{[]string{`{"type":"transcript.text.delta","delta":"He"}`}, 502},
	} {
		srv := stepfunASR(t, nil, c.events...)
		resp, _ := Call(context.Background(), srv.Client(), "openai", srv.URL, "k",
			transcribeReq("stepaudio-2.5-asr", "stepfun", TranscriptionRequest{Audio: []byte("RIFF"), Format: "wav"}))
		if resp.HTTPStatus != c.status || resp.HasContent {
			t.Errorf("%v: status %d, has content %v", c.events, resp.HTTPStatus, resp.HasContent)
		}
	}
}

func TestStepfunTranscriptionConvertsWebm(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	path := t.TempDir() + "/clip.webm"
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-ac", "1", "-c:a", "libopus", "-f", "webm", "-y", path).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	webm, _ := os.ReadFile(path)
	var sent map[string]any
	srv := stepfunASR(t, &sent, `{"type":"transcript.text.done","text":""}`)
	req := transcribeReq("stepaudio-2.5-asr", "stepfun", TranscriptionRequest{Audio: webm, Format: "webm"})
	req.FFmpeg = ffmpeg
	if _, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", req); err != nil {
		t.Fatal(err)
	}
	audio, _ := sent["audio"].(map[string]any)
	input, _ := audio["input"].(map[string]any)
	if format, _ := input["format"].(map[string]any); format["type"] != "ogg" {
		t.Errorf("format sent = %v, want ogg", input["format"])
	}
}

func TestGeminiTranscription(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":" こんにちは \n"}]},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()
	resp, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k",
		transcribeReq("gemini-3.5-flash", "", TranscriptionRequest{Audio: []byte("RIFF"), Format: "wav", Language: "ja"}))
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := sent["contents"].([]any)
	first, _ := contents[0].(map[string]any)
	parts, _ := first["parts"].([]any)
	instr, _ := parts[0].(map[string]any)
	audio, _ := parts[1].(map[string]any)
	inline, _ := audio["inlineData"].(map[string]any)
	text, _ := instr["text"].(string)
	if !strings.HasPrefix(text, "Transcribe the speech in this audio exactly") || !strings.Contains(text, "The language is probably ja.") || inline["mimeType"] != "audio/wav" {
		t.Errorf("parts = %v", parts)
	}
	if string(resp.OpenAIBody) != `{"text":"こんにちは"}` || !resp.HasContent {
		t.Errorf("response = %s", resp.OpenAIBody)
	}
}

func TestGeminiTranscriptionRetriesWithoutThinking(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gc, _ := body["generationConfig"].(map[string]any)
		if _, thinking := gc["thinkingConfig"]; thinking {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"code":400,"message":"thinking level is not supported for this model"}}`)
			return
		}
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`)
	}))
	defer srv.Close()
	resp, _ := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k",
		transcribeReq("gemini-2.5-flash", "", TranscriptionRequest{Audio: []byte("RIFF"), Format: "wav"}))
	if calls != 2 || string(resp.OpenAIBody) != `{"text":"hi"}` {
		t.Errorf("calls %d, body %s", calls, resp.OpenAIBody)
	}
}

func TestEmptyTranscriptHasContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"text":""}`) }))
	defer srv.Close()
	resp, _ := Call(context.Background(), srv.Client(), "openai", srv.URL, "k",
		transcribeReq("whisper", "openai", TranscriptionRequest{Audio: []byte("RIFF"), Format: "wav"}))
	if !resp.HasContent {
		t.Errorf("an empty transcript is a transcript: %+v", resp)
	}
}

// TestStepfunTranscriptionRejectsOddFormats verifies only plain, known format
// names reach ffmpeg: anything else is refused as the request's fault.
func TestStepfunTranscriptionRejectsOddFormats(t *testing.T) {
	srv := stepfunASR(t, nil)
	req := transcribeReq("stepaudio-2.5-asr", "stepfun", TranscriptionRequest{Audio: make([]byte, 200), Format: "pcm;rate=1"})
	req.FFmpeg = "/nonexistent/ffmpeg-must-not-run"
	resp, _ := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", req)
	if resp.HTTPStatus != 400 || resp.HasContent {
		t.Errorf("odd format: status %d", resp.HTTPStatus)
	}
}

// TestStepfunErrorEventsBlackOutOnlyForKeyFaults verifies an error event on a
// stream that answered 200 blacks out the key only when it is about the key.
func TestStepfunErrorEventsBlackOutOnlyForKeyFaults(t *testing.T) {
	for msg, noBlackout := range map[string]bool{
		"internal error":       true,
		"invalid audio format": true,
		"quota exceeded":       false,
		"invalid api key":      false,
		"rate limit exceeded":  false,
	} {
		srv := stepfunASR(t, nil, `{"type":"error","message":"`+msg+`"}`)
		resp, _ := Call(context.Background(), srv.Client(), "openai", srv.URL, "k",
			transcribeReq("stepaudio-2.5-asr", "stepfun", TranscriptionRequest{Audio: []byte("RIFF"), Format: "wav"}))
		if resp.NoBlackout != noBlackout || resp.OK() {
			t.Errorf("%q: NoBlackout %v (want %v), status %d", msg, resp.NoBlackout, noBlackout, resp.HTTPStatus)
		}
	}
}
