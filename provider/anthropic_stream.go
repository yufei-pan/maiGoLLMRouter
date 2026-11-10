package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// anthropicChatStream streams a Messages call ("stream": true) and translates
// its events into Chat Completions chunks as they arrive: text, thinking (as
// reasoning_content) and each tool call's arguments piece by piece.
func anthropicChatStream(ctx context.Context, client *http.Client, baseURL, apiKey string, timeout time.Duration, req Request, forwardUsage bool) (*Stream, *Response, error) {
	body := buildAnthropicBody(req)
	body["stream"] = true
	out := mustJSON(body)
	url := baseURL + "/messages"
	conn, err := openStream(ctx, client, url, anthropicHeaders(apiKey), out, timeout)
	if err != nil {
		return nil, &Response{OutboundURL: url, OutboundBody: out}, err
	}
	if !conn.isEventStream() {
		raw, err := conn.readAll()
		resp := &Response{HTTPStatus: conn.resp.StatusCode, OutboundURL: url, OutboundBody: out, RawResponse: raw, OpenAIBody: raw}
		if err == nil && resp.OK() {
			anthropicJSONReply(resp, req.Model)
		}
		return nil, resp, err
	}
	src := &anthropicChatSource{
		conn: conn, sse: newSSEReader(conn), tr: newAnthropicStreamTranslator(req.Model),
		acc: newChatAccumulator(req.Model), forwardUsage: forwardUsage,
	}
	return &Stream{HTTPStatus: conn.resp.StatusCode, OutboundURL: url, OutboundBody: out, src: src}, nil, nil
}

// anthropicStreamTranslator turns the events of one streamed Messages reply
// into Chat Completions chunks.
type anthropicStreamTranslator struct {
	model    string
	id       string
	created  int64
	blocks   map[int]*anthropicBlock // content blocks by Anthropic's index
	tools    int                     // tool_use blocks so far: the next one's Chat index
	finished bool                    // a stop_reason arrived
	usage    *anthropicUsage         // nil until message_start
}

// anthropicBlock is one content block of the reply.
type anthropicBlock struct {
	typ  string // "text", "thinking", "tool_use", or a type the client is not shown
	tool int    // tool_use: its Chat tool-call index
	args bool   // tool_use: some of its arguments were sent
}

func newAnthropicStreamTranslator(model string) *anthropicStreamTranslator {
	return &anthropicStreamTranslator{
		model: model, id: "chatcmpl-" + randomID(), created: time.Now().Unix(), blocks: make(map[int]*anthropicBlock),
	}
}

func (t *anthropicStreamTranslator) chunk(delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "created": t.created, "model": t.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

func (t *anthropicStreamTranslator) toolChunk(call map[string]any) map[string]any {
	return t.chunk(map[string]any{"tool_calls": []any{call}}, nil)
}

// translate returns the Chat chunks for one event, whether the reply ended
// (message_stop), or the failure an error event reports. ping, signatures and
// event or block types the client has no field for give nothing.
func (t *anthropicStreamTranslator) translate(event string, data []byte) (out []map[string]any, end bool, se *StreamError) {
	var e struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		ContentBlock struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
			ID       string `json:"id"`
			Name     string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage json.RawMessage `json:"usage"`
		Error map[string]any  `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return nil, false, nil // not JSON: neither forwarded nor fatal
	}
	typ := e.Type
	if typ == "" {
		typ = event
	}
	switch typ {
	case "message_start":
		t.usage = &anthropicUsage{}
		t.addUsage(e.Message.Usage)
		out = append(out, t.chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	case "content_block_start":
		b := &anthropicBlock{typ: e.ContentBlock.Type}
		t.blocks[e.Index] = b
		switch {
		case b.typ == "text" && e.ContentBlock.Text != "":
			out = append(out, t.chunk(map[string]any{"content": e.ContentBlock.Text}, nil))
		case b.typ == "thinking" && e.ContentBlock.Thinking != "":
			out = append(out, t.chunk(map[string]any{"reasoning_content": e.ContentBlock.Thinking}, nil))
		case b.typ == "tool_use":
			b.tool = t.tools
			t.tools++
			out = append(out, t.toolChunk(map[string]any{
				"index": b.tool, "id": e.ContentBlock.ID, "type": "function",
				"function": map[string]any{"name": e.ContentBlock.Name, "arguments": ""},
			}))
		}
	case "content_block_delta":
		switch d := e.Delta; {
		case d.Type == "text_delta" && d.Text != "":
			out = append(out, t.chunk(map[string]any{"content": d.Text}, nil))
		case d.Type == "thinking_delta" && d.Thinking != "":
			out = append(out, t.chunk(map[string]any{"reasoning_content": d.Thinking}, nil))
		case d.Type == "input_json_delta" && d.PartialJSON != "":
			// A server tool's input (web search) arrives this way too: not the client's call.
			if b := t.blocks[e.Index]; b != nil && b.typ == "tool_use" {
				b.args = true
				out = append(out, t.toolChunk(map[string]any{"index": b.tool, "function": map[string]any{"arguments": d.PartialJSON}}))
			}
		}
	case "content_block_stop":
		// A call without arguments still gets a JSON object, as in a whole reply.
		if b := t.blocks[e.Index]; b != nil && b.typ == "tool_use" && !b.args {
			b.args = true
			out = append(out, t.toolChunk(map[string]any{"index": b.tool, "function": map[string]any{"arguments": "{}"}}))
		}
	case "message_delta":
		t.addUsage(e.Usage)
		if e.Delta.StopReason != "" {
			t.finished = true
			out = append(out, t.chunk(map[string]any{}, normalizeAnthropicFinish(e.Delta.StopReason)))
		}
	case "message_stop":
		return nil, true, nil
	case "error":
		return nil, false, anthropicStreamError(e.Error)
	}
	return out, false, nil
}

// addUsage folds in a usage object. message_delta's counts are totals so far,
// so each field it carries replaces the one message_start gave.
func (t *anthropicStreamTranslator) addUsage(raw json.RawMessage) {
	if t.usage != nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, t.usage)
	}
}

// usageChunk is the reply's usage, once message_start has given it.
func (t *anthropicStreamTranslator) usageChunk() map[string]any {
	if t.usage == nil {
		return nil
	}
	return map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "created": t.created, "model": t.model,
		"choices": []any{}, "usage": t.usage.openAI(),
	}
}

// anthropicErrorStatus is the HTTP status Anthropic gives each error type
// when it can still answer with one.
var anthropicErrorStatus = map[string]int{
	"invalid_request_error": 400,
	"authentication_error":  401,
	"billing_error":         402,
	"permission_error":      403,
	"not_found_error":       404,
	"request_too_large":     413,
	"rate_limit_error":      429,
	"api_error":             500,
	"overloaded_error":      529,
}

// anthropicStreamError turns an error event's object ({"type":
// "overloaded_error", "message": "Overloaded"}) into the stream's failure, with
// the status the same error has before a stream starts, so the router judges
// both alike; the client's error event carries the object with that code.
func anthropicStreamError(obj map[string]any) *StreamError {
	if obj == nil {
		obj = map[string]any{}
	}
	if status, ok := anthropicErrorStatus[asString(obj, "type")]; ok {
		obj["code"] = status
	}
	frame := Frame{Data: mustJSON(map[string]any{"error": obj})}
	return streamErrorFromObject(obj, &frame)
}

// anthropicChatSource reads a Messages event stream as Chat chunks.
type anthropicChatSource struct {
	conn         *streamConn
	sse          *sseReader
	tr           *anthropicStreamTranslator
	acc          *chatAccumulator
	forwardUsage bool
	queue        []Frame
	done         bool
}

func (s *anthropicChatSource) next() (Frame, error) {
	for {
		if len(s.queue) > 0 {
			f := s.queue[0]
			s.queue = s.queue[1:]
			return f, nil
		}
		if s.done {
			return Frame{}, io.EOF
		}
		ev, err := s.sse.Next()
		if err == io.EOF {
			s.conn.close()
			if !s.tr.finished {
				return Frame{}, &StreamError{
					Status: 502,
					Body:   upstreamErrorBody("the provider's stream ended before the reply finished"),
					Err:    errors.New("stream ended before the reply finished"),
				}
			}
			s.end() // a stop_reason came, only message_stop is missing
			continue
		}
		if err != nil {
			s.conn.close()
			return Frame{}, s.conn.failure(err)
		}
		chunks, end, se := s.tr.translate(ev.Event, []byte(ev.Data))
		if se != nil {
			s.conn.close()
			return Frame{}, se
		}
		for _, c := range chunks {
			if s.acc.add(c) {
				s.conn.timer.outputStarted()
			}
			s.queue = append(s.queue, Frame{Data: mustJSON(c)})
		}
		if end {
			s.conn.close()
			s.end()
		}
	}
}

// end queues the usage chunk and [DONE]. Usage always goes into the log; to
// the client only when asked.
func (s *anthropicChatSource) end() {
	if c := s.tr.usageChunk(); c != nil {
		s.acc.add(c)
		if s.forwardUsage {
			s.queue = append(s.queue, Frame{Data: mustJSON(c)})
		}
	}
	s.queue = append(s.queue, Frame{Data: []byte("[DONE]")})
	s.done = true
}

func (s *anthropicChatSource) state() StreamState {
	st := s.acc.state()
	st.Raw = s.conn.raw.b
	return st
}

func (s *anthropicChatSource) errorFrame(status int, message string) Frame {
	return chatErrorFrame(status, message)
}

func (s *anthropicChatSource) close() { s.conn.close() }
