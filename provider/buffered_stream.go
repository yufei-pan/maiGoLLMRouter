package provider

import (
	"encoding/json"
	"io"
	"strings"
)

// BufferedStream turns a complete, already checked reply into a stream, for a
// dialect the router does not stream, a provider that ignored "stream", or
// streaming switched off. Nothing reaches the client before the reply is
// final, so failures are still HTTP errors.
func BufferedStream(op Operation, resp *Response, wantsUsage bool) *Stream {
	src := &fixedSource{
		op: op,
		st: StreamState{
			Output: resp.HasContent, FinishReason: resp.FinishReason, HasContent: resp.HasContent,
			Prohibited: resp.Prohibited, Body: resp.OpenAIBody, Raw: resp.RawResponse,
		},
	}
	if op == OpResponses {
		src.frames, src.em = bufferedResponsesFrames(resp.OpenAIBody)
	} else {
		src.frames = bufferedChatFrames(resp.OpenAIBody, wantsUsage)
	}
	return &Stream{
		HTTPStatus: resp.HTTPStatus, OutboundURL: resp.OutboundURL, OutboundBody: resp.OutboundBody,
		LearnChatOnly: resp.LearnChatOnly, src: src,
	}
}

// RefusalStream is the short stream sent when every target refused on policy
// grounds: a reply that ends as a refusal, so the client sees the model
// declining rather than a failure.
func RefusalStream(op Operation, model string) *Stream {
	src := &fixedSource{op: op}
	if op == OpResponses {
		em := newResponsesEmitter(model)
		final := em.inProgress()
		final["status"] = "incomplete"
		final["incomplete_details"] = map[string]any{"reason": "content_filter"}
		src.frames = []Frame{
			em.event("response.created", map[string]any{"response": em.inProgress()}),
			em.event("response.incomplete", map[string]any{"response": final}),
		}
		src.em = em
		src.st.Body = mustJSON(final)
	} else {
		acc := newChatAccumulator(model)
		base := chunkBase(acc.id, acc.created, model)
		role := withChoice(base, 0, map[string]any{"role": "assistant", "content": ""}, nil)
		finish := withChoice(base, 0, map[string]any{}, "content_filter")
		acc.add(role)
		acc.add(finish)
		src.frames = []Frame{{Data: mustJSON(role)}, {Data: mustJSON(finish)}, {Data: []byte("[DONE]")}}
		src.st.Body = acc.state().Body
		src.st.FinishReason = "content_filter"
	}
	src.st.Prohibited = true
	return &Stream{HTTPStatus: 200, src: src}
}

// fixedSource hands out frames worked out in advance.
type fixedSource struct {
	op     Operation
	frames []Frame
	st     StreamState
	em     *responsesEmitter // Responses only: for the next sequence_number
}

func (s *fixedSource) next() (Frame, error) {
	if len(s.frames) == 0 {
		return Frame{}, io.EOF
	}
	f := s.frames[0]
	s.frames = s.frames[1:]
	return f, nil
}

func (s *fixedSource) state() StreamState { return s.st }

func (s *fixedSource) errorFrame(status int, message string) Frame {
	if s.em != nil {
		return responsesErrorFrame(s.em.seq(), message)
	}
	return chatErrorFrame(status, message)
}

func (s *fixedSource) close() {}

func chunkBase(id string, created any, model string) map[string]any {
	return map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model}
}

func withChoice(base map[string]any, index int, delta map[string]any, finish any) map[string]any {
	c := make(map[string]any, len(base)+1)
	for k, v := range base {
		c[k] = v
	}
	c["choices"] = []any{map[string]any{"index": index, "delta": delta, "finish_reason": finish}}
	return c
}

// bufferedChatFrames splits a chat.completion into the chunks a stream of it
// would have had: per choice the role, any reasoning, the text, each tool call
// whole and the finish; then usage when asked; then [DONE].
func bufferedChatFrames(body []byte, wantsUsage bool) []Frame {
	var c struct {
		ID      string `json:"id"`
		Created any    `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Content          any              `json:"content"`
				ReasoningContent string           `json:"reasoning_content"`
				ToolCalls        []map[string]any `json:"tool_calls"`
			} `json:"message"`
			FinishReason any `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	_ = json.Unmarshal(body, &c)
	base := chunkBase(c.ID, c.Created, c.Model)
	var frames []Frame
	add := func(m map[string]any) { frames = append(frames, Frame{Data: mustJSON(m)}) }
	for _, ch := range c.Choices {
		add(withChoice(base, ch.Index, map[string]any{"role": "assistant", "content": ""}, nil))
		if ch.Message.ReasoningContent != "" {
			add(withChoice(base, ch.Index, map[string]any{"reasoning_content": ch.Message.ReasoningContent}, nil))
		}
		if text := contentToText(ch.Message.Content); text != "" {
			add(withChoice(base, ch.Index, map[string]any{"content": text}, nil))
		}
		for i, tc := range ch.Message.ToolCalls {
			delta := make(map[string]any, len(tc)+1)
			for k, v := range tc {
				delta[k] = v
			}
			delta["index"] = i
			add(withChoice(base, ch.Index, map[string]any{"tool_calls": []any{delta}}, nil))
		}
		add(withChoice(base, ch.Index, map[string]any{}, ch.FinishReason))
	}
	if wantsUsage && c.Usage != nil {
		u := make(map[string]any, len(base)+2)
		for k, v := range base {
			u[k] = v
		}
		u["choices"] = []any{}
		u["usage"] = c.Usage
		add(u)
	}
	return append(frames, Frame{Data: []byte("[DONE]")})
}

// bufferedResponsesFrames sends a finished Responses object as the event
// sequence a stream of it would have had, keeping its own ids.
func bufferedResponsesFrames(body []byte) ([]Frame, *responsesEmitter) {
	var obj map[string]any
	_ = json.Unmarshal(body, &obj)
	model, _ := obj["model"].(string)
	em := newResponsesEmitter(model)
	if id, ok := obj["id"].(string); ok && id != "" {
		em.respID = id
	}
	if created, ok := obj["created_at"].(float64); ok {
		em.created = int64(created)
	}
	frames := em.start()
	output, _ := obj["output"].([]any)
	for i, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := item["id"].(string)
		switch item["type"] {
		case "reasoning":
			if text := responsesSummaryText(item); text != "" {
				frames = append(frames, em.closeAll()...)
				em.nextID = id
				frames = append(frames, em.reasoning(text)...)
			}
		case "message":
			if text := responsesMessageText(item); text != "" {
				frames = append(frames, em.closeAll()...)
				em.nextID = id
				frames = append(frames, em.text(text)...)
			}
		case "function_call":
			em.nextID = id
			frames = append(frames, em.toolCall(i, asString(item, "call_id"), asString(item, "name"),
				asString(item, "arguments"), thoughtSignatureFrom(item))...)
		default:
			em.nextID = id
			frames = append(frames, em.item(item)...)
		}
	}
	if obj == nil {
		obj = em.inProgress()
	}
	return append(frames, em.finish(obj)...), em
}

func responsesSummaryText(item map[string]any) string {
	summary, _ := item["summary"].([]any)
	var texts []string
	for _, s := range summary {
		if m, ok := s.(map[string]any); ok && asString(m, "text") != "" {
			texts = append(texts, asString(m, "text"))
		}
	}
	return strings.Join(texts, "\n\n")
}

func responsesMessageText(item map[string]any) string {
	content, _ := item["content"].([]any)
	var b strings.Builder
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			b.WriteString(asString(m, "text"))
			b.WriteString(asString(m, "refusal"))
		}
	}
	return b.String()
}
