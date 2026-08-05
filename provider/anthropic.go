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
	"tools": true, "tool_choice": true, "functions": true, "function_call": true,
	"response_format": true, // consumed (dropped): Anthropic uses a different API
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
			switch role {
			case "system", "developer":
				text := contentToText(mm["content"])
				if system.Len() > 0 {
					system.WriteString("\n\n")
				}
				system.WriteString(text)
			case "assistant":
				messages = append(messages, map[string]any{
					"role":    "assistant",
					"content": anthropicAssistantContent(mm),
				})
			case "tool", "function":
				messages = append(messages, map[string]any{
					"role":    "user",
					"content": anthropicToolResultContent(mm),
				})
			default: // user
				messages = append(messages, map[string]any{
					"role":    "user",
					"content": anthropicUserContent(mm["content"]),
				})
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
	if tools := anthropicTools(req.Body["tools"], req.Body["functions"]); len(tools) > 0 {
		body["tools"] = tools
	}
	if tc := anthropicToolChoice(req.Body["tool_choice"]); tc != nil {
		body["tool_choice"] = tc
	}
	// Passthrough of unknown args at the top level (tools/response_format consumed).
	for k, v := range extras(req.Body, anthropicConsumed) {
		body[k] = v
	}
	return body
}

func anthropicUserContent(content any) any {
	parts := openAIContentParts(content)
	if len(parts) == 0 {
		return contentToText(content)
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch asString(p, "type") {
		case "text", "":
			if t := asString(p, "text"); t != "" {
				out = append(out, map[string]any{"type": "text", "text": t})
			}
		case "image_url":
			if block := anthropicImageBlock(p["image_url"]); block != nil {
				out = append(out, block)
			}
		}
	}
	if len(out) == 0 {
		return contentToText(content)
	}
	if len(out) == 1 && out[0]["type"] == "text" {
		return out[0]["text"]
	}
	return out
}

func anthropicImageBlock(imageURL any) map[string]any {
	url, mediaType, data, ok := parseImageURL(imageURL)
	if !ok {
		return nil
	}
	if data != "" {
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": mediaType,
				"data":       data,
			},
		}
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url",
			"url":  url,
		},
	}
}

func anthropicAssistantContent(mm map[string]any) any {
	blocks := make([]map[string]any, 0)
	text := contentToText(mm["content"])
	if text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	if tcs, ok := mm["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tcm["function"].(map[string]any)
			if fn == nil {
				continue
			}
			input := jsonObjectOrWrap(asString(fn, "arguments"))
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    asString(tcm, "id"),
				"name":  asString(fn, "name"),
				"input": input,
			})
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	if len(blocks) == 1 && blocks[0]["type"] == "text" {
		return blocks[0]["text"]
	}
	return blocks
}

func anthropicToolResultContent(mm map[string]any) []map[string]any {
	return []map[string]any{{
		"type":        "tool_result",
		"tool_use_id": asString(mm, "tool_call_id"),
		"content":     contentToText(mm["content"]),
	}}
}

func anthropicTools(tools, functions any) []any {
	var out []any
	appendFn := func(fn map[string]any) {
		name := asString(fn, "name")
		if name == "" {
			return
		}
		entry := map[string]any{"name": name}
		if d := asString(fn, "description"); d != "" {
			entry["description"] = d
		}
		schema, _ := fn["parameters"].(map[string]any)
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		entry["input_schema"] = schema
		out = append(out, entry)
	}
	if arr, ok := tools.([]any); ok {
		for _, t := range arr {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := tm["function"].(map[string]any); ok {
				appendFn(fn)
			} else if asString(tm, "type") == "function" || asString(tm, "name") != "" {
				appendFn(tm)
			}
		}
	}
	if arr, ok := functions.([]any); ok {
		for _, f := range arr {
			if fm, ok := f.(map[string]any); ok {
				appendFn(fm)
			}
		}
	}
	return out
}

func anthropicToolChoice(v any) any {
	switch t := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "auto":
			return map[string]any{"type": "auto"}
		case "none":
			return map[string]any{"type": "none"}
		case "required":
			return map[string]any{"type": "any"}
		}
	case map[string]any:
		if fn, ok := t["function"].(map[string]any); ok {
			if name := asString(fn, "name"); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
		if asString(t, "type") == "function" {
			if name := asString(t, "name"); name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

func anthropicToOpenAIChat(raw []byte, model string) (openAIBody []byte, finish string, hasContent bool) {
	var a struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
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
	var toolCalls []map[string]any
	for _, c := range a.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "tool_use":
			args := "{}"
			if len(c.Input) > 0 {
				args = string(c.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": args,
				},
			})
		}
	}
	finish = normalizeAnthropicFinish(a.StopReason)
	hasContent = text.Len() > 0 || len(toolCalls) > 0

	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		if text.Len() == 0 {
			msg["content"] = nil
		}
	}

	result := map[string]any{
		"id":      "chatcmpl-" + randomID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
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
