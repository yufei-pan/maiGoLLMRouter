package provider

import (
	"encoding/json"
	"testing"
)

func anthropicBodyFor(t *testing.T, bodyJSON string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(bodyJSON), &body); err != nil {
		t.Fatal(err)
	}
	return buildAnthropicBody(Request{Op: OpChat, Model: "claude-opus-5-5", Body: body})
}

// pi and opencode send OpenAI's reasoning_effort; Anthropic answers 400
// "reasoning_effort: Extra inputs are not permitted" and takes output_config.effort.
func TestAnthropicReasoningEffortBecomesOutputConfigEffort(t *testing.T) {
	cases := []struct {
		effort any
		want   string // "" = no output_config at all
	}{
		{"high", "high"},
		{"xhigh", "xhigh"},
		{"max", "max"},
		{" XHigh ", "xhigh"},
		{"minimal", "low"},
		{"none", "low"},
		{"", ""},
		{3, ""},
		{nil, ""},
	}
	for _, c := range cases {
		raw, _ := json.Marshal(map[string]any{
			"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
			"reasoning_effort": c.effort,
		})
		got := anthropicBodyFor(t, string(raw))
		if _, has := got["reasoning_effort"]; has {
			t.Errorf("effort %#v: reasoning_effort reached Anthropic", c.effort)
		}
		oc, has := got["output_config"]
		if c.want == "" {
			if has {
				t.Errorf("effort %#v: output_config = %v, want none", c.effort, oc)
			}
			continue
		}
		if e := oc.(map[string]any)["effort"]; e != c.want {
			t.Errorf("effort %#v: output_config.effort = %v, want %s", c.effort, e, c.want)
		}
	}
}

func TestAnthropicOwnOutputConfigEffortWins(t *testing.T) {
	got := anthropicBodyFor(t, `{"messages": [{"role": "user", "content": "hi"}],
		"reasoning_effort": "low", "output_config": {"effort": "max"}}`)
	if e := got["output_config"].(map[string]any)["effort"]; e != "max" {
		t.Errorf("output_config.effort = %v, want the client's own max", e)
	}
}

func TestAnthropicEffortMergesIntoOwnOutputConfig(t *testing.T) {
	var body map[string]any
	if err := json.Unmarshal([]byte(`{"messages": [{"role": "user", "content": "hi"}],
		"reasoning_effort": "high", "output_config": {"format": {"type": "json_schema"}}}`), &body); err != nil {
		t.Fatal(err)
	}
	got := buildAnthropicBody(Request{Op: OpChat, Model: "claude-opus-5-5", Body: body})
	oc := got["output_config"].(map[string]any)
	if oc["effort"] != "high" || oc["format"] == nil {
		t.Errorf("output_config = %v, want effort high beside the client's format", oc)
	}
	// The caller's body goes to the next target as it is.
	if _, has := body["output_config"].(map[string]any)["effort"]; has {
		t.Error("the caller's output_config was changed")
	}
}

// Anthropic rejects every field name it doesn't know; pi sends "store": false on
// every request, which made every Claude call a 400.
func TestAnthropicDropsOpenAIOnlyFields(t *testing.T) {
	got := anthropicBodyFor(t, `{"messages": [{"role": "user", "content": "hi"}],
		"store": false, "parallel_tool_calls": true, "metadata": {"a": "b"}, "user": "u1",
		"service_tier": "auto", "prompt_cache_key": "k", "safety_identifier": "s",
		"thinking": {"type": "adaptive"}}`)
	for _, k := range openAIOnlyFields {
		if _, has := got[k]; has {
			t.Errorf("%s reached Anthropic", k)
		}
	}
	if got["thinking"] == nil {
		t.Error("thinking, an Anthropic field, was not passed through")
	}
}

func TestAnthropicAutomaticCaching(t *testing.T) {
	got := anthropicBodyFor(t, `{"messages": [{"role": "user", "content": "hi"}]}`)
	if cc, _ := got["cache_control"].(map[string]any); cc["type"] != "ephemeral" || len(cc) != 1 {
		t.Errorf("cache_control = %v, want {type: ephemeral}", got["cache_control"])
	}
	got = anthropicBodyFor(t, `{"messages": [{"role": "user", "content": "hi"}],
		"cache_control": {"type": "ephemeral", "ttl": "1h"}}`)
	if cc := got["cache_control"].(map[string]any); cc["ttl"] != "1h" {
		t.Errorf("cache_control = %v, want the client's own 1h", cc)
	}
}

// Anthropic's input_tokens is only the uncached remainder; OpenAI's prompt_tokens
// is the whole prompt, which clients use to size the conversation.
func TestAnthropicUsageCountsCachedTokens(t *testing.T) {
	raw := []byte(`{"content": [{"type": "text", "text": "ok"}], "stop_reason": "end_turn",
		"usage": {"input_tokens": 4, "cache_creation_input_tokens": 300,
		"cache_read_input_tokens": 19621, "output_tokens": 7}}`)
	out, _, _ := anthropicToOpenAIChat(raw, "claude-opus-5-5")
	var chat struct {
		Usage struct {
			Prompt  int `json:"prompt_tokens"`
			Total   int `json:"total_tokens"`
			Details struct {
				Cached  int `json:"cached_tokens"`
				Written int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &chat); err != nil {
		t.Fatal(err)
	}
	if chat.Usage.Prompt != 19925 || chat.Usage.Total != 19932 || chat.Usage.Details.Cached != 19621 || chat.Usage.Details.Written != 300 {
		t.Errorf("usage = %+v, want prompt 19925, total 19932, cached 19621, written 300", chat.Usage)
	}

	resp, err := ChatToResponses(out, "claude-opus-5-5", "stop")
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Usage struct {
			Input   int `json:"input_tokens"`
			Details struct {
				Cached int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(resp, &r); err != nil {
		t.Fatal(err)
	}
	if r.Usage.Input != 19925 || r.Usage.Details.Cached != 19621 {
		t.Errorf("Responses usage = %+v, want input 19925, cached 19621", r.Usage)
	}
}
