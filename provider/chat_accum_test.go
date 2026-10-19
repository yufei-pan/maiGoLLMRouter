package provider

import (
	"encoding/json"
	"testing"
)

// chunk decodes one chat.completion.chunk written as JSON.
func chunk(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad chunk %s: %v", s, err)
	}
	return m
}

// completion decodes an accumulator's Body.
type completion struct {
	Choices []struct {
		Message struct {
			Content          *string `json:"content"`
			ReasoningContent string  `json:"reasoning_content"`
			ToolCalls        []struct {
				ID               string `json:"id"`
				ThoughtSignature string `json:"thought_signature"`
				Function         struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

func decodeCompletion(t *testing.T, body []byte) completion {
	t.Helper()
	var c completion
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("Body is not a chat.completion: %v\n%s", err, body)
	}
	if len(c.Choices) == 0 {
		t.Fatalf("Body has no choices: %s", body)
	}
	return c
}

func TestChatAccumulatorTextAndUsage(t *testing.T) {
	a := newChatAccumulator("m")
	if a.add(chunk(t, `{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`)) {
		t.Error("a role-only chunk is not output")
	}
	if !a.add(chunk(t, `{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`)) {
		t.Error("a content chunk is output")
	}
	a.add(chunk(t, `{"choices":[{"index":0,"delta":{"content":"lo"}}]}`))
	a.add(chunk(t, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	a.add(chunk(t, `{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))

	s := a.state()
	if !s.Output || !s.HasContent || s.FinishReason != "stop" || s.Prohibited {
		t.Fatalf("state = %+v", s)
	}
	c := decodeCompletion(t, s.Body)
	if c.Choices[0].Message.Content == nil || *c.Choices[0].Message.Content != "Hello" {
		t.Errorf("content = %v, want Hello", c.Choices[0].Message.Content)
	}
	if c.Usage.PromptTokens != 5 {
		t.Errorf("usage.prompt_tokens = %d, want 5", c.Usage.PromptTokens)
	}
}

func TestChatAccumulatorInterleavedToolCalls(t *testing.T) {
	a := newChatAccumulator("m")
	a.add(chunk(t, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c0","type":"function","function":{"name":"a","arguments":"{\"x\""}}]}}]}`))
	a.add(chunk(t, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c1","type":"function","thought_signature":"sig","function":{"name":"b","arguments":"{}"}}]}}]}`))
	a.add(chunk(t, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`))
	a.add(chunk(t, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))

	s := a.state()
	if !s.Output || !s.HasContent || s.FinishReason != "tool_calls" {
		t.Fatalf("state = %+v", s)
	}
	calls := decodeCompletion(t, s.Body).Choices[0].Message.ToolCalls
	if len(calls) != 2 {
		t.Fatalf("tool calls = %+v, want 2", calls)
	}
	if calls[0].ID != "c0" || calls[0].Function.Name != "a" || calls[0].Function.Arguments != `{"x":1}` {
		t.Errorf("call 0 = %+v", calls[0])
	}
	if calls[1].ID != "c1" || calls[1].Function.Name != "b" || calls[1].Function.Arguments != `{}` || calls[1].ThoughtSignature != "sig" {
		t.Errorf("call 1 = %+v", calls[1])
	}
}

func TestChatAccumulatorReasoningCountsAsOutput(t *testing.T) {
	a := newChatAccumulator("m")
	if !a.add(chunk(t, `{"choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`)) {
		t.Error("reasoning_content is output")
	}
	s := a.state()
	if !s.Output || s.HasContent {
		t.Fatalf("state = %+v, want output without content", s)
	}
	if got := decodeCompletion(t, s.Body).Choices[0].Message.ReasoningContent; got != "think" {
		t.Errorf("reasoning_content = %q, want think", got)
	}

	b := newChatAccumulator("m")
	if !b.add(chunk(t, `{"choices":[{"index":0,"delta":{"reasoning":"hmm"}}]}`)) {
		t.Error("reasoning is output")
	}
}

func TestChatAccumulatorContentFilterIsProhibited(t *testing.T) {
	a := newChatAccumulator("m")
	a.add(chunk(t, `{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`))
	a.add(chunk(t, `{"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`))
	s := a.state()
	if !s.Prohibited || s.Output {
		t.Fatalf("state = %+v, want prohibited without output", s)
	}
}
