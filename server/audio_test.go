package server

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os/exec"
	"strings"
	"testing"
	"time"

	"maiGoLLMRouter/inflight"
	"maiGoLLMRouter/logstore"
	"maiGoLLMRouter/router"
)

// audioServer runs the API against one provider block (serving model "m" as
// "p/real") with extra [server] settings.
func audioServer(t *testing.T, serverExtra, providerBlock string, logs *logstore.Store) *httptest.Server {
	t.Helper()
	cfg := loadCfg(t, fmt.Sprintf(`
[server]
client_keys = ["sk-test"]
%s

[[provider]]
name = "p"
keys = ["sk-upstream"]
%s

[model."m"]
targets = ["p/real"]
`, serverExtra, providerBlock))
	srv := New(cfg, router.New(cfg), logs, inflight.New())
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(srv.WithCORS(mux))
	t.Cleanup(ts.Close)
	return ts
}

func openAIBlock(url string) string { return fmt.Sprintf("kind = \"openai\"\nbase_url = %q", url) }
func geminiBlock(url string) string { return fmt.Sprintf("kind = \"gemini\"\nbase_url = %q", url) }

// sinePCM is one second of a 440 Hz tone as 24 kHz mono s16le.
func sinePCM() []byte {
	b := make([]byte, 48000)
	for i := range 24000 {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(int16(8000*math.Sin(2*math.Pi*440*float64(i)/24000))))
	}
	return b
}

func pcmBackend(t *testing.T) *httptest.Server {
	t.Helper()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write(sinePCM())
	}))
	t.Cleanup(b.Close)
	return b
}

func TestSpeechRouteConvertsToOpus(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	ts := audioServer(t, "", openAIBlock(pcmBackend(t).URL), nil)
	resp := post(t, ts, "/v1/audio/speech", `{"model":"m","input":"Hello","response_format":"opus"}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/ogg" || !bytes.HasPrefix(body, []byte("OggS")) {
		t.Fatalf("status %d, type %q, starts %q", resp.StatusCode, resp.Header.Get("Content-Type"), body[:min(8, len(body))])
	}
}

func TestSpeechRouteWithoutFFmpegSendsWav(t *testing.T) {
	ts := audioServer(t, `ffmpeg = "/nonexistent/ffmpeg"`, openAIBlock(pcmBackend(t).URL), nil)
	resp := post(t, ts, "/v1/audio/speech", `{"model":"m","input":"Hello","response_format":"opus"}`)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/wav" || !bytes.HasPrefix(body, []byte("RIFF")) {
		t.Fatalf("status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestSpeechRouteRejectsUnknownFormat(t *testing.T) {
	ts := audioServer(t, "", openAIBlock(pcmBackend(t).URL), nil)
	if resp := post(t, ts, "/v1/audio/speech", `{"model":"m","input":"Hello","response_format":"ogg"}`); resp.StatusCode != 400 {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
}

// postMultipart posts a transcription upload: fields, plus a file part.
func postMultipart(t *testing.T, ts *httptest.Server, fields map[string]string, filename, partType string, data []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", partType)
	pw, _ := mw.CreatePart(h)
	_, _ = pw.Write(data)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/audio/transcriptions", &body)
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestTranscriptionMultipartAndJSON(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"text":"hi"}`) }))
	defer backend.Close()
	ts := audioServer(t, "", openAIBlock(backend.URL), nil)

	resp := postMultipart(t, ts, map[string]string{"model": "m"}, "speech.wav", "audio/wav", []byte("RIFFdata"))
	if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || string(body) != `{"text":"hi"}` {
		t.Errorf("multipart: %d %s", resp.StatusCode, body)
	}
	resp = post(t, ts, "/v1/audio/transcriptions", `{"model":"m","input_audio":{"data":"UklGRg==","format":"wav"}}`)
	if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || string(body) != `{"text":"hi"}` {
		t.Errorf("json: %d %s", resp.StatusCode, body)
	}
	resp = postMultipart(t, ts, map[string]string{"model": "m", "response_format": "text"}, "speech.wav", "audio/wav", []byte("RIFFdata"))
	if body, _ := io.ReadAll(resp.Body); !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") || string(body) != "hi" {
		t.Errorf("text: %q %s", resp.Header.Get("Content-Type"), body)
	}
}

func TestTranscriptionFormatFallsBackToWav(t *testing.T) {
	var mime string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Contents []struct {
				Parts []struct {
					InlineData *struct {
						MimeType string `json:"mimeType"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"contents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, p := range body.Contents[0].Parts {
			if p.InlineData != nil {
				mime = p.InlineData.MimeType
			}
		}
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`)
	}))
	defer backend.Close()
	ts := audioServer(t, "", geminiBlock(backend.URL), nil)
	resp := postMultipart(t, ts, map[string]string{"model": "m"}, "blob", "application/octet-stream", []byte("RIFFdata"))
	if body, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || mime != "audio/wav" {
		t.Errorf("status %d %s, Gemini got %q", resp.StatusCode, body, mime)
	}
}

// lastLog waits for the newest log entry and returns it decoded.
func lastLog(t *testing.T, logs *logstore.Store) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(logs.Recent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	recent := logs.Recent()
	if len(recent) == 0 {
		t.Fatal("no log entry")
	}
	raw, err := logs.ReadEntry(recent[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func TestAudioLogsHoldNoBytes(t *testing.T) {
	logs, _ := logstore.New(t.TempDir())
	ts := audioServer(t, `ffmpeg = "/nonexistent/ffmpeg"`, openAIBlock(pcmBackend(t).URL), logs)
	resp := post(t, ts, "/v1/audio/speech", `{"model":"m","input":"Hello","response_format":"opus"}`)
	_, _ = io.ReadAll(resp.Body)
	if r, _ := lastLog(t, logs)["response"].(map[string]any); r["audio"] != "<48044 bytes audio/wav>" {
		t.Errorf("speech log response = %v", r)
	}

	logs2, _ := logstore.New(t.TempDir())
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"text":"hi"}`) }))
	defer backend.Close()
	ts2 := audioServer(t, "", openAIBlock(backend.URL), logs2)
	clip := base64.StdEncoding.EncodeToString([]byte("RIFF-a-recording"))
	resp = post(t, ts2, "/v1/audio/transcriptions", `{"model":"m","input_audio":{"data":"`+clip+`","format":"wav"}}`)
	_, _ = io.ReadAll(resp.Body)
	entry := lastLog(t, logs2)
	req, _ := json.Marshal(entry["request"])
	if strings.Contains(string(req), clip) || !strings.Contains(string(req), "16 bytes audio/wav") {
		t.Errorf("transcription log request = %s", req)
	}
}
