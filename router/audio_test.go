package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"maiGoLLMRouter/provider"
)

// audioProviders answers for a Gemini-kind "google" (key kg), a stepfun-flavored
// OpenAI-kind "stepfun" (key ks) and a plain OpenAI-kind "openrouter" (key ko).
type audioProviders struct {
	google, stepfun, openrouter http.HandlerFunc
}

func (a audioProviders) config(t *testing.T, server, routes string) *Router {
	t.Helper()
	start := func(h http.HandlerFunc) string {
		if h == nil {
			h = func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected call to %s", r.URL.Path) }
		}
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv.URL
	}
	return New(loadCfg(t, fmt.Sprintf(`
[server]
%s

[[provider]]
name = "google"
kind = "gemini"
base_url = %q
keys = ["kg"]

[[provider]]
name = "stepfun"
kind = "openai"
audio = "stepfun"
base_url = %q
keys = ["ks"]

[[provider]]
name = "openrouter"
kind = "openai"
base_url = %q
keys = ["ko"]

[[provider]]
name = "claude"
kind = "anthropic"
base_url = "http://127.0.0.1:1"
keys = ["kc"]

%s
`, server, start(a.google), start(a.stepfun), start(a.openrouter), routes)))
}

func geminiWav(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}}]}}]}`)
}

func pcmReply(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "audio/pcm")
	_, _ = w.Write([]byte{1, 2, 3, 4})
}

func TestSpeechUsesPerTargetVoice(t *testing.T) {
	var sent map[string]any
	r := audioProviders{
		google: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		stepfun: func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&sent)
			pcmReply(w)
		},
	}.config(t, "", `
[model."shiori"]
targets = ["google/g-tts", "stepfun/s-tts"]
[model."shiori".speech."google/g-tts"]
voice = "Kore"
[model."shiori".speech."stepfun/s-tts"]
voice = "zixinnansheng"
`)
	res := r.ExecuteSpeech(context.Background(), "shiori", provider.SpeechRequest{Input: "Hello", Voice: "marin"})
	if !res.Success || res.ContentType != "audio/pcm" || string(res.Body) != "\x01\x02\x03\x04" {
		t.Fatalf("result = %+v", res)
	}
	if sent["voice"] != "zixinnansheng" || sent["response_format"] != "pcm" {
		t.Errorf("stepfun got %v", sent)
	}
}

func TestSpeechFailsOverOn400WithoutBlackout(t *testing.T) {
	r := audioProviders{
		stepfun: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"input too long"}}`)
		},
		google: geminiWav,
	}.config(t, "", `
[model."shiori"]
targets = ["stepfun/s-tts", "google/g-tts"]
`)
	res := r.ExecuteSpeech(context.Background(), "shiori", provider.SpeechRequest{Input: "Hello"})
	if !res.Success || res.ContentType != "audio/wav" {
		t.Fatalf("result = %+v", res)
	}
	if r.blackout.Blocked("ks", "s-tts") {
		t.Error("a 400 must not black out the key")
	}
}

func TestSpeechClientVoiceWithoutSettings(t *testing.T) {
	var sent map[string]any
	r := audioProviders{openrouter: func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&sent)
		pcmReply(w)
	}}.config(t, "", "")
	if res := r.ExecuteSpeech(context.Background(), "openrouter/x", provider.SpeechRequest{Input: "Hi", Voice: "alloy"}); !res.Success {
		t.Fatalf("result = %+v", res)
	}
	if sent["voice"] != "alloy" || sent["response_format"] != "pcm" {
		t.Errorf("openrouter got %v", sent)
	}
}

func TestTranscriptionStepfunErrorFailsOver(t *testing.T) {
	r := audioProviders{
		stepfun: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"error\",\"message\":\"internal error\"}\n\n")
		},
		google: func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`)
		},
	}.config(t, "", `
[model."listen"]
targets = ["stepfun/asr", "google/g-flash"]
`)
	res := r.ExecuteTranscription(context.Background(), "listen", provider.TranscriptionRequest{Audio: []byte("RIFF"), Format: "wav"})
	if !res.Success || string(res.Body) != `{"text":"hello"}` {
		t.Fatalf("result = %+v (%s)", res, res.Body)
	}
	if r.blackout.Blocked("ks", "asr") {
		t.Error("an error event that says nothing about the key must not black it out")
	}
}

func TestEmptyTranscriptIsSuccess(t *testing.T) {
	r := audioProviders{openrouter: func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"text":""}`) }}.config(t, "", `
[model."listen"]
targets = ["openrouter/whisper", "google/g-flash"]
`)
	res := r.ExecuteTranscription(context.Background(), "listen", provider.TranscriptionRequest{Audio: []byte("RIFF"), Format: "wav"})
	if !res.Success || len(res.Attempts) != 1 || string(res.Body) != `{"text":""}` {
		t.Fatalf("result = %+v", res)
	}
}

func TestAudioTimeoutFailsOver(t *testing.T) {
	r := audioProviders{
		openrouter: func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		},
		google: geminiWav,
	}.config(t, `audio_timeout = "100ms"`, `
[model."shiori"]
targets = ["openrouter/tts", "google/g-tts"]
`)
	res := r.ExecuteSpeech(context.Background(), "shiori", provider.SpeechRequest{Input: "Hi"})
	if !res.Success || len(res.Attempts) != 2 || res.Attempts[0].Outcome != "provider_error" {
		t.Fatalf("result = %+v", res)
	}
}

func TestAudioOnAnthropicOnly(t *testing.T) {
	r := audioProviders{}.config(t, "", `
[model."shiori"]
targets = ["claude/claude-x"]
`)
	if res := r.ExecuteSpeech(context.Background(), "shiori", provider.SpeechRequest{Input: "Hi"}); res.Status != http.StatusBadRequest || res.Success {
		t.Fatalf("result = %+v", res)
	}
}
