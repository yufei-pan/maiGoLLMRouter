package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// openAIChatStream streams an OpenAI-kind Chat Completions call. The chunks go
// to the client as the downstream wrote them; usage is always asked for and,
// unless forwardUsage, kept out of what the client sees.
func openAIChatStream(ctx context.Context, client *http.Client, baseURL, apiKey string, timeout time.Duration, req Request, forwardUsage bool) (*Stream, *Response, error) {
	out := mustJSON(openAIStreamBody(req))
	url := baseURL + "/chat/completions"
	conn, err := openStream(ctx, client, url, map[string]string{"Authorization": "Bearer " + apiKey}, out, timeout)
	if err != nil {
		return nil, &Response{OutboundURL: url, OutboundBody: out}, err
	}
	if !conn.isEventStream() {
		raw, err := conn.readAll()
		resp := &Response{HTTPStatus: conn.resp.StatusCode, OutboundURL: url, OutboundBody: out, RawResponse: raw, OpenAIBody: raw}
		if err == nil && resp.OK() {
			resp.FinishReason, resp.HasContent = openAIChatOutcome(raw)
		}
		return nil, resp, err
	}
	src := &openAIChatSource{conn: conn, sse: newSSEReader(conn), acc: newChatAccumulator(req.Model), forwardUsage: forwardUsage}
	return &Stream{HTTPStatus: conn.resp.StatusCode, OutboundURL: url, OutboundBody: out, src: src}, nil, nil
}

// openAIStreamBody copies the inbound body, pins the resolved model and asks
// for a stream. A Chat request also asks for usage, which the log needs.
func openAIStreamBody(req Request) map[string]any {
	body := make(map[string]any, len(req.Body)+2)
	for k, v := range req.Body {
		body[k] = v
	}
	body["model"] = req.Model
	body["stream"] = true
	if req.Op == OpChat {
		so := map[string]any{}
		if old, ok := req.Body["stream_options"].(map[string]any); ok {
			for k, v := range old {
				so[k] = v
			}
		}
		so["include_usage"] = true
		body["stream_options"] = so
	}
	return body
}

// openAIChatSource passes an OpenAI-kind chunk stream through to the client.
type openAIChatSource struct {
	conn         *streamConn
	sse          *sseReader
	acc          *chatAccumulator
	forwardUsage bool
	finished     bool // a finish_reason arrived
	done         bool // [DONE] was handed out
}

func (s *openAIChatSource) next() (Frame, error) {
	if s.done {
		return Frame{}, io.EOF
	}
	for {
		ev, err := s.sse.Next()
		if err == io.EOF {
			s.conn.close()
			if s.finished {
				// Some servers close after the finish without [DONE]: still a clean end.
				s.done = true
				return Frame{Data: []byte("[DONE]")}, nil
			}
			return Frame{}, &StreamError{
				Status: 502,
				Body:   upstreamErrorBody("the provider's stream ended before the reply finished"),
				Err:    errors.New("stream ended before the reply finished"),
			}
		}
		if err != nil {
			s.conn.close()
			return Frame{}, s.conn.failure(err)
		}
		if ev.Data == "[DONE]" {
			s.conn.close()
			s.done = true
			return Frame{Data: []byte("[DONE]")}, nil
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(ev.Data), &chunk) != nil {
			continue // not JSON: neither forwarded nor fatal
		}
		if obj, ok := chunk["error"].(map[string]any); ok {
			s.conn.close()
			return Frame{}, streamErrorFromObject(obj, &Frame{Data: []byte(ev.Data)})
		}
		if s.acc.add(chunk) {
			s.conn.timer.outputStarted()
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			if cm, ok := c.(map[string]any); ok && asString(cm, "finish_reason") != "" {
				s.finished = true
			}
		}
		data := []byte(ev.Data)
		if _, hasUsage := chunk["usage"].(map[string]any); hasUsage && !s.forwardUsage {
			if len(choices) == 0 {
				continue // a usage-only chunk the client didn't ask for
			}
			delete(chunk, "usage")
			data = mustJSON(chunk)
		}
		return Frame{Data: data}, nil
	}
}

func (s *openAIChatSource) state() StreamState {
	st := s.acc.state()
	st.Raw = s.conn.raw.b
	return st
}

func (s *openAIChatSource) errorFrame(status int, message string) Frame {
	return chatErrorFrame(status, message)
}

func (s *openAIChatSource) close() { s.conn.close() }

// openAIResponsesStream streams an OpenAI-kind Responses call: natively on
// /responses (forced, or probed), or through Chat translation when the
// provider has no such route.
func openAIResponsesStream(ctx context.Context, client *http.Client, baseURL, apiKey string, timeout time.Duration, req Request) (*Stream, *Response, error) {
	if req.ResponsesMode == ResponsesModeChatOnly {
		return openAIResponsesViaChatStream(ctx, client, baseURL, apiKey, timeout, req, false)
	}
	out := mustJSON(openAIStreamBody(req))
	url := baseURL + "/responses"
	conn, err := openStream(ctx, client, url, map[string]string{"Authorization": "Bearer " + apiKey}, out, timeout)
	if err != nil {
		return nil, &Response{OutboundURL: url, OutboundBody: out}, err
	}
	if conn.isEventStream() {
		src := &responsesPassSource{conn: conn, sse: newSSEReader(conn), lastSeq: -1}
		return &Stream{HTTPStatus: conn.resp.StatusCode, OutboundURL: url, OutboundBody: out, src: src}, nil, nil
	}
	raw, err := conn.readAll()
	resp := &Response{HTTPStatus: conn.resp.StatusCode, OutboundURL: url, OutboundBody: out, RawResponse: raw, OpenAIBody: raw}
	if err != nil {
		return nil, resp, err
	}
	if resp.OK() {
		resp.FinishReason, resp.HasContent = responsesOutcome(raw)
		return nil, resp, nil
	}
	// Only a probe downgrades to chat, as on the buffered path.
	if req.ResponsesMode == ResponsesModeProbe && isResponsesUnsupported(resp.HTTPStatus, raw) {
		s, chatResp, err := openAIResponsesViaChatStream(ctx, client, baseURL, apiKey, timeout, req, true)
		if chatResp != nil && chatResp.Incompatible {
			chatResp.OutboundURL = resp.OutboundURL
			chatResp.OutboundBody = resp.OutboundBody
			chatResp.RawResponse = resp.RawResponse
		}
		return s, chatResp, err
	}
	return nil, resp, nil
}

// openAIResponsesViaChatStream serves a Responses request over the provider's
// Chat Completions stream, re-emitted as Responses events. learnedChatOnly
// records that a probe already proved the /responses route absent.
func openAIResponsesViaChatStream(ctx context.Context, client *http.Client, baseURL, apiKey string, timeout time.Duration, req Request, learnedChatOnly bool) (*Stream, *Response, error) {
	chatBody, err := ResponsesToChat(req.Body, req.Model)
	if err != nil {
		return nil, &Response{Incompatible: true, LearnChatOnly: learnedChatOnly}, nil
	}
	chatReq := req
	chatReq.Op, chatReq.Body = OpChat, chatBody
	s, resp, err := openAIChatStream(ctx, client, baseURL, apiKey, timeout, chatReq, true)
	if s == nil {
		if resp != nil {
			resp.LearnChatOnly = learnedChatOnly
			if err == nil && resp.OK() {
				wrapChatReplyAsResponses(resp, req.Model)
			}
		}
		return nil, resp, err
	}
	s.LearnChatOnly = learnedChatOnly
	return chatToResponses(s, req.Model), nil, nil
}

// wrapChatReplyAsResponses turns a buffered Chat reply into a Responses body.
// The reply is still checked as the Chat call it was, so FinishReason and
// HasContent stay; a reply that cannot be wrapped is never sent as Chat.
func wrapChatReplyAsResponses(resp *Response, model string) {
	wrapped, err := ChatToResponses(resp.OpenAIBody, model, resp.FinishReason)
	if err != nil {
		resp.OpenAIBody = nil
		resp.FinishReason, resp.HasContent = "", false
		return
	}
	resp.OpenAIBody = wrapped
}

// responsesPassSource passes an OpenAI-kind Responses event stream through.
type responsesPassSource struct {
	conn    *streamConn
	sse     *sseReader
	output  bool
	final   map[string]any
	lastSeq int
	done    bool
}

func (s *responsesPassSource) next() (Frame, error) {
	if s.done {
		return Frame{}, io.EOF
	}
	for {
		ev, err := s.sse.Next()
		if err == io.EOF {
			s.conn.close()
			return Frame{}, &StreamError{
				Status: 502,
				Body:   upstreamErrorBody("the provider's stream ended before the response completed"),
				Err:    errors.New("stream ended before the response completed"),
			}
		}
		if err != nil {
			s.conn.close()
			return Frame{}, s.conn.failure(err)
		}
		var m map[string]any
		if json.Unmarshal([]byte(ev.Data), &m) != nil {
			continue // not JSON (or a stray [DONE]): neither forwarded nor fatal
		}
		typ := asString(m, "type")
		if typ == "" {
			typ = ev.Event
		}
		name := ev.Event
		if name == "" {
			name = typ
		}
		f := Frame{Event: name, Data: []byte(ev.Data)}
		if seq, ok := m["sequence_number"].(float64); ok {
			s.lastSeq = int(seq)
		}
		switch typ {
		case "error":
			s.conn.close()
			obj, ok := m["error"].(map[string]any)
			if !ok {
				obj = make(map[string]any, len(m))
				for k, v := range m {
					if k != "type" && k != "sequence_number" {
						obj[k] = v
					}
				}
			}
			return Frame{}, streamErrorFromObject(obj, &f)
		case "response.failed":
			s.conn.close()
			resp, _ := m["response"].(map[string]any)
			obj, ok := resp["error"].(map[string]any)
			if !ok {
				obj = map[string]any{"message": "the response failed"}
			}
			return Frame{}, streamErrorFromObject(obj, &f)
		case "response.completed", "response.incomplete":
			s.final, _ = m["response"].(map[string]any)
			s.done = true
			s.conn.close()
			return f, nil
		}
		if isResponsesOutputEvent(typ, m) && !s.output {
			s.output = true
			s.conn.timer.outputStarted()
		}
		return f, nil
	}
}

// isResponsesOutputEvent reports an event that carries real output: any
// delta, an added item other than an empty message or reasoning, or any
// finished item.
func isResponsesOutputEvent(typ string, m map[string]any) bool {
	switch {
	case strings.HasSuffix(typ, ".delta"), typ == "response.output_item.done":
		return true
	case typ == "response.output_item.added":
		item, _ := m["item"].(map[string]any)
		t := asString(item, "type")
		return t != "message" && t != "reasoning"
	}
	return false
}

func (s *responsesPassSource) state() StreamState {
	st := StreamState{Output: s.output, Raw: s.conn.raw.b}
	if s.final == nil {
		st.Body = []byte(`{"object":"response","status":"in_progress","output":[]}`)
		return st
	}
	st.Body = mustJSON(s.final)
	st.FinishReason, st.HasContent = responsesOutcome(st.Body)
	st.Prohibited = isProhibitedContent(st.Body)
	return st
}

func (s *responsesPassSource) errorFrame(status int, message string) Frame {
	s.lastSeq++
	return responsesErrorFrame(s.lastSeq, message)
}

func (s *responsesPassSource) close() { s.conn.close() }
