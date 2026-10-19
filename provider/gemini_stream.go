package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// geminiChatStream streams a Gemini chat call (streamGenerateContent with
// alt=sse) and translates it into Chat Completions chunks. Gemini sends each
// function call whole, so a tool call arrives as one delta.
func geminiChatStream(ctx context.Context, client *http.Client, baseURL, apiKey string, timeout time.Duration, req Request, forwardUsage bool) (*Stream, *Response, error) {
	out := mustJSON(buildGeminiChatBody(req))
	url := baseURL + "/" + geminiModelPath(req.Model) + ":streamGenerateContent?alt=sse"
	conn, err := openStream(ctx, client, url, map[string]string{"x-goog-api-key": apiKey}, out, timeout)
	if err != nil {
		return nil, &Response{OutboundURL: url, OutboundBody: out}, err
	}
	if !conn.isEventStream() {
		raw, err := conn.readAll()
		resp := &Response{HTTPStatus: conn.resp.StatusCode, OutboundURL: url, OutboundBody: out, RawResponse: raw, OpenAIBody: raw}
		if err == nil && resp.OK() {
			geminiJSONReply(resp, req)
		}
		return nil, resp, err
	}
	src := &geminiChatSource{
		conn: conn, sse: newSSEReader(conn), tr: newGeminiStreamTranslator(req),
		acc: newChatAccumulator(req.Model), forwardUsage: forwardUsage,
	}
	return &Stream{HTTPStatus: conn.resp.StatusCode, OutboundURL: url, OutboundBody: out, src: src}, nil, nil
}

// geminiJSONReply judges a 2xx reply that came back as JSON instead of a
// stream: one GenerateContentResponse, or an array of them.
func geminiJSONReply(resp *Response, req Request) {
	var arr []json.RawMessage
	if json.Unmarshal(resp.RawResponse, &arr) != nil {
		body, finish, hasContent := geminiToOpenAIChat(resp.RawResponse, req.Model)
		rememberGeminiSignatures(body, req)
		resp.OpenAIBody, resp.FinishReason, resp.HasContent = body, finish, hasContent
		return
	}
	tr := newGeminiStreamTranslator(req)
	acc := newChatAccumulator(req.Model)
	for _, item := range arr {
		chunks, _ := tr.translate(item)
		for _, c := range chunks {
			acc.add(c)
		}
	}
	for _, c := range tr.final(true) {
		acc.add(c)
	}
	st := acc.state()
	resp.OpenAIBody, resp.FinishReason, resp.HasContent = st.Body, st.FinishReason, st.HasContent
}

// geminiStreamTranslator turns streamed GenerateContentResponse objects into
// Chat Completions chunks for one reply.
type geminiStreamTranslator struct {
	req       Request
	id        string
	created   int64
	sentRole  bool
	toolCount map[int]int // tool calls sent so far, per candidate
	finished  bool        // a finish reason (or a prompt block) arrived
	usage     map[string]any
}

func newGeminiStreamTranslator(req Request) *geminiStreamTranslator {
	return &geminiStreamTranslator{
		req: req, id: "chatcmpl-" + randomID(), created: time.Now().Unix(), toolCount: make(map[int]int),
	}
}

func (t *geminiStreamTranslator) chunk(ci int, delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "created": t.created, "model": t.req.Model,
		"choices": []any{map[string]any{"index": ci, "delta": delta, "finish_reason": finish}},
	}
}

// translate returns the Chat chunks for one GenerateContentResponse, or the
// error it reports.
func (t *geminiStreamTranslator) translate(raw []byte) ([]map[string]any, *StreamError) {
	var g struct {
		Candidates []struct {
			Index   *int `json:"index"`
			Content struct {
				Parts []struct {
					Text                  string `json:"text"`
					Thought               bool   `json:"thought"`
					ThoughtSignature      string `json:"thoughtSignature"`
					ThoughtSignatureSnake string `json:"thought_signature"`
					FunctionCall          *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		UsageMetadata *struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
		Error map[string]any `json:"error"`
	}
	if json.Unmarshal(raw, &g) != nil {
		return nil, nil
	}
	if g.Error != nil {
		frame := Frame{Data: mustJSON(map[string]any{"error": g.Error})}
		return nil, streamErrorFromObject(g.Error, &frame)
	}
	if u := g.UsageMetadata; u != nil {
		t.usage = map[string]any{
			"prompt_tokens": u.PromptTokenCount, "completion_tokens": u.CandidatesTokenCount, "total_tokens": u.TotalTokenCount,
		}
	}
	var out []map[string]any
	if !t.sentRole {
		t.sentRole = true
		out = append(out, t.chunk(0, map[string]any{"role": "assistant", "content": ""}, nil))
	}
	if g.PromptFeedback.BlockReason != "" {
		t.finished = true
		return append(out, t.chunk(0, map[string]any{}, "content_filter")), nil
	}
	for i, c := range g.Candidates {
		ci := i
		if c.Index != nil {
			ci = *c.Index
		}
		for _, p := range c.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				args := p.FunctionCall.Args
				if args == nil {
					args = map[string]any{}
				}
				id := "call_" + randomID()
				tc := map[string]any{
					"index": t.toolCount[ci], "id": id, "type": "function",
					"function": map[string]any{"name": p.FunctionCall.Name, "arguments": string(mustJSON(args))},
				}
				sig := p.ThoughtSignature
				if sig == "" {
					sig = p.ThoughtSignatureSnake
				}
				if sig != "" {
					tc["thought_signature"] = sig
					t.req.Signatures.Put(id, t.req.signer(), sig)
				}
				t.toolCount[ci]++
				out = append(out, t.chunk(ci, map[string]any{"tool_calls": []any{tc}}, nil))
			case p.Text == "":
				// Often a part carrying only a signature: nothing to show.
			case p.Thought:
				out = append(out, t.chunk(ci, map[string]any{"reasoning_content": p.Text}, nil))
			default:
				out = append(out, t.chunk(ci, map[string]any{"content": p.Text}, nil))
			}
		}
		if c.FinishReason != "" {
			fr := normalizeGeminiFinish(c.FinishReason)
			if t.toolCount[ci] > 0 {
				fr = "tool_calls" // OpenAI clients key off this to read tool_calls
			}
			t.finished = true
			out = append(out, t.chunk(ci, map[string]any{}, fr))
		}
	}
	return out, nil
}

// final returns the usage chunk, when asked for and known.
func (t *geminiStreamTranslator) final(forwardUsage bool) []map[string]any {
	if !forwardUsage || t.usage == nil {
		return nil
	}
	return []map[string]any{{
		"id": t.id, "object": "chat.completion.chunk", "created": t.created, "model": t.req.Model,
		"choices": []any{}, "usage": t.usage,
	}}
}

// geminiChatSource reads a Gemini SSE stream as Chat chunks.
type geminiChatSource struct {
	conn         *streamConn
	sse          *sseReader
	tr           *geminiStreamTranslator
	acc          *chatAccumulator
	forwardUsage bool
	queue        []Frame
	done         bool
}

func (s *geminiChatSource) next() (Frame, error) {
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
			// Usage always goes into the log; to the client only when asked.
			for _, c := range s.tr.final(true) {
				s.acc.add(c)
				if s.forwardUsage {
					s.queue = append(s.queue, Frame{Data: mustJSON(c)})
				}
			}
			s.queue = append(s.queue, Frame{Data: []byte("[DONE]")})
			s.done = true
			continue
		}
		if err != nil {
			s.conn.close()
			return Frame{}, s.conn.failure(err)
		}
		chunks, se := s.tr.translate([]byte(ev.Data))
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
	}
}

func (s *geminiChatSource) state() StreamState {
	st := s.acc.state()
	st.Raw = s.conn.raw.b
	return st
}

func (s *geminiChatSource) errorFrame(status int, message string) Frame {
	return chatErrorFrame(status, message)
}

func (s *geminiChatSource) close() { s.conn.close() }
