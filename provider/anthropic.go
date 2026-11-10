package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"
)

const anthropicVersion = "2023-06-01"
const defaultAnthropicMaxTokens = 8192

// anthropicConsumed lists OpenAI fields handled explicitly; everything else is
// merged into the top-level request body as passthrough (#13).
var anthropicConsumed = withOpenAIOnly(map[string]bool{
	"model": true, "messages": true, "input": true, "stream": true, "stream_options": true,
	"max_tokens": true, "max_completion_tokens": true, "stop": true, "n": true,
	"tools": true, "tool_choice": true, "functions": true, "function_call": true,
	"response_format":  true, // consumed (dropped): Anthropic uses a different API
	"reasoning_effort": true, // becomes output_config.effort (anthropicEffort)
})

// anthropicEffort reads OpenAI's reasoning_effort as Anthropic's
// output_config.effort. "none" and "minimal" become "low": Claude Opus 5.5 can't
// turn thinking off, and low is the least effort every current Claude model
// takes. Any other value goes on lowercased, for Anthropic to judge (it takes
// low, medium, high, xhigh and max). ok is false when there is nothing to send.
func anthropicEffort(v any) (effort string, ok bool) {
	s, _ := v.(string)
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "":
		return "", false
	case "none", "minimal":
		return "low", true
	}
	return s, true
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

	status, raw, err := doJSON(ctx, client, http.MethodPost, url, anthropicHeaders(apiKey), out)
	resp := &Response{HTTPStatus: status, OutboundURL: url, OutboundBody: out, RawResponse: raw}
	if err != nil {
		resp.OpenAIBody = raw
		return resp, err
	}
	if !resp.OK() {
		resp.OpenAIBody = raw
		return resp, nil
	}
	anthropicJSONReply(resp, req.Model)
	return resp, nil
}

func anthropicHeaders(apiKey string) map[string]string {
	return map[string]string{"x-api-key": apiKey, "anthropic-version": anthropicVersion}
}

// anthropicJSONReply judges a 2xx Messages reply that came back whole. A
// refusal is a policy block, as it is on a stream.
func anthropicJSONReply(resp *Response, model string) {
	resp.OpenAIBody, resp.FinishReason, resp.HasContent = anthropicToOpenAIChat(resp.RawResponse, model)
	if resp.FinishReason == "content_filter" {
		resp.Prohibited = true
	}
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
	// Automatic prompt caching, unless the client set its own cache_control.
	// Coding agents resend the whole conversation every turn: a cache read costs
	// a tenth of the input price or less, a write 1.25x. The 5-minute lifetime
	// is the user's choice (2026-11-10); a read restarts it.
	if _, set := body["cache_control"]; !set {
		body["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	// Anthropic rejects reasoning_effort as an unknown field; its own name for
	// it is output_config.effort. An effort the client set there itself wins.
	if effort, ok := anthropicEffort(req.Body["reasoning_effort"]); ok {
		switch given := body["output_config"].(type) {
		case nil:
			body["output_config"] = map[string]any{"effort": effort}
		case map[string]any:
			if _, set := given["effort"]; !set {
				// A copy: the caller's body goes to other targets as it is.
				merged := make(map[string]any, len(given)+1)
				maps.Copy(merged, given)
				merged["effort"] = effort
				body["output_config"] = merged
			}
		}
		// A request that sets an effort asks for reasoning, so ask Anthropic
		// to show it as a summary (no extra tokens). Claude Opus 5.5 otherwise
		// leaves the thinking out, and at max effort it thought 277 s with
		// nothing to send (tested 2026-11-10): past opencode's 300 s wait for
		// a reply to start. Summaries streamed from 2.2 s, never 3.1 s apart.
		// A thinking setting the client sent itself wins.
		if _, set := body["thinking"]; !set {
			body["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
		}
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
		case "file":
			if block := anthropicFileBlock(p["file"]); block != nil {
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

// anthropicFileBlock turns an OpenAI file part into a document block (or an
// image block for image files). The filename becomes the document title.
func anthropicFileBlock(file any) map[string]any {
	f, ok := parseFilePart(file)
	if !ok {
		return nil
	}
	source := map[string]any{"type": "base64", "media_type": f.MediaType, "data": f.Data}
	switch {
	case f.Data == "":
		source = map[string]any{"type": "url", "url": f.URL}
	case strings.HasPrefix(f.MediaType, "image/"):
		return map[string]any{"type": "image", "source": source}
	case strings.HasPrefix(f.MediaType, "text/"):
		// Anthropic takes plain-text documents as text, not base64.
		if text, err := base64.StdEncoding.DecodeString(f.Data); err == nil {
			source = map[string]any{"type": "text", "media_type": "text/plain", "data": string(text)}
		}
	}
	block := map[string]any{"type": "document", "source": source}
	if f.Filename != "" {
		block["title"] = f.Filename
	}
	return block
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
		entry["input_schema"] = anthropicInputSchema(schema)
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

// anthropicInputSchema returns the schema Anthropic accepts as a tool's input_schema.
// Anthropic refuses the whole request when oneOf, anyOf or allOf sits at the top level
// ("input_schema does not support oneOf, allOf, or anyOf at the top level"); they are
// fine deeper down. OpenAI-style clients do send them there (MaiBot's wait tool says
// "seconds or until, not both" with a top-level oneOf), so those keys are left out of
// a copy. The tool's description still states the rule, and the tool checks its own
// arguments.
func anthropicInputSchema(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema))
	for k, v := range schema {
		switch k {
		case "oneOf", "anyOf", "allOf":
			continue
		}
		out[k] = v
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
		StopReason string         `json:"stop_reason"`
		Usage      anthropicUsage `json:"usage"`
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
		"usage": a.Usage.openAI(),
	}
	return mustJSON(result), finish, hasContent
}

// anthropicUsage is Anthropic's token count for one reply.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"` // the uncached remainder only
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

// openAI is the usage in OpenAI's meaning, for a whole reply and a stream's
// usage chunk alike. OpenAI's prompt_tokens is the whole prompt, cached part
// included; clients size the conversation by it (when to compact). OpenAI has
// no name for cache writes: cache_write_tokens is OpenRouter's, which annai
// reads to price them.
func (u anthropicUsage) openAI() map[string]any {
	prompt := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	return map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      prompt + u.OutputTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens":      u.CacheReadInputTokens,
			"cache_write_tokens": u.CacheCreationInputTokens,
		},
	}
}

// normalizeAnthropicFinish maps a stop_reason to OpenAI's finish_reason. A
// refusal (Anthropic's safety classifiers declined, before or during the
// reply) is OpenAI's content_filter.
func normalizeAnthropicFinish(r string) string {
	switch r {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "":
		return ""
	default:
		return r
	}
}
