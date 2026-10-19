package provider

import (
	"sort"
	"strings"
	"time"
)

// chatAccumulator folds chat.completion.chunk objects into the complete reply
// they add up to: the body logged for a streamed request, and the facts the
// router checks (output started, finish reason, content).
type chatAccumulator struct {
	id      string
	model   string
	created any
	choices map[int]*chatChoiceAcc
	usage   map[string]any
	output  bool
}

type chatChoiceAcc struct {
	content   strings.Builder
	reasoning strings.Builder
	refusal   strings.Builder
	calls     map[int]*chatCallAcc
	finish    string
}

type chatCallAcc struct {
	id, typ, name, signature string
	args                     strings.Builder
}

func newChatAccumulator(model string) *chatAccumulator {
	return &chatAccumulator{
		id:      "chatcmpl-" + randomID(),
		model:   model,
		created: time.Now().Unix(),
		choices: make(map[int]*chatChoiceAcc),
	}
}

// add folds one chunk into the reply. It reports whether the chunk carried real
// output: text, reasoning, a refusal, or any piece of a tool call.
func (a *chatAccumulator) add(chunk map[string]any) (output bool) {
	if s := asString(chunk, "id"); s != "" {
		a.id = s
	}
	if s := asString(chunk, "model"); s != "" {
		a.model = s
	}
	if v, ok := chunk["created"]; ok && v != nil {
		a.created = v
	}
	if u, ok := chunk["usage"].(map[string]any); ok {
		a.usage = u
	}
	choices, _ := chunk["choices"].([]any)
	for _, raw := range choices {
		ch, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		c := a.choice(indexOf(ch))
		delta, _ := ch["delta"].(map[string]any)
		if s := asString(delta, "content"); s != "" {
			c.content.WriteString(s)
			output = true
		}
		for _, k := range []string{"reasoning_content", "reasoning"} {
			if s := asString(delta, k); s != "" {
				c.reasoning.WriteString(s)
				output = true
			}
		}
		if s := asString(delta, "refusal"); s != "" {
			c.refusal.WriteString(s)
			output = true
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, rawTC := range tcs {
			tc, ok := rawTC.(map[string]any)
			if !ok {
				continue
			}
			output = true
			call := c.call(indexOf(tc))
			if s := asString(tc, "id"); s != "" && call.id == "" {
				call.id = s
			}
			if s := asString(tc, "type"); s != "" {
				call.typ = s
			}
			if s := thoughtSignatureFrom(tc); s != "" {
				call.signature = s
			}
			fn, _ := tc["function"].(map[string]any)
			if s := asString(fn, "name"); s != "" && call.name == "" {
				call.name = s
			}
			call.args.WriteString(asString(fn, "arguments"))
		}
		if s := asString(ch, "finish_reason"); s != "" {
			c.finish = s
		}
	}
	if output {
		a.output = true
	}
	return output
}

func (a *chatAccumulator) choice(i int) *chatChoiceAcc {
	c, ok := a.choices[i]
	if !ok {
		c = &chatChoiceAcc{calls: make(map[int]*chatCallAcc)}
		a.choices[i] = c
	}
	return c
}

func (c *chatChoiceAcc) call(i int) *chatCallAcc {
	call, ok := c.calls[i]
	if !ok {
		call = &chatCallAcc{}
		c.calls[i] = call
	}
	return call
}

// indexOf reads an "index" field, 0 when absent.
func indexOf(m map[string]any) int {
	if f, ok := m["index"].(float64); ok {
		return int(f)
	}
	return 0
}

func sortedKeys[V any](m map[int]V) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

// state reports the reply so far. Body is a chat.completion; Raw is left to
// the stream that owns the accumulator.
func (a *chatAccumulator) state() StreamState {
	if len(a.choices) == 0 {
		a.choice(0)
	}
	choices := make([]any, 0, len(a.choices))
	var s StreamState
	order := sortedKeys(a.choices)
	for _, i := range order {
		c := a.choices[i]
		msg := map[string]any{"role": "assistant", "content": c.content.String()}
		if c.reasoning.Len() > 0 {
			msg["reasoning_content"] = c.reasoning.String()
		}
		if c.refusal.Len() > 0 {
			msg["refusal"] = c.refusal.String()
		}
		if len(c.calls) > 0 {
			calls := make([]any, 0, len(c.calls))
			for _, j := range sortedKeys(c.calls) {
				call := c.calls[j]
				typ := call.typ
				if typ == "" {
					typ = "function"
				}
				tc := map[string]any{
					"id":       call.id,
					"type":     typ,
					"function": map[string]any{"name": call.name, "arguments": call.args.String()},
				}
				if call.signature != "" {
					tc["thought_signature"] = call.signature
				}
				calls = append(calls, tc)
			}
			msg["tool_calls"] = calls
			if c.content.Len() == 0 {
				msg["content"] = nil
			}
		}
		var finish any
		if c.finish != "" {
			finish = c.finish
		}
		choices = append(choices, map[string]any{"index": i, "message": msg, "finish_reason": finish})
		if i == order[0] {
			s.FinishReason = c.finish
			s.HasContent = c.content.Len() > 0 || len(c.calls) > 0
		}
	}
	body := map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": a.created,
		"model":   a.model,
		"choices": choices,
	}
	if a.usage != nil {
		body["usage"] = a.usage
	}
	s.Output = a.output
	s.Body = mustJSON(body)
	s.Prohibited = isProhibitedContent(s.Body)
	return s
}
