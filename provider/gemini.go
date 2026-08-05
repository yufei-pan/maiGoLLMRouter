package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// geminiConsumed lists OpenAI fields handled explicitly; everything else is
// passed through, either to the top level (geminiTopLevel) or, by default, into
// generationConfig (#13).
var geminiConsumed = map[string]bool{
	"model": true, "messages": true, "input": true, "stream": true,
	"temperature": true, "top_p": true, "max_tokens": true,
	"max_completion_tokens": true, "stop": true, "n": true,
	"encoding_format": true, "dimensions": true, "user": true,
	"tools": true, "tool_choice": true, "functions": true, "function_call": true,
	"response_format": true,
}

// geminiTopLevel lists generateContent request fields that belong at the top
// level of the request body rather than inside generationConfig. Both camelCase
// and snake_case forms are accepted (the Gemini API understands both). Passing
// e.g. "tools" into generationConfig is rejected by Gemini, so these are routed
// to the top level on passthrough. (OpenAI "tools"/"tool_choice" are translated
// separately into Gemini's tools/toolConfig, so they are not listed here.)
var geminiTopLevel = map[string]bool{
	"toolConfig": true, "tool_config": true,
	"safetySettings": true, "safety_settings": true,
	"systemInstruction": true, "system_instruction": true,
	"cachedContent": true, "cached_content": true,
	"labels": true,
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

	msgs, _ := req.Body["messages"].([]any)
	// Map tool_call_id -> function name so tool-result messages can be turned
	// into Gemini functionResponse parts (which are keyed by name, not id).
	toolNames := collectToolCallNames(msgs)
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
			parts := []map[string]any{}
			if text != "" {
				parts = append(parts, map[string]any{"text": text})
			}
			parts = append(parts, assistantToolCallParts(mm["tool_calls"])...)
			if len(parts) == 0 {
				parts = append(parts, map[string]any{"text": ""})
			}
			contents = append(contents, map[string]any{"role": "model", "parts": parts})
		case "tool", "function":
			contents = append(contents, map[string]any{
				"role":  "user",
				"parts": []map[string]any{toolResponsePart(mm, text, toolNames)},
			})
		default: // user
			contents = append(contents, map[string]any{"role": "user", "parts": geminiUserParts(mm["content"])})
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
	applyGeminiResponseFormat(genCfg, req.Body["response_format"])
	// Passthrough of unknown args. Known top-level request fields (tools,
	// safetySettings, etc.) go to the body root; an explicit generationConfig is
	// merged into ours; everything else falls into generationConfig.
	topLevel := map[string]any{}
	for k, v := range extras(req.Body, geminiConsumed) {
		switch {
		case k == "generationConfig" || k == "generation_config":
			if gc, ok := v.(map[string]any); ok {
				for gk, gv := range gc {
					genCfg[gk] = gv
				}
			}
		case geminiTopLevel[k]:
			topLevel[k] = v
		default:
			genCfg[k] = v
		}
	}

	body := map[string]any{"contents": contents}
	if len(systemParts) > 0 {
		body["systemInstruction"] = map[string]any{"parts": systemParts}
	}
	// Explicit top-level passthrough fields win over derived ones.
	for k, v := range topLevel {
		body[k] = v
	}
	// Translate OpenAI tool calling into Gemini's tools / toolConfig.
	if decls := geminiFunctionDeclarations(req.Body["tools"], req.Body["functions"]); decls != nil {
		body["tools"] = []any{map[string]any{"functionDeclarations": decls}}
	}
	if tc := geminiToolConfig(req.Body["tool_choice"]); tc != nil {
		body["toolConfig"] = tc
	}
	if len(genCfg) > 0 {
		body["generationConfig"] = genCfg
	}
	return body
}

// collectToolCallNames maps each assistant tool_call id to its function name so
// later tool-result messages can be converted to Gemini functionResponse parts.
func collectToolCallNames(msgs []any) map[string]string {
	names := map[string]string{}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		tcs, ok := mm["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, tc := range tcs {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tcm["function"].(map[string]any)
			if id := asString(tcm, "id"); id != "" && fn != nil {
				names[id] = asString(fn, "name")
			}
		}
	}
	return names
}

// geminiUserParts converts OpenAI user content (string or parts, including
// image_url) into Gemini parts. Text-only content stays a single text part.
func geminiUserParts(content any) []map[string]any {
	parts := openAIContentParts(content)
	if len(parts) == 0 {
		return []map[string]any{{"text": contentToText(content)}}
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch asString(p, "type") {
		case "text", "":
			if t := asString(p, "text"); t != "" {
				out = append(out, map[string]any{"text": t})
			}
		case "image_url":
			if block := geminiImagePart(p["image_url"]); block != nil {
				out = append(out, block)
			}
		}
	}
	if len(out) == 0 {
		return []map[string]any{{"text": contentToText(content)}}
	}
	return out
}

func geminiImagePart(imageURL any) map[string]any {
	url, mediaType, data, ok := parseImageURL(imageURL)
	if !ok {
		return nil
	}
	if data != "" {
		if mediaType == "" {
			mediaType = "image/png"
		}
		return map[string]any{"inlineData": map[string]any{
			"mimeType": mediaType,
			"data":     data,
		}}
	}
	return map[string]any{"fileData": map[string]any{
		"fileUri": url,
	}}
}

// applyGeminiResponseFormat maps OpenAI response_format into Gemini
// generationConfig fields. Unknown shapes are ignored (field is consumed so it
// cannot leak into generationConfig and cause a 400).
func applyGeminiResponseFormat(genCfg map[string]any, rf any) {
	m, ok := rf.(map[string]any)
	if !ok {
		return
	}
	switch asString(m, "type") {
	case "json_object":
		genCfg["responseMimeType"] = "application/json"
	case "json_schema":
		genCfg["responseMimeType"] = "application/json"
		js, _ := m["json_schema"].(map[string]any)
		if js == nil {
			return
		}
		if schema, ok := js["schema"]; ok {
			if s := sanitizeSchema(schema); s != nil {
				genCfg["responseSchema"] = s
			}
		}
	}
}

// assistantToolCallParts converts an assistant message's tool_calls into Gemini
// functionCall parts.
func assistantToolCallParts(v any) []map[string]any {
	tcs, ok := v.([]any)
	if !ok {
		return nil
	}
	parts := make([]map[string]any, 0, len(tcs))
	for _, tc := range tcs {
		tcm, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tcm["function"].(map[string]any)
		if !ok {
			continue
		}
		args := map[string]any{}
		if as, ok := fn["arguments"].(string); ok && as != "" {
			_ = json.Unmarshal([]byte(as), &args)
		}
		parts = append(parts, map[string]any{"functionCall": map[string]any{
			"name": asString(fn, "name"),
			"args": args,
		}})
	}
	return parts
}

// toolResponsePart converts an OpenAI tool/function result message into a Gemini
// functionResponse part. The response value must be an object; non-JSON content
// is wrapped under "result".
func toolResponsePart(mm map[string]any, text string, toolNames map[string]string) map[string]any {
	name := asString(mm, "name")
	if id := asString(mm, "tool_call_id"); id != "" {
		if n, ok := toolNames[id]; ok && n != "" {
			name = n
		}
	}
	var respObj map[string]any
	if err := json.Unmarshal([]byte(text), &respObj); err != nil || respObj == nil {
		respObj = map[string]any{"result": text}
	}
	return map[string]any{"functionResponse": map[string]any{
		"name":     name,
		"response": respObj,
	}}
}

// geminiFunctionDeclarations builds Gemini functionDeclarations from OpenAI
// "tools" (array of {type:"function", function:{...}}) and/or the legacy
// "functions" array. If tools are already Gemini-native (contain
// functionDeclarations), they are returned unchanged. Returns nil when there is
// nothing to translate.
func geminiFunctionDeclarations(tools, functions any) []any {
	var decls []any

	if arr, ok := tools.([]any); ok {
		// Already Gemini-native: pass the declarations through untouched.
		if len(arr) > 0 {
			if m, ok := arr[0].(map[string]any); ok {
				if fd, ok := m["functionDeclarations"].([]any); ok {
					return fd
				}
				if fd, ok := m["function_declarations"].([]any); ok {
					return fd
				}
			}
		}
		for _, t := range arr {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := tm["function"].(map[string]any); ok {
				decls = append(decls, sanitizeFunctionDecl(fn))
			} else if _, hasName := tm["name"]; hasName {
				decls = append(decls, sanitizeFunctionDecl(tm))
			}
		}
	}

	if arr, ok := functions.([]any); ok {
		for _, f := range arr {
			if fm, ok := f.(map[string]any); ok {
				decls = append(decls, sanitizeFunctionDecl(fm))
			}
		}
	}

	if len(decls) == 0 {
		return nil
	}
	return decls
}

// sanitizeFunctionDecl keeps only the fields Gemini accepts on a function
// declaration and sanitizes its parameter schema.
func sanitizeFunctionDecl(fn map[string]any) map[string]any {
	out := map[string]any{"name": asString(fn, "name")}
	if desc := asString(fn, "description"); desc != "" {
		out["description"] = desc
	}
	if params, ok := fn["parameters"]; ok {
		if s := sanitizeSchema(params); s != nil {
			out["parameters"] = s
		}
	}
	return out
}

// geminiSchemaKeys is the whitelist of JSON Schema fields Gemini's Schema type
// accepts. Anything else (e.g. additionalProperties, $schema) is dropped, since
// Gemini rejects unknown fields.
var geminiSchemaKeys = map[string]bool{
	"type": true, "format": true, "title": true, "description": true,
	"nullable": true, "enum": true, "items": true, "properties": true,
	"required": true, "minimum": true, "maximum": true, "minItems": true,
	"maxItems": true, "minLength": true, "maxLength": true, "pattern": true,
	"default": true, "example": true, "anyOf": true, "propertyOrdering": true,
	"minProperties": true, "maxProperties": true,
}

// sanitizeSchema recursively drops JSON Schema fields unsupported by Gemini.
func sanitizeSchema(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		if !geminiSchemaKeys[k] {
			continue
		}
		switch k {
		case "properties":
			if props, ok := val.(map[string]any); ok {
				np := make(map[string]any, len(props))
				for pk, pv := range props {
					np[pk] = sanitizeSchema(pv)
				}
				out[k] = np
			}
		case "items":
			out[k] = sanitizeSchema(val)
		case "anyOf":
			if arr, ok := val.([]any); ok {
				na := make([]any, 0, len(arr))
				for _, e := range arr {
					na = append(na, sanitizeSchema(e))
				}
				out[k] = na
			}
		default:
			out[k] = val
		}
	}
	return out
}

// geminiToolConfig translates OpenAI tool_choice into Gemini's toolConfig.
func geminiToolConfig(v any) map[string]any {
	switch tc := v.(type) {
	case string:
		mode := ""
		switch strings.ToLower(tc) {
		case "auto":
			mode = "AUTO"
		case "none":
			mode = "NONE"
		case "required", "any":
			mode = "ANY"
		}
		if mode == "" {
			return nil
		}
		return map[string]any{"functionCallingConfig": map[string]any{"mode": mode}}
	case map[string]any:
		if fn, ok := tc["function"].(map[string]any); ok {
			if name := asString(fn, "name"); name != "" {
				return map[string]any{"functionCallingConfig": map[string]any{
					"mode":                 "ANY",
					"allowedFunctionNames": []any{name},
				}}
			}
		}
	}
	return nil
}

func geminiToOpenAIChat(raw []byte, model string) (openAIBody []byte, finish string, hasContent bool) {
	var g struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
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
		var toolCalls []map[string]any
		for _, p := range c.Content.Parts {
			if p.FunctionCall != nil {
				args := p.FunctionCall.Args
				if args == nil {
					args = map[string]any{}
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   "call_" + randomID(),
					"type": "function",
					"function": map[string]any{
						"name":      p.FunctionCall.Name,
						"arguments": string(mustJSON(args)),
					},
				})
				continue
			}
			text.WriteString(p.Text)
		}

		fr := normalizeGeminiFinish(c.FinishReason)
		msg := map[string]any{"role": "assistant"}
		if text.Len() > 0 {
			msg["content"] = text.String()
		} else {
			msg["content"] = nil
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
			fr = "tool_calls" // OpenAI clients key off this to read tool_calls
		}
		if i == 0 {
			finish = fr
			hasContent = text.Len() > 0 || len(toolCalls) > 0
		}
		choices = append(choices, map[string]any{
			"index":         i,
			"message":       msg,
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
