package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"

	"maiGoLLMRouter/audioconv"
)

// TranscriptionRequest is an inbound /v1/audio/transcriptions request.
type TranscriptionRequest struct {
	Audio          []byte
	Format         string // wav, mp3, ogg, webm, m4a, flac, aac, ...
	Filename       string
	Language       string
	Prompt         string
	ResponseFormat string // "json" (default), "text", "verbose_json"
	Temperature    any
}

// geminiTranscribeInstruction is what Gemini, which has no transcription
// endpoint, is told to do with the audio.
const geminiTranscribeInstruction = "Transcribe the speech in this audio exactly, in the language spoken. " +
	"Output only the transcript, with no notes, labels or timestamps. If there is no speech, output nothing."

// stepfunFormats are the containers stepfun's transcription reads as they are.
var stepfunFormats = []string{"wav", "mp3", "ogg", "m4a"}

// callTranscription turns speech into text on one target. A successful reply
// is OpenAI's {"text": …} (or, for verbose_json on an OpenAI-kind provider,
// the provider's own body); HasContent means a text came back, even empty.
func callTranscription(ctx context.Context, client *http.Client, kind, baseURL, apiKey string, req Request) (*Response, error) {
	switch {
	case kind == "openai" && req.AudioAPI == "stepfun":
		return stepfunTranscription(ctx, client, baseURL, apiKey, req)
	case kind == "openai":
		return openAITranscription(ctx, client, baseURL, apiKey, req)
	case kind == "gemini":
		return geminiTranscription(ctx, client, baseURL, apiKey, req)
	default:
		return &Response{Incompatible: true}, nil
	}
}

func openAITranscription(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request) (*Response, error) {
	tr := req.Transcription
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	name := tr.Filename
	if name == "" {
		name = "speech." + tr.Format
	}
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		return nil, err
	}
	_, _ = fw.Write(tr.Audio)
	format := "json"
	if tr.ResponseFormat == "verbose_json" {
		format = "verbose_json"
	}
	fields := [][2]string{{"model", req.Model}, {"response_format", format}, {"language", tr.Language}, {"prompt", tr.Prompt}}
	if tr.Temperature != nil {
		fields = append(fields, [2]string{"temperature", fmt.Sprint(tr.Temperature)})
	}
	for _, f := range fields {
		if f[1] != "" {
			_ = mw.WriteField(f[0], f[1])
		}
	}
	_ = mw.Close()

	url := baseURL + "/audio/transcriptions"
	status, _, raw, err := doRaw(ctx, client, url, map[string]string{"Authorization": "Bearer " + apiKey}, mw.FormDataContentType(), body.Bytes())
	resp := &Response{HTTPStatus: status, OutboundURL: url, OpenAIBody: raw, RawResponse: raw}
	if err != nil || !resp.OK() {
		return resp, err
	}
	var parsed struct {
		Text *string `json:"text"`
	}
	if json.Unmarshal(raw, &parsed) != nil || parsed.Text == nil {
		return resp, nil
	}
	resp.HasContent = true
	if format != "verbose_json" {
		resp.OpenAIBody = mustJSON(map[string]any{"text": *parsed.Text})
	}
	return resp, nil
}

func stepfunTranscription(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request) (*Response, error) {
	tr := req.Transcription
	audio, format := tr.Audio, strings.ToLower(tr.Format)
	if !knownAudioFormat(format) {
		// Only plain, known names reach ffmpeg: a client's format string must
		// not carry parameters (such as a PCM rate) into the conversion.
		body := upstreamErrorBody("unknown audio format")
		return &Response{HTTPStatus: http.StatusBadRequest, OpenAIBody: body, RawResponse: body}, nil
	}
	if !slices.Contains(stepfunFormats, format) {
		converted, _, err := audioconv.Convert(ctx, req.FFmpeg, audio, "audio/"+format, "opus")
		if err != nil {
			// The audio can't reach stepfun in a form it reads: a fault of the
			// request, not of the provider or key.
			body := upstreamErrorBody("this provider can't read " + format + " audio")
			return &Response{HTTPStatus: http.StatusBadRequest, OpenAIBody: body, RawResponse: body}, nil
		}
		audio, format = converted, "ogg"
	}
	transcription := map[string]any{"model": req.Model, "enable_itn": true}
	if tr.Language != "" {
		transcription["language"] = tr.Language
	}
	out := mustJSON(map[string]any{"audio": map[string]any{
		"data":  base64.StdEncoding.EncodeToString(audio),
		"input": map[string]any{"transcription": transcription, "format": map[string]any{"type": format}},
	}})
	url := baseURL + "/audio/asr/sse"
	headers := map[string]string{"Authorization": "Bearer " + apiKey, "Accept": "text/event-stream"}
	status, _, raw, err := doRaw(ctx, client, url, headers, "application/json", out)
	resp := &Response{HTTPStatus: status, OutboundURL: url, OpenAIBody: raw, RawResponse: raw}
	if err != nil || !resp.OK() {
		return resp, err
	}
	sse := newSSEReader(bytes.NewReader(raw))
	for {
		ev, err := sse.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return resp, err
		}
		var m struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Message string `json:"message"`
			Code    any    `json:"code"`
		}
		if json.Unmarshal([]byte(ev.Data), &m) != nil {
			continue
		}
		switch m.Type {
		case "transcript.text.done":
			resp.OpenAIBody, resp.HasContent = mustJSON(map[string]any{"text": m.Text}), true
			return resp, nil
		case "error":
			// A stream that answered 200 still failed. It blacks out the key
			// only when its structured code is about the key (auth, quota,
			// rate): message text may repeat request data, so it can only
			// spare a key, never condemn one. Its status is 400 when the
			// request itself was at fault.
			code := ""
			if m.Code != nil {
				code = fmt.Sprint(m.Code)
			}
			switch {
			case stepfunKeyFault(code):
				resp.HTTPStatus = http.StatusBadGateway
			case stepfunRequestFault(code, m.Message):
				resp.HTTPStatus, resp.NoBlackout = http.StatusBadRequest, true
			default:
				resp.HTTPStatus, resp.NoBlackout = http.StatusBadGateway, true
			}
			resp.OpenAIBody = mustJSON(map[string]any{"error": map[string]any{"message": m.Message, "code": m.Code}})
			return resp, nil
		}
	}
	resp.HTTPStatus = http.StatusBadGateway
	resp.OpenAIBody = upstreamErrorBody("the provider's transcription ended without a result")
	return resp, nil
}

// stepfunKeyFault reports an error code about the key itself (its auth,
// quota or rate limit), the only kind that says the key should rest. It reads
// the structured code only, never message text.
func stepfunKeyFault(code string) bool {
	c := strings.ToLower(code)
	for _, w := range []string{"auth", "api_key", "apikey", "permission", "forbidden", "quota", "balance", "insufficient", "rate_limit", "401", "402", "403", "429"} {
		if strings.Contains(c, w) {
			return true
		}
	}
	return false
}

// knownAudioFormat reports a plain audio format name the router knows.
func knownAudioFormat(format string) bool {
	_, ok := geminiAudioMIMEs[format]
	return ok || format == "pcm"
}

// stepfunRequestFault reports an error event caused by the request itself (the
// audio), which another key or provider would refuse too.
func stepfunRequestFault(code, message string) bool {
	s := strings.ToLower(code + " " + message)
	for _, w := range []string{"invalid", "empty", "too short", "unsupported", "format", "decode", "duration"} {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func geminiTranscription(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request) (*Response, error) {
	tr := req.Transcription
	instruction := geminiTranscribeInstruction
	if tr.Language != "" {
		instruction += " The language is probably " + tr.Language + "."
	}
	if tr.Prompt != "" {
		instruction += " Context: " + tr.Prompt
	}
	temperature := tr.Temperature
	if temperature == nil {
		temperature = 0
	}
	genCfg := map[string]any{"temperature": temperature, "thinkingConfig": map[string]any{"thinkingLevel": "minimal"}}
	body := map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{
			map[string]any{"text": instruction},
			map[string]any{"inlineData": map[string]any{"mimeType": geminiAudioMIME(tr.Format), "data": base64.StdEncoding.EncodeToString(tr.Audio)}},
		}}},
		"generationConfig": genCfg,
	}
	url := baseURL + "/" + geminiModelPath(req.Model) + ":generateContent"
	headers := map[string]string{"x-goog-api-key": apiKey}
	status, raw, err := doJSON(ctx, client, http.MethodPost, url, headers, mustJSON(body))
	if err == nil && status == http.StatusBadRequest && strings.Contains(strings.ToLower(string(raw)), "thinking") {
		// Least thinking keeps transcription fast, but not every model takes it.
		delete(genCfg, "thinkingConfig")
		status, raw, err = doJSON(ctx, client, http.MethodPost, url, headers, mustJSON(body))
	}
	resp := &Response{HTTPStatus: status, OutboundURL: url, OpenAIBody: raw, RawResponse: raw}
	if err != nil || !resp.OK() {
		return resp, err
	}
	var g struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text    string `json:"text"`
					Thought bool   `json:"thought"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(raw, &g) != nil || len(g.Candidates) == 0 {
		return resp, nil
	}
	var text strings.Builder
	for _, p := range g.Candidates[0].Content.Parts {
		if !p.Thought {
			text.WriteString(p.Text)
		}
	}
	resp.OpenAIBody, resp.HasContent = mustJSON(map[string]any{"text": strings.TrimSpace(text.String())}), true
	return resp, nil
}
