package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
