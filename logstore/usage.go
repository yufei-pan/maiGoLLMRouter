package logstore

import "encoding/json"

// attemptUsageView is the subset of a router attempt record needed for usage.
type attemptUsageView struct {
	Outcome  string `json:"outcome"`
	Response string `json:"response,omitempty"`
}

// attemptsCount returns the number of attempts recorded on an entry without
// decoding their (potentially large) payloads.
func attemptsCount(attempts any) int {
	if attempts == nil {
		return 0
	}
	raw, err := json.Marshal(attempts)
	if err != nil {
		return 0
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0
	}
	return len(arr)
}

// usageForEntry computes prompt/completion token counts for an entry's index
// row, reusing computeUsage. Attempts are re-marshaled to raw JSON so the same
// logic applies whether they arrive as []router.Attempt (write time) or decoded
// JSON (tests).
func usageForEntry(e Entry) (prompt, completion int, ok bool) {
	var attempts []json.RawMessage
	if e.Attempts != nil {
		if raw, err := json.Marshal(e.Attempts); err == nil {
			_ = json.Unmarshal(raw, &attempts)
		}
	}
	return computeUsage(e.Success, e.Response, attempts)
}

func parseUsageFromBody(raw []byte) (prompt, completion int, ok bool) {
	if len(raw) == 0 {
		return 0, 0, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, 0, false
	}
	if u, exists := m["usage"]; exists {
		var usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			InputTokens      int `json:"input_tokens"`
			OutputTokens     int `json:"output_tokens"`
		}
		if err := json.Unmarshal(u, &usage); err == nil {
			if usage.PromptTokens != 0 || usage.CompletionTokens != 0 {
				return usage.PromptTokens, usage.CompletionTokens, true
			}
			if usage.InputTokens != 0 || usage.OutputTokens != 0 {
				return usage.InputTokens, usage.OutputTokens, true
			}
		}
	}
	if um, exists := m["usageMetadata"]; exists {
		var meta struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		}
		if err := json.Unmarshal(um, &meta); err == nil {
			out := meta.CandidatesTokenCount
			if out == 0 {
				out = meta.ThoughtsTokenCount
			}
			if out == 0 && meta.TotalTokenCount > meta.PromptTokenCount {
				out = meta.TotalTokenCount - meta.PromptTokenCount
			}
			if meta.PromptTokenCount != 0 || out != 0 {
				return meta.PromptTokenCount, out, true
			}
		}
	}
	return 0, 0, false
}

func isFailedAttemptOutcome(outcome string) bool {
	switch outcome {
	case "bad_output", "provider_error", "error", "canceled":
		return true
	default:
		return false
	}
}

// computeUsage derives prompt and completion token counts for log summaries.
// Successful requests use the top-level response only. Failed requests take
// the largest prompt count seen across attempts (and the top-level response as
// a fallback) and sum completion tokens from every failed attempt that logged a
// response body with usage.
func computeUsage(success bool, response json.RawMessage, attempts []json.RawMessage) (prompt, completion int, ok bool) {
	if success {
		return parseUsageFromBody(response)
	}

	var promptSet bool
	var completionSet bool
	for _, raw := range attempts {
		var att attemptUsageView
		if err := json.Unmarshal(raw, &att); err != nil || !isFailedAttemptOutcome(att.Outcome) {
			continue
		}
		if att.Response == "" {
			continue
		}
		p, c, has := parseUsageFromBody([]byte(att.Response))
		if !has {
			continue
		}
		if !promptSet || p > prompt {
			prompt = p
			promptSet = true
		}
		completion += c
		completionSet = true
	}
	if p, _, has := parseUsageFromBody(response); has {
		if !promptSet || p > prompt {
			prompt = p
			promptSet = true
		}
	}
	if promptSet || completionSet {
		return prompt, completion, true
	}
	return 0, 0, false
}
