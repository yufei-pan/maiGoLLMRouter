package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// geminiConsumed lists OpenAI fields handled explicitly; everything else is
// merged into generationConfig as passthrough (#13).
var geminiConsumed = map[string]bool{
	"model": true, "messages": true, "input": true, "stream": true,
	"temperature": true, "top_p": true, "max_tokens": true,
	"max_completion_tokens": true, "stop": true, "n": true,
	"encoding_format": true, "dimensions": true, "user": true,
}

func callGemini(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request) (*Response, error) {
	if req.Op == OpEmbed {
		return geminiEmbed(ctx, client, baseURL, apiKey, req)
	}
	return geminiChat(ctx, client, baseURL, apiKey, req)
}

func geminiModelPath(model string) string {
	return "models/" + strings.TrimPrefix(model, "models/")
}

func geminiChat(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request) (*Response, error) {
	body := buildGeminiChatBody(req)
	url := baseURL + "/" + geminiModelPath(req.Model) + ":generateContent"
	out := mustJSON(body)
	headers := map[string]string{"x-goog-api-key": apiKey}

	status, raw, err := doJSON(ctx, client, http.MethodPost, url, headers, out)
	resp := &Response{HTTPStatus: status, OutboundURL: url, OutboundBody: out, RawResponse: raw}
	if err != nil {
		resp.OpenAIBody = raw
		return resp, err
	}
	if !resp.OK() {
		resp.OpenAIBody = raw
		return resp, nil
	}
	openAIBody, finish, hasContent := geminiToOpenAIChat(raw, req.Model)
	resp.OpenAIBody = openAIBody
	resp.FinishReason = finish
	resp.HasContent = hasContent
	return resp, nil
}

func buildGeminiChatBody(req Request) map[string]any {
	var contents []map[string]any
	var systemParts []map[string]any

	if msgs, ok := req.Body["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role := asString(mm, "role")
			text := contentToText(mm["content"])
			switch role {
			case "system", "developer":
				systemParts = append(systemParts, map[string]any{"text": text})
			case "assistant":
				contents = append(contents, map[string]any{"role": "model", "parts": []map[string]any{{"text": text}}})
			default: // user, tool, function
				contents = append(contents, map[string]any{"role": "user", "parts": []map[string]any{{"text": text}}})
			}
		}
	}

	genCfg := map[string]any{}
	if v, ok := req.Body["temperature"]; ok {
		genCfg["temperature"] = v
	}
	if v, ok := req.Body["top_p"]; ok {
		genCfg["topP"] = v
	}
	if v, ok := req.Body["max_tokens"]; ok {
		genCfg["maxOutputTokens"] = v
	}
	if v, ok := req.Body["max_completion_tokens"]; ok {
		genCfg["maxOutputTokens"] = v
	}
	if v, ok := req.Body["n"]; ok {
		genCfg["candidateCount"] = v
	}
	if v, ok := req.Body["stop"]; ok {
		switch s := v.(type) {
		case string:
			genCfg["stopSequences"] = []string{s}
		case []any:
			genCfg["stopSequences"] = s
		}
	}
	// Passthrough of unknown args into generationConfig.
	for k, v := range extras(req.Body, geminiConsumed) {
		genCfg[k] = v
	}

	body := map[string]any{"contents": contents}
	if len(systemParts) > 0 {
		body["systemInstruction"] = map[string]any{"parts": systemParts}
	}
	if len(genCfg) > 0 {
		body["generationConfig"] = genCfg
	}
	return body
}

func geminiToOpenAIChat(raw []byte, model string) (openAIBody []byte, finish string, hasContent bool) {
	var g struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return raw, "", false
	}

	choices := make([]map[string]any, 0, len(g.Candidates))
	for i, c := range g.Candidates {
		var text strings.Builder
		for _, p := range c.Content.Parts {
			text.WriteString(p.Text)
		}
		fr := normalizeGeminiFinish(c.FinishReason)
		if i == 0 {
			finish = fr
			hasContent = text.Len() > 0
		}
		choices = append(choices, map[string]any{
			"index":         i,
			"message":       map[string]any{"role": "assistant", "content": text.String()},
			"finish_reason": fr,
		})
	}

	result := map[string]any{
		"id":      "chatcmpl-" + randomID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": choices,
		"usage": map[string]any{
			"prompt_tokens":     g.UsageMetadata.PromptTokenCount,
			"completion_tokens": g.UsageMetadata.CandidatesTokenCount,
			"total_tokens":      g.UsageMetadata.TotalTokenCount,
		},
	}
	return mustJSON(result), finish, hasContent
}

func normalizeGeminiFinish(r string) string {
	switch strings.ToUpper(r) {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	case "":
		return ""
	default:
		return strings.ToLower(r)
	}
}

func geminiEmbed(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request) (*Response, error) {
	inputs := embedInputs(req.Body["input"])
	modelPath := geminiModelPath(req.Model)
	url := baseURL + "/" + modelPath + ":batchEmbedContents"

	reqs := make([]map[string]any, 0, len(inputs))
	for _, in := range inputs {
		reqs = append(reqs, map[string]any{
			"model":   modelPath,
			"content": map[string]any{"parts": []map[string]any{{"text": in}}},
		})
	}
	out := mustJSON(map[string]any{"requests": reqs})
	headers := map[string]string{"x-goog-api-key": apiKey}

	status, raw, err := doJSON(ctx, client, http.MethodPost, url, headers, out)
	resp := &Response{HTTPStatus: status, OutboundURL: url, OutboundBody: out, RawResponse: raw, OpenAIBody: raw}
	if err != nil {
		return resp, err
	}
	if !resp.OK() {
		return resp, nil
	}

	var g struct {
		Embeddings []struct {
			Values []float64 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return resp, nil
	}
	data := make([]map[string]any, 0, len(g.Embeddings))
	for i, e := range g.Embeddings {
		data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": e.Values})
	}
	result := map[string]any{
		"object": "list",
		"data":   data,
		"model":  req.Model,
		"usage":  map[string]any{"prompt_tokens": 0, "total_tokens": 0},
	}
	resp.OpenAIBody = mustJSON(result)
	resp.HasContent = len(data) > 0
	return resp, nil
}

// embedInputs normalizes the OpenAI "input" field (string or []string/[]any)
// into a slice of strings.
func embedInputs(v any) []string {
	switch in := v.(type) {
	case string:
		return []string{in}
	case []any:
		out := make([]string, 0, len(in))
		for _, e := range in {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
