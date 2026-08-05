package provider

import (
	"encoding/json"
	"strings"
)

// responsesOutcome extracts finish reason and content presence from a Responses
// body, aligned with MaiBot completed-response success criteria.
func responsesOutcome(raw []byte) (finishReason string, hasContent bool) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", false
	}
	status, _ := body["status"].(string)
	switch status {
	case "failed", "incomplete", "cancelled":
		return "", false
	}

	hasToolCall := false
	output, _ := body["output"].([]any)
	for _, item := range output {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch m["type"] {
		case "function_call":
			hasContent = true
			hasToolCall = true
		case "message":
			if responsesMessageHasContent(m) {
				hasContent = true
			}
		case "reasoning":
			if responsesReasoningHasContent(m) {
				hasContent = true
			}
		}
	}

	if hasToolCall {
		return "tool_calls", true
	}
	if hasContent {
		return "stop", true
	}
	return "", false
}

func responsesMessageHasContent(m map[string]any) bool {
	content, ok := m["content"].([]any)
	if !ok {
		return false
	}
	for _, part := range content {
		pm, ok := part.(map[string]any)
		if !ok {
			continue
		}
		switch pm["type"] {
		case "output_text":
			if text, _ := pm["text"].(string); text != "" {
				return true
			}
		case "refusal":
			if text, _ := pm["refusal"].(string); text != "" {
				return true
			}
			if text, _ := pm["text"].(string); text != "" {
				return true
			}
		}
	}
	return false
}

func responsesReasoningHasContent(m map[string]any) bool {
	if text, _ := m["reasoning_text"].(string); text != "" {
		return true
	}
	summary, ok := m["summary"].([]any)
	if !ok {
		return false
	}
	for _, s := range summary {
		sm, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if text, _ := sm["text"].(string); text != "" {
			return true
		}
	}
	return false
}

// isResponsesUnsupported reports whether an HTTP error indicates the upstream
// does not expose a /responses route (vs. a normal Responses validation error).
func isResponsesUnsupported(status int, raw []byte) bool {
	if status == 404 {
		return true
	}
	if status < 400 || status > 499 {
		return false
	}
	s := strings.ToLower(string(raw))
	if !strings.Contains(s, "/responses") {
		return false
	}
	for _, marker := range []string{"not found", "unknown", "no route", "not supported", "404"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
