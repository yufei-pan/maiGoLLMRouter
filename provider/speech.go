package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
)

// SpeechRequest is an inbound /v1/audio/speech request, made ready for one
// target by the router.
type SpeechRequest struct {
	Input        string
	Voice        string         // already chosen for this target
	Format       string         // what to ask an OpenAI-kind provider for
	Instructions string         // delivery guidance (tone, pace)
	Speed        any            // nil when not given
	Extra        map[string]any // other client fields, passed through to OpenAI-kind providers
}

// geminiVoices are Gemini's prebuilt voices; any other name becomes Kore.
var geminiVoices = []string{
	"Zephyr", "Puck", "Charon", "Kore", "Fenrir", "Leda", "Orus", "Aoede", "Callirrhoe", "Autonoe",
	"Enceladus", "Iapetus", "Umbriel", "Algieba", "Despina", "Erinome", "Algenib", "Rasalgethi", "Laomedeia", "Achernar",
	"Alnilam", "Schedar", "Gacrux", "Pulcherrima", "Achird", "Zubenelgenubi", "Vindemiatrix", "Sadachbia", "Sadaltager", "Sulafat",
}

const defaultGeminiVoice = "Kore"

// callSpeech turns text into speech on one target. The audio comes back in
// OpenAIBody with its type in ContentType; it is never kept in RawResponse.
func callSpeech(ctx context.Context, client *http.Client, kind, baseURL, apiKey string, req Request) (*Response, error) {
	switch kind {
	case "openai":
		return openAISpeech(ctx, client, baseURL, apiKey, req)
	case "gemini":
		return geminiSpeech(ctx, client, baseURL, apiKey, req)
	default:
		return &Response{Incompatible: true}, nil
	}
}

func openAISpeech(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request) (*Response, error) {
	sp := req.Speech
	body := make(map[string]any, len(sp.Extra)+6)
	for k, v := range sp.Extra {
		body[k] = v
	}
	body["model"] = req.Model
	body["input"] = sp.Input
	body["response_format"] = sp.Format
	if sp.Voice != "" {
		body["voice"] = sp.Voice
	}
	if sp.Speed != nil {
		body["speed"] = sp.Speed
	}
	if sp.Instructions != "" {
		if req.AudioAPI == "stepfun" {
			body["instruction"] = sp.Instructions // stepfun's name for it
		} else {
			body["instructions"] = sp.Instructions
		}
	}
	if req.AudioAPI == "stepfun" && sp.Format == "pcm" {
		body["sample_rate"] = 24000 // stepfun's default is 24 kHz too; pcm carries no header to say so
	}
	out := mustJSON(body)
	url := baseURL + "/audio/speech"
	status, header, raw, err := doRaw(ctx, client, url, map[string]string{"Authorization": "Bearer " + apiKey}, "application/json", out)
	resp := &Response{HTTPStatus: status, OutboundURL: url, OutboundBody: out}
	if err != nil {
		return resp, err
	}
	ct := header.Get("Content-Type")
	if resp.OK() && strings.HasPrefix(strings.ToLower(ct), "audio/") {
		resp.OpenAIBody, resp.ContentType, resp.HasContent = raw, ct, len(raw) > 0
		return resp, nil
	}
	// An error, or a 2xx that isn't audio: keep the body to log and forward.
	resp.OpenAIBody, resp.RawResponse = raw, raw
	return resp, nil
}

func geminiSpeech(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request) (*Response, error) {
	sp := req.Speech
	voice := sp.Voice
	if !slices.Contains(geminiVoices, voice) {
		voice = defaultGeminiVoice
	}
	body := map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": sp.Input}}}},
		"generationConfig": map[string]any{
			"responseModalities": []any{"AUDIO"},
			"speechConfig": map[string]any{
				"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]any{"voiceName": voice}},
			},
		},
	}
	if sp.Instructions != "" {
		body["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": sp.Instructions}}}
	}
	out := mustJSON(body)
	url := baseURL + "/" + geminiModelPath(req.Model) + ":generateContent"
	status, raw, err := doJSON(ctx, client, http.MethodPost, url, map[string]string{"x-goog-api-key": apiKey}, out)
	resp := &Response{HTTPStatus: status, OutboundURL: url, OutboundBody: out}
	if err != nil || !resp.OK() {
		resp.OpenAIBody, resp.RawResponse = raw, raw
		return resp, err
	}
	var g struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	_ = json.Unmarshal(raw, &g)
	for _, c := range g.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData == nil {
				continue
			}
			audio, derr := base64.StdEncoding.DecodeString(p.InlineData.Data)
			if derr == nil && len(audio) > 0 {
				resp.OpenAIBody, resp.ContentType, resp.HasContent = audio, p.InlineData.MimeType, true
				return resp, nil
			}
		}
	}
	// No audio (a text reply, or a block): keep the body to log and judge.
	resp.OpenAIBody, resp.RawResponse = raw, raw
	return resp, nil
}

// doRaw sends one POST and returns the status, headers and body.
func doRaw(ctx context.Context, client *http.Client, url string, headers map[string]string, contentType string, body []byte) (int, http.Header, []byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	httpReq.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw, err
}
