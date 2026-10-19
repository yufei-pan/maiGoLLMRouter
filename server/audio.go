package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"maiGoLLMRouter/audioconv"
	"maiGoLLMRouter/config"
	"maiGoLLMRouter/inflight"
	"maiGoLLMRouter/logstore"
	"maiGoLLMRouter/provider"
	"maiGoLLMRouter/router"
)

const maxAudioRequest = 32 << 20

// speechConsumed are the speech request fields the router reads itself; any
// other field passes through to OpenAI-kind providers.
var speechConsumed = map[string]bool{
	"model": true, "input": true, "voice": true, "response_format": true,
	"instructions": true, "speed": true, "stream_format": true,
}

// handleSpeech implements POST /v1/audio/speech: text in, audio out in the
// format the client asks for, converted with ffmpeg when the provider's
// differs.
func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	clientKey, ok := s.audioPreamble(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAudioRequest))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	model, _ := body["model"].(string)
	input, _ := body["input"].(string)
	if strings.TrimSpace(model) == "" || strings.TrimSpace(input) == "" {
		writeError(w, http.StatusBadRequest, "model and input are required", "invalid_request_error")
		return
	}
	format := "mp3"
	if v, given := body["response_format"]; given {
		f, _ := v.(string)
		if !slices.Contains(config.SpeechFormats, f) {
			writeError(w, http.StatusBadRequest, "response_format must be one of "+strings.Join(config.SpeechFormats, ", "), "invalid_request_error")
			return
		}
		format = f
	}
	sp := provider.SpeechRequest{Input: input, Extra: map[string]any{}}
	sp.Voice, _ = body["voice"].(string)
	sp.Instructions, _ = body["instructions"].(string)
	if v, given := body["speed"]; given && v != nil {
		sp.Speed = v
	}
	for k, v := range body {
		if !speechConsumed[k] {
			sp.Extra[k] = v
		}
	}

	const endpoint = "/v1/audio/speech"
	ctx, reqID, start, done := s.beginAudio(r, endpoint, model, clientKey, preview(input))
	defer done()
	res := s.rt.ExecuteSpeech(ctx, model, sp)
	if !res.Success {
		writeJSON(w, res.Status, res.Body)
		s.log(reqID, endpoint, clientKey, model, time.Since(start).Milliseconds(), raw, res, false)
		return
	}
	s.mu.RLock()
	ffmpeg := s.cfg.Server.FFmpeg
	s.mu.RUnlock()
	audio, ct := convertSpeech(r.Context(), ffmpeg, res.Body, res.ContentType, format)
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(audio)

	logged := *res
	logged.Body = mustJSON(map[string]any{"audio": describe(len(audio), ct)})
	s.log(reqID, endpoint, clientKey, model, time.Since(start).Milliseconds(), raw, &logged, false)
}

// convertSpeech puts audio into the client's format. When ffmpeg is missing or
// fails, the audio goes as the provider made it, under its true type, with raw
// PCM wrapped in a WAV header so it still plays.
func convertSpeech(ctx context.Context, ffmpeg string, data []byte, ct, format string) ([]byte, string) {
	if audioconv.Matches(ct, format) {
		return data, ct
	}
	out, oct, err := audioconv.Convert(ctx, ffmpeg, data, ct, format)
	if err == nil {
		return out, oct
	}
	log.Printf("WARNING: speech not converted to %s: %v", format, err)
	if audioconv.IsRawPCM(ct) {
		return audioconv.WrapPCM(data, audioconv.PCMRate(ct)), "audio/wav"
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	return data, ct
}

// handleTranscription implements POST /v1/audio/transcriptions, taking an
// OpenAI-style multipart upload or OpenRouter-style JSON with base64 audio.
func (s *Server) handleTranscription(w http.ResponseWriter, r *http.Request) {
	clientKey, ok := s.audioPreamble(w, r)
	if !ok {
		return
	}
	model, tr, msg := readTranscription(w, r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg, "invalid_request_error")
		return
	}
	logged := map[string]any{"model": model, "audio": describe(len(tr.Audio), "audio/"+tr.Format), "response_format": tr.ResponseFormat}
	for k, v := range map[string]string{"language": tr.Language, "prompt": tr.Prompt} {
		if v != "" {
			logged[k] = v
		}
	}
	reqLog := mustJSON(logged)

	const endpoint = "/v1/audio/transcriptions"
	ctx, reqID, start, done := s.beginAudio(r, endpoint, model, clientKey, fmt.Sprintf("audio %d KB %s", (len(tr.Audio)+1023)/1024, tr.Format))
	defer done()
	res := s.rt.ExecuteTranscription(ctx, model, tr)
	if !res.Success {
		writeJSON(w, res.Status, res.Body)
	} else if tr.ResponseFormat == "text" {
		var parsed struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(res.Body, &parsed)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, parsed.Text)
	} else {
		writeJSON(w, http.StatusOK, res.Body)
	}
	s.log(reqID, endpoint, clientKey, model, time.Since(start).Milliseconds(), reqLog, res, false)
}

// readTranscription reads either request shape. A non-empty message is the
// reason the request is refused.
func readTranscription(w http.ResponseWriter, r *http.Request) (string, provider.TranscriptionRequest, string) {
	var tr provider.TranscriptionRequest
	var model, temperature string
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "multipart/form-data" {
		r.Body = http.MaxBytesReader(w, r.Body, maxAudioRequest)
		if err := r.ParseMultipartForm(maxAudioRequest); err != nil {
			return "", tr, "invalid multipart body"
		}
		defer r.MultipartForm.RemoveAll()
		f, h, err := r.FormFile("file")
		if err != nil {
			return "", tr, "missing file"
		}
		defer f.Close()
		if tr.Audio, err = io.ReadAll(f); err != nil {
			return "", tr, "failed to read file"
		}
		model, tr.Filename = r.FormValue("model"), h.Filename
		tr.Language, tr.Prompt, tr.ResponseFormat = r.FormValue("language"), r.FormValue("prompt"), r.FormValue("response_format")
		temperature = r.FormValue("temperature")
		// The format: the file's extension, else the part's type, else WAV.
		tr.Format = "wav"
		for _, c := range []string{filepath.Ext(h.Filename), h.Header.Get("Content-Type")} {
			if f, ok := provider.NormalizeAudioFormat(c); ok && c != "" {
				tr.Format = f
				break
			}
		}
	} else {
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxAudioRequest))
		if err != nil {
			return "", tr, "failed to read request body"
		}
		var body struct {
			Model      string `json:"model"`
			InputAudio *struct {
				Data   string `json:"data"`
				Format string `json:"format"`
			} `json:"input_audio"`
			Language       string `json:"language"`
			Prompt         string `json:"prompt"`
			ResponseFormat string `json:"response_format"`
			Temperature    any    `json:"temperature"`
		}
		if json.Unmarshal(raw, &body) != nil {
			return "", tr, "invalid JSON body"
		}
		if body.InputAudio == nil || body.InputAudio.Data == "" {
			return "", tr, "missing input_audio"
		}
		mimeType, b64 := body.InputAudio.Data, ""
		if rest, ok := strings.CutPrefix(mimeType, "data:"); ok {
			meta, data, _ := strings.Cut(rest, ",")
			mimeType, b64 = meta, data
		} else {
			mimeType, b64 = "", body.InputAudio.Data
		}
		if tr.Audio, err = base64.StdEncoding.DecodeString(b64); err != nil {
			return "", tr, "input_audio.data is not base64"
		}
		tr.Format = "wav"
		if body.InputAudio.Format != "" {
			f, ok := provider.NormalizeAudioFormat(body.InputAudio.Format)
			if !ok {
				return "", tr, "unknown input_audio.format"
			}
			tr.Format = f
		} else if f, ok := provider.NormalizeAudioFormat(mimeType); ok && mimeType != "" {
			tr.Format = f
		}
		model, tr.Language, tr.Prompt, tr.ResponseFormat = body.Model, body.Language, body.Prompt, body.ResponseFormat
		if body.Temperature != nil {
			temperature = fmt.Sprint(body.Temperature)
		}
	}
	if strings.TrimSpace(model) == "" {
		return "", tr, "missing required field: model"
	}
	switch tr.ResponseFormat {
	case "":
		tr.ResponseFormat = "json"
	case "json", "text", "verbose_json":
	default:
		return "", tr, "response_format must be json, text or verbose_json"
	}
	if temperature != "" {
		t, err := strconv.ParseFloat(temperature, 64)
		if err != nil {
			return "", tr, "temperature must be a number"
		}
		tr.Temperature = t
	}
	return model, tr, ""
}

// audioPreamble checks the method and the client key.
func (s *Server) audioPreamble(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return "", false
	}
	clientKey, ok := s.authorize(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid or missing bearer token", "authentication_error")
		return "", false
	}
	return clientKey, true
}

// beginAudio registers an audio request in the in-flight view. done removes it.
func (s *Server) beginAudio(r *http.Request, endpoint, model, clientKey, previewText string) (context.Context, string, time.Time, func()) {
	start := time.Now()
	reqID := "req-" + randomID()
	ctx := r.Context()
	if s.inflight == nil {
		return ctx, reqID, start, func() {}
	}
	live := s.inflight.Register(inflight.Meta{
		ID:             reqID,
		StartedAt:      start.UTC().Format(time.RFC3339Nano),
		Endpoint:       endpoint,
		InboundModel:   model,
		ClientKey:      maskKey(clientKey),
		RequestPreview: previewText,
	})
	return router.WithAttemptObserver(ctx, live), reqID, start, func() { s.inflight.Unregister(reqID) }
}

// preview is the start of a text, as the in-flight view shows it.
func preview(text string) string {
	if utf8.RuneCountInString(text) <= logstore.DefaultRequestPreviewLen {
		return text
	}
	return string([]rune(text)[:logstore.DefaultRequestPreviewLen]) + "…"
}

// describe is how audio appears in logs: its size and type, never its bytes.
func describe(n int, contentType string) string {
	return fmt.Sprintf("<%d bytes %s>", n, contentType)
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
