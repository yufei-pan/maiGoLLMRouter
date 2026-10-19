package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// speechServer records the outbound path and JSON body, then answers with reply.
func speechServer(t *testing.T, path *string, body *map[string]any, reply func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if path != nil {
			*path = r.URL.Path
		}
		if body != nil {
			_ = json.NewDecoder(r.Body).Decode(body)
		} else {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		reply(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func speechReq(model, audioAPI string, sp SpeechRequest) Request {
	return Request{Op: OpSpeech, Model: model, AudioAPI: audioAPI, Speech: &sp}
}

func TestOpenAISpeechPassThrough(t *testing.T) {
	var path string
	var sent map[string]any
	srv := speechServer(t, &path, &sent, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write([]byte{1, 2, 3, 4})
	})
	req := speechReq("real", "openai", SpeechRequest{Input: "Hello", Voice: "Kore", Format: "pcm", Instructions: "warm", Extra: map[string]any{"foo": float64(1)}})
	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", req)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/audio/speech" || sent["model"] != "real" || sent["input"] != "Hello" || sent["voice"] != "Kore" ||
		sent["response_format"] != "pcm" || sent["instructions"] != "warm" || sent["foo"] != float64(1) {
		t.Errorf("path %s, body %v", path, sent)
	}
	if string(resp.OpenAIBody) != "\x01\x02\x03\x04" || resp.ContentType != "audio/pcm" || !resp.HasContent || resp.RawResponse != nil {
		t.Errorf("response = %+v", resp)
	}
}

func TestStepfunSpeechFields(t *testing.T) {
	var sent map[string]any
	srv := speechServer(t, nil, &sent, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "audio/pcm")
		_, _ = w.Write([]byte{1, 2})
	})
	req := speechReq("stepaudio-2.5-tts", "stepfun", SpeechRequest{Input: "Hello", Voice: "zixinnansheng", Format: "pcm", Instructions: "warm"})
	if _, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", req); err != nil {
		t.Fatal(err)
	}
	if sent["instruction"] != "warm" || sent["sample_rate"] != float64(24000) {
		t.Errorf("body %v, want instruction and sample_rate 24000", sent)
	}
	if _, has := sent["instructions"]; has {
		t.Error("stepfun must not get instructions (plural)")
	}
}

func TestGeminiSpeech(t *testing.T) {
	wav := "RIFF\x00\x00\x00\x00WAVEfmt "
	var path string
	var sent map[string]any
	srv := speechServer(t, &path, &sent, func(w http.ResponseWriter) {
		fmt.Fprintf(w, `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/wav","data":%q}}]},"finishReason":"STOP"}]}`,
			base64.StdEncoding.EncodeToString([]byte(wav)))
	})
	req := speechReq("gemini-3.8-flash-lite-tts", "", SpeechRequest{Input: "Hello", Voice: "Puck"})
	resp, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/models/gemini-3.8-flash-lite-tts:generateContent") {
		t.Errorf("path = %s", path)
	}
	gc, _ := sent["generationConfig"].(map[string]any)
	mods, _ := gc["responseModalities"].([]any)
	if len(mods) != 1 || mods[0] != "AUDIO" || voiceName(sent) != "Puck" {
		t.Errorf("generationConfig = %v", gc)
	}
	if string(resp.OpenAIBody) != wav || resp.ContentType != "audio/wav" || !resp.HasContent {
		t.Errorf("response = %+v", resp)
	}
}

func voiceName(body map[string]any) any {
	gc, _ := body["generationConfig"].(map[string]any)
	sc, _ := gc["speechConfig"].(map[string]any)
	vc, _ := sc["voiceConfig"].(map[string]any)
	pv, _ := vc["prebuiltVoiceConfig"].(map[string]any)
	return pv["voiceName"]
}

func TestGeminiSpeechUnknownVoice(t *testing.T) {
	var sent map[string]any
	srv := speechServer(t, nil, &sent, func(w http.ResponseWriter) {
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}}]}}]}`)
	})
	if _, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", speechReq("g-tts", "", SpeechRequest{Input: "Hi", Voice: "marin"})); err != nil {
		t.Fatal(err)
	}
	if voiceName(sent) != "Kore" {
		t.Errorf("voice = %v, want Kore", voiceName(sent))
	}
}

func TestGeminiSpeechWithoutAudioIsNotContent(t *testing.T) {
	srv := speechServer(t, nil, nil, func(w http.ResponseWriter) {
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"I can't"}]},"finishReason":"STOP"}]}`)
	})
	resp, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", speechReq("g-tts", "", SpeechRequest{Input: "Hi"}))
	if err != nil || !resp.OK() || resp.HasContent {
		t.Errorf("text reply: err %v, response %+v", err, resp)
	}
	blocked := speechServer(t, nil, nil, func(w http.ResponseWriter) {
		fmt.Fprint(w, `{"candidates":[{"finishReason":"SAFETY"}]}`)
	})
	resp, _ = Call(context.Background(), blocked.Client(), "gemini", blocked.URL, "k", speechReq("g-tts", "", SpeechRequest{Input: "Hi"}))
	if !resp.Prohibited || resp.HasContent {
		t.Errorf("safety block: response %+v", resp)
	}
}

func TestAnthropicSpeechIsIncompatible(t *testing.T) {
	srv := speechServer(t, nil, nil, func(w http.ResponseWriter) { t.Error("anthropic must not be called for speech") })
	resp, err := Call(context.Background(), srv.Client(), "anthropic", srv.URL, "k", speechReq("claude", "", SpeechRequest{Input: "Hi"}))
	if err != nil || resp == nil || !resp.Incompatible {
		t.Errorf("err %v, response %+v", err, resp)
	}
}

// TestGeminiSpeechDropsInstructions verifies Gemini's TTS models never get
// instructions: they refuse systemInstruction ("Developer instruction is not
// enabled for this model", checked live on 2026-10-19), and putting style in
// the text would have it read aloud.
func TestGeminiSpeechDropsInstructions(t *testing.T) {
	var sent map[string]any
	srv := speechServer(t, nil, &sent, func(w http.ResponseWriter) {
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/wav","data":"UklGRg=="}}]}}]}`)
	})
	if _, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", speechReq("g-tts", "", SpeechRequest{Input: "Hi", Instructions: "Speak slowly"})); err != nil {
		t.Fatal(err)
	}
	if _, has := sent["systemInstruction"]; has {
		t.Errorf("Gemini got systemInstruction: %v", sent["systemInstruction"])
	}
	if b, _ := json.Marshal(sent); strings.Contains(string(b), "Speak slowly") {
		t.Errorf("the instructions reached Gemini: %s", b)
	}
}
