package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const anthropicVersion = "2023-06-01"
const defaultAnthropicMaxTokens = 8192

// anthropicConsumed lists OpenAI fields handled explicitly; everything else is
// merged into the top-level request body as passthrough (#13).
var anthropicConsumed = map[string]bool{
	"model": true, "messages": true, "input": true, "stream": true,
	"max_tokens": true, "max_completion_tokens": true, "stop": true, "n": true,
}

func callAnthropic(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request) (*Response, error) {
	if req.Op == OpEmbed {
		return &Response{
			HTTPStatus:   http.StatusBadRequest,
			OpenAIBody:   []byte(`{"error":{"message":"embeddings are not supported by the anthropic provider","type":"invalid_request_error"}}`),
			OutboundURL:  baseURL + "/embeddings",
			OutboundBody: nil,
		}, fmt.Errorf("anthropic does not support embeddings")
	}

	body := buildAnthropicBody(req)
	url := baseURL + "/messages"
	out := mustJSON(body)
	headers := map[string]string{
		"x-api-key":         apiKey,
		"anthropic-version": anthropicVersion,
	}

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
	openAIBody, finish, hasContent := anthropicToOpenAIChat(raw, req.Model)
	resp.OpenAIBody = openAIBody
	resp.FinishReason = finish
	resp.HasContent = hasContent
	return resp, nil
}

func buildAnthropicBody(req Request) map[string]any {
	var system strings.Builder
	var messages []map[string]any

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
				if system.Len() > 0 {
					system.WriteString("\n\n")
				}
				system.WriteString(text)
			case "assistant":
				messages = append(messages, map[string]any{"role": "assistant", "content": text})
			default:
				messages = append(messages, map[string]any{"role": "user", "content": text})
			}
		}
	}

	body := map[string]any{
		"model":    req.Model,
		"messages": messages,
	}
	if system.Len() > 0 {
		body["system"] = system.String()
	}

	// max_tokens is required by the Anthropic API.
	if v, ok := req.Body["max_tokens"]; ok {
		body["max_tokens"] = v
	} else if v, ok := req.Body["max_completion_tokens"]; ok {
		body["max_tokens"] = v
	} else {
		body["max_tokens"] = defaultAnthropicMaxTokens
	}

	if v, ok := req.Body["stop"]; ok {
		switch s := v.(type) {
		case string:
			body["stop_sequences"] = []string{s}
		case []any:
			body["stop_sequences"] = s
		}
	}
	// Passthrough of unknown args at the top level.
	for k, v := range extras(req.Body, anthropicConsumed) {
		body[k] = v
	}
	return body
}

func anthropicToOpenAIChat(raw []byte, model string) (openAIBody []byte, finish string, hasContent bool) {
	var a struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return raw, "", false
	}

	var text strings.Builder
	for _, c := range a.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	finish = normalizeAnthropicFinish(a.StopReason)
	hasContent = text.Len() > 0

	result := map[string]any{
		"id":      "chatcmpl-" + randomID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text.String()},
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     a.Usage.InputTokens,
			"completion_tokens": a.Usage.OutputTokens,
			"total_tokens":      a.Usage.InputTokens + a.Usage.OutputTokens,
		},
	}
	return mustJSON(result), finish, hasContent
}

func normalizeAnthropicFinish(r string) string {
	switch r {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "":
		return ""
	default:
		return r
	}
}
