package provider

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// responsesEmitter writes the Responses streaming event sequence for a reply
// that arrives piece by piece: reasoning, text and tool calls. Each event
// carries its type (also its SSE event name) and the next sequence_number.
type responsesEmitter struct {
	model   string
	respID  string
	created int64
	nextSeq int
	items   []*emittedItem // in output_index order
	current *emittedItem   // the open message or reasoning item, if any
	calls   map[int]*emittedItem
}

type emittedItem struct {
	kind   string // "message", "reasoning" or "function_call"
	id     string
	index  int
	text   strings.Builder // message text, reasoning summary, or call arguments
	callID    string
	name      string
	signature string // a Gemini thought signature carried on the call
	closed    bool
}

func newResponsesEmitter(model string) *responsesEmitter {
	return &responsesEmitter{
		model:   model,
		respID:  "resp_mai_" + randomID(),
		created: time.Now().Unix(),
		calls:   make(map[int]*emittedItem),
	}
}

// seq returns the next sequence_number and uses it up.
func (e *responsesEmitter) seq() int {
	n := e.nextSeq
	e.nextSeq++
	return n
}

func (e *responsesEmitter) event(typ string, fields map[string]any) Frame {
	fields["type"] = typ
	fields["sequence_number"] = e.seq()
	return Frame{Event: typ, Data: mustJSON(fields)}
}

func (e *responsesEmitter) inProgress() map[string]any {
	return map[string]any{
		"id": e.respID, "object": "response", "created_at": e.created,
		"status": "in_progress", "model": e.model, "output": []any{},
	}
}

// start opens the response.
func (e *responsesEmitter) start() []Frame {
	return []Frame{
		e.event("response.created", map[string]any{"response": e.inProgress()}),
		e.event("response.in_progress", map[string]any{"response": e.inProgress()}),
	}
}

func (e *responsesEmitter) open(kind, prefix string) *emittedItem {
	it := &emittedItem{kind: kind, id: prefix + randomID(), index: len(e.items)}
	e.items = append(e.items, it)
	return it
}

// closeAll closes every open item, in output order.
func (e *responsesEmitter) closeAll() []Frame {
	var out []Frame
	for _, it := range e.items {
		out = append(out, e.closeItem(it)...)
	}
	e.current = nil
	return out
}

// reasoning adds to the open reasoning summary, opening one if needed.
func (e *responsesEmitter) reasoning(delta string) []Frame {
	var out []Frame
	if e.current == nil || e.current.kind != "reasoning" {
		out = append(out, e.closeAll()...)
		it := e.open("reasoning", "rs_")
		e.current = it
		out = append(out,
			e.event("response.output_item.added", map[string]any{
				"output_index": it.index, "item": map[string]any{"id": it.id, "type": "reasoning", "summary": []any{}},
			}),
			e.event("response.reasoning_summary_part.added", map[string]any{
				"item_id": it.id, "output_index": it.index, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": ""},
			}))
	}
	it := e.current
	it.text.WriteString(delta)
	return append(out, e.event("response.reasoning_summary_text.delta", map[string]any{
		"item_id": it.id, "output_index": it.index, "summary_index": 0, "delta": delta,
	}))
}

// text adds to the open assistant message, opening one if needed.
func (e *responsesEmitter) text(delta string) []Frame {
	var out []Frame
	if e.current == nil || e.current.kind != "message" {
		out = append(out, e.closeAll()...)
		it := e.open("message", "msg_")
		e.current = it
		out = append(out,
			e.event("response.output_item.added", map[string]any{
				"output_index": it.index,
				"item":         map[string]any{"id": it.id, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
			}),
			e.event("response.content_part.added", map[string]any{
				"item_id": it.id, "output_index": it.index, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			}))
	}
	it := e.current
	it.text.WriteString(delta)
	return append(out, e.event("response.output_text.delta", map[string]any{
		"item_id": it.id, "output_index": it.index, "content_index": 0, "delta": delta,
	}))
}

// toolCall adds a piece of the tool call at Chat index, opening its item the
// first time the index is seen. A signature, when given, is kept for the
// call's done event.
func (e *responsesEmitter) toolCall(index int, callID, name, argsDelta, signature string) []Frame {
	var out []Frame
	it, ok := e.calls[index]
	if !ok {
		if e.current != nil {
			out = append(out, e.closeItem(e.current)...)
			e.current = nil
		}
		it = e.open("function_call", "fc_")
		it.callID, it.name = callID, name
		e.calls[index] = it
		out = append(out, e.event("response.output_item.added", map[string]any{
			"output_index": it.index,
			"item": map[string]any{
				"id": it.id, "type": "function_call", "call_id": callID, "name": name,
				"arguments": "", "status": "in_progress",
			},
		}))
	}
	if signature != "" {
		it.signature = signature
	}
	if argsDelta != "" && !it.closed {
		it.text.WriteString(argsDelta)
		out = append(out, e.event("response.function_call_arguments.delta", map[string]any{
			"item_id": it.id, "output_index": it.index, "delta": argsDelta,
		}))
	}
	return out
}

func (e *responsesEmitter) closeItem(it *emittedItem) []Frame {
	if it.closed {
		return nil
	}
	it.closed = true
	text := it.text.String()
	switch it.kind {
	case "reasoning":
		part := map[string]any{"type": "summary_text", "text": text}
		return []Frame{
			e.event("response.reasoning_summary_text.done", map[string]any{
				"item_id": it.id, "output_index": it.index, "summary_index": 0, "text": text,
			}),
			e.event("response.reasoning_summary_part.done", map[string]any{
				"item_id": it.id, "output_index": it.index, "summary_index": 0, "part": part,
			}),
			e.event("response.output_item.done", map[string]any{
				"output_index": it.index,
				"item":         map[string]any{"id": it.id, "type": "reasoning", "summary": []any{part}},
			}),
		}
	case "message":
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		return []Frame{
			e.event("response.output_text.done", map[string]any{
				"item_id": it.id, "output_index": it.index, "content_index": 0, "text": text,
			}),
			e.event("response.content_part.done", map[string]any{
				"item_id": it.id, "output_index": it.index, "content_index": 0, "part": part,
			}),
			e.event("response.output_item.done", map[string]any{
				"output_index": it.index,
				"item": map[string]any{
					"id": it.id, "type": "message", "role": "assistant", "status": "completed", "content": []any{part},
				},
			}),
		}
	default: // function_call
		item := map[string]any{
			"id": it.id, "type": "function_call", "call_id": it.callID, "name": it.name,
			"arguments": text, "status": "completed",
		}
		if it.signature != "" {
			item["thought_signature"] = it.signature
		}
		return []Frame{
			e.event("response.function_call_arguments.done", map[string]any{
				"item_id": it.id, "output_index": it.index, "arguments": text,
			}),
			e.event("response.output_item.done", map[string]any{"output_index": it.index, "item": item}),
		}
	}
}

// finish closes the open items and ends the response with final: completed,
// or incomplete when final says so. The streamed item ids are written into
// final's output items.
func (e *responsesEmitter) finish(final map[string]any) []Frame {
	out := e.closeAll()
	e.stampIDs(final)
	final["id"] = e.respID
	final["created_at"] = e.created
	typ := "response.completed"
	if final["status"] == "incomplete" {
		typ = "response.incomplete"
	}
	return append(out, e.event(typ, map[string]any{"response": final}))
}

// stampIDs gives final's output items the ids their streamed items had:
// function calls by call_id, messages and reasoning in order of appearance.
func (e *responsesEmitter) stampIDs(final map[string]any) {
	output, _ := final["output"].([]any)
	used := make(map[*emittedItem]bool)
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := item["type"].(string)
		for _, it := range e.items {
			if used[it] || it.kind != typ {
				continue
			}
			if typ == "function_call" && item["call_id"] != it.callID {
				continue
			}
			used[it] = true
			item["id"] = it.id
			break
		}
	}
}

// responsesFinal is the Responses object for a finished Chat reply: what
// ChatToResponses makes, with the reasoning summary first when there is one,
// and a content_filter finish reported as incomplete.
func responsesFinal(chatBody []byte, model, finish, reasoning string) (map[string]any, error) {
	raw, err := ChatToResponses(chatBody, model, finish)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if reasoning != "" {
		output, _ := obj["output"].([]any)
		item := map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": reasoning}}}
		obj["output"] = append([]any{item}, output...)
	}
	if strings.EqualFold(finish, "content_filter") {
		obj["status"] = "incomplete"
		obj["incomplete_details"] = map[string]any{"reason": "content_filter"}
	}
	return obj, nil
}

// chatToResponses re-emits a Chat Completions stream as Responses events.
func chatToResponses(inner *Stream, model string) *Stream {
	return &Stream{
		HTTPStatus:    inner.HTTPStatus,
		OutboundURL:   inner.OutboundURL,
		OutboundBody:  inner.OutboundBody,
		LearnChatOnly: inner.LearnChatOnly,
		src:           &chatToResponsesSource{inner: inner, model: model, em: newResponsesEmitter(model)},
	}
}

type chatToResponsesSource struct {
	inner   *Stream
	model   string
	em      *responsesEmitter
	queue   []Frame
	started bool
	final   map[string]any
	done    bool
}

func (s *chatToResponsesSource) next() (Frame, error) {
	for {
		if len(s.queue) > 0 {
			f := s.queue[0]
			s.queue = s.queue[1:]
			return f, nil
		}
		if s.done {
			return Frame{}, io.EOF
		}
		if !s.started {
			s.started = true
			s.queue = s.em.start()
			continue
		}
		f, err := s.inner.Next()
		if err == io.EOF || (err == nil && string(f.Data) == "[DONE]") {
			if err := s.end(); err != nil {
				return Frame{}, err
			}
			continue
		}
		if err != nil {
			return Frame{}, s.translateError(err)
		}
		var chunk map[string]any
		if json.Unmarshal(f.Data, &chunk) != nil {
			continue
		}
		s.queue = s.translate(chunk)
	}
}

// translate turns one Chat chunk's first choice into Responses events.
func (s *chatToResponsesSource) translate(chunk map[string]any) []Frame {
	choices, _ := chunk["choices"].([]any)
	var out []Frame
	for _, raw := range choices {
		ch, ok := raw.(map[string]any)
		if !ok || indexOf(ch) != 0 {
			continue
		}
		delta, _ := ch["delta"].(map[string]any)
		for _, k := range []string{"reasoning_content", "reasoning"} {
			if r := asString(delta, k); r != "" {
				out = append(out, s.em.reasoning(r)...)
			}
		}
		for _, k := range []string{"content", "refusal"} {
			if t := asString(delta, k); t != "" {
				out = append(out, s.em.text(t)...)
			}
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, rawTC := range tcs {
			tc, ok := rawTC.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			out = append(out, s.em.toolCall(indexOf(tc), asString(tc, "id"), asString(fn, "name"), asString(fn, "arguments"), thoughtSignatureFrom(tc))...)
		}
	}
	return out
}

// end builds the final object from the finished Chat reply and queues the
// closing events.
func (s *chatToResponsesSource) end() error {
	st := s.inner.State()
	final, err := responsesFinal(st.Body, s.model, st.FinishReason, chatReasoning(st.Body))
	if err != nil {
		return &StreamError{Status: 502, Body: upstreamErrorBody("the provider's reply could not be translated"), Err: err}
	}
	s.final = final
	s.queue = s.em.finish(final)
	s.done = true
	return nil
}

// translateError carries a Chat stream's failure over as a Responses error event.
func (s *chatToResponsesSource) translateError(err error) error {
	var se *StreamError
	if !errors.As(err, &se) {
		return err
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(se.Body, &body)
	msg := body.Error.Message
	if msg == "" {
		msg = se.Error()
	}
	code, ok := body.Error.Code.(string)
	if !ok || code == "" {
		code = "server_error"
	}
	f := Frame{Event: "error", Data: mustJSON(map[string]any{
		"type": "error", "code": code, "message": msg, "sequence_number": s.em.seq(),
	})}
	out := *se
	out.Frame = &f
	return &out
}

func (s *chatToResponsesSource) state() StreamState {
	in := s.inner.State()
	out := StreamState{
		Output: in.Output, FinishReason: in.FinishReason, HasContent: in.HasContent,
		Prohibited: in.Prohibited, Raw: in.Raw,
	}
	if s.final != nil {
		out.Body = mustJSON(s.final)
	} else if partial, err := responsesFinal(in.Body, s.model, in.FinishReason, chatReasoning(in.Body)); err == nil {
		partial["status"] = "in_progress"
		out.Body = mustJSON(partial)
	}
	return out
}

func (s *chatToResponsesSource) errorFrame(status int, message string) Frame {
	return responsesErrorFrame(s.em.seq(), message)
}

func (s *chatToResponsesSource) close() { s.inner.Close() }

// chatReasoning is the reasoning text of a chat.completion's first choice.
func chatReasoning(chatBody []byte) string {
	var parsed struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(chatBody, &parsed) != nil || len(parsed.Choices) == 0 {
		return ""
	}
	return parsed.Choices[0].Message.ReasoningContent
}
