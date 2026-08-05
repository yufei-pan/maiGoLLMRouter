package provider

import (
	"fmt"
)

// PortableForChat reports whether a Responses request body can be translated
// into Chat Completions (function tools + portable input items only).
func PortableForChat(body map[string]any) error {
	if err := portableTools(body["tools"]); err != nil {
		return err
	}
	return portableInput(body["input"])
}

func portableTools(raw any) error {
	if raw == nil {
		return nil
	}
	tools, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("tools: non-portable shape")
	}
	for i, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			return fmt.Errorf("tools[%d]: non-portable shape", i)
		}
		typ, _ := m["type"].(string)
		if typ != "function" {
			return fmt.Errorf("tools[%d]: type %q is not portable for chat", i, typ)
		}
	}
	return nil
}

func portableInput(raw any) error {
	if raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case string:
		return nil
	case []any:
		for i, item := range v {
			if err := portableInputItem(item, i); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("input: non-portable shape")
	}
}

func portableInputItem(raw any, i int) error {
	m, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("input[%d]: non-portable shape", i)
	}
	if typ, hasType := m["type"].(string); hasType {
		switch typ {
		case "function_call":
			if _, ok := m["call_id"]; !ok {
				return fmt.Errorf("input[%d]: function_call missing call_id", i)
			}
			if _, ok := m["name"]; !ok {
				return fmt.Errorf("input[%d]: function_call missing name", i)
			}
			if _, ok := m["arguments"]; !ok {
				return fmt.Errorf("input[%d]: function_call missing arguments", i)
			}
			return nil
		case "function_call_output":
			if _, ok := m["call_id"]; !ok {
				return fmt.Errorf("input[%d]: function_call_output missing call_id", i)
			}
			if _, ok := m["output"]; !ok {
				return fmt.Errorf("input[%d]: function_call_output missing output", i)
			}
			return nil
		case "message":
			// role message with explicit type; fall through to role check
		default:
			// Reject reasoning, web_search_call, and any other non-listed type.
			return fmt.Errorf("input[%d]: type %q is not portable for chat", i, typ)
		}
	}
	role, _ := m["role"].(string)
	if !isPortableRole(role) {
		return fmt.Errorf("input[%d]: role %q is not portable for chat", i, role)
	}
	return portableContent(m["content"], i)
}

func isPortableRole(role string) bool {
	switch role {
	case "system", "user", "assistant", "developer":
		return true
	default:
		return false
	}
}

func portableContent(raw any, i int) error {
	switch v := raw.(type) {
	case nil, string:
		return nil
	case []any:
		for j, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				return fmt.Errorf("input[%d].content[%d]: non-portable shape", i, j)
			}
			typ, _ := pm["type"].(string)
			switch typ {
			case "input_text", "output_text", "text", "input_image", "image_url":
				// ok
			default:
				return fmt.Errorf("input[%d].content[%d]: type %q is not portable for chat", i, j, typ)
			}
		}
		return nil
	default:
		return fmt.Errorf("input[%d]: content non-portable shape", i)
	}
}

// ResponsesToChat translates a portable Responses request body into a Chat
// Completions body. model is the resolved downstream model name.
func ResponsesToChat(body map[string]any, model string) (map[string]any, error) {
	if err := PortableForChat(body); err != nil {
		return nil, err
	}

	out := map[string]any{"model": model}

	msgs, err := responsesInputToMessages(body["input"])
	if err != nil {
		return nil, err
	}
	out["messages"] = msgs

	if tools, ok := body["tools"].([]any); ok {
		out["tools"] = responsesToolsToChat(tools)
	}

	if v, ok := body["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}

	if text, ok := body["text"].(map[string]any); ok {
		if format, ok := text["format"]; ok {
			out["response_format"] = format
		}
	}

	consumed := map[string]struct{}{
		"model":                {},
		"input":                {},
		"tools":                {},
		"text":                 {},
		"max_output_tokens":    {},
		"max_tokens":           {},
		"stream":               {},
		"stream_options":       {},
		"previous_response_id": {},
		"store":                {},
	}
	for k, v := range body {
		if _, skip := consumed[k]; skip {
			continue
		}
		out[k] = v
	}

	return out, nil
}

func responsesInputToMessages(raw any) ([]any, error) {
	if raw == nil {
		return []any{}, nil
	}
	if s, ok := raw.(string); ok {
		return []any{map[string]any{"role": "user", "content": s}}, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("input: unexpected shape")
	}
	msgs := make([]any, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("input item: unexpected shape")
		}
		if typ, _ := m["type"].(string); typ == "function_call" {
			msgs = append(msgs, map[string]any{
				"role": "assistant",
				"tool_calls": []any{
					map[string]any{
						"id":   m["call_id"],
						"type": "function",
						"function": map[string]any{
							"name":      m["name"],
							"arguments": stringifyAny(m["arguments"]),
						},
					},
				},
			})
			continue
		}
		if typ, _ := m["type"].(string); typ == "function_call_output" {
			msgs = append(msgs, map[string]any{
				"role":         "tool",
				"tool_call_id": m["call_id"],
				"content":      stringifyAny(m["output"]),
			})
			continue
		}
		role, _ := m["role"].(string)
		if role == "developer" {
			role = "system"
		}
		content, err := responsesContentToChat(m["content"])
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, map[string]any{"role": role, "content": content})
	}
	return msgs, nil
}

func responsesContentToChat(raw any) (any, error) {
	switch v := raw.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case []any:
		parts := make([]any, 0, len(v))
		for _, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content part: unexpected shape")
			}
			typ, _ := pm["type"].(string)
			switch typ {
			case "input_text", "output_text", "text":
				parts = append(parts, map[string]any{"type": "text", "text": stringifyAny(pm["text"])})
			case "input_image", "image_url":
				img := pm["image_url"]
				if s, ok := img.(string); ok {
					img = map[string]any{"url": s}
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": img})
			default:
				return nil, fmt.Errorf("content part type %q not portable", typ)
			}
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("content: unexpected shape")
	}
}

func responsesToolsToChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn := map[string]any{}
		if name, ok := m["name"]; ok {
			fn["name"] = name
		}
		if desc, ok := m["description"]; ok {
			fn["description"] = desc
		}
		if params, ok := m["parameters"]; ok {
			fn["parameters"] = params
		}
		// Responses may already nest under "function".
		if nested, ok := m["function"].(map[string]any); ok {
			for k, v := range nested {
				fn[k] = v
			}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

func stringifyAny(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		return fmt.Sprint(s)
	}
}
