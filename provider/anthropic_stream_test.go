package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Events in the shape Anthropic's Messages API streams them.
const (
	aStart = `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-opus-5-5","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":4,"cache_creation_input_tokens":300,"cache_read_input_tokens":19621,"output_tokens":1}}}`
	aPing  = `{"type":"ping"}`
	aStop  = `{"type":"message_stop"}`
)

func aBlockStart(i int, block string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, i, block)
}

func aTextStart(i int) string { return aBlockStart(i, `{"type":"text","text":""}`) }

func aToolStart(i int, id, name string) string {
	return aBlockStart(i, fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":{}}`, id, name))
}

func aDelta(i int, delta string) string {
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, i, delta)
}

func aText(i int, s string) string {
	return aDelta(i, fmt.Sprintf(`{"type":"text_delta","text":%s}`, mustJSON(s)))
}

func aThinking(i int, s string) string {
	return aDelta(i, fmt.Sprintf(`{"type":"thinking_delta","thinking":%s}`, mustJSON(s)))
}

func aArgs(i int, partial string) string {
	return aDelta(i, fmt.Sprintf(`{"type":"input_json_delta","partial_json":%s}`, mustJSON(partial)))
}

func aBlockStop(i int) string { return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i) }

func aMessageDelta(stop string, output int) string {
	return fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":%d}}`, stop, output)
}

const aOverloaded = `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`

// anthropicSSE writes each event under its own event name, as Anthropic does.
func anthropicSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, e := range events {
		var m struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &m)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.Type, e)
		w.(http.Flusher).Flush()
	}
}

func anthropicStreamCall(t *testing.T, url string, op Operation, body map[string]any, timeout time.Duration) (*Stream, *Response) {
	t.Helper()
	req := Request{Op: op, Model: "claude-opus-5-5", Provider: "claude", Body: body}
	s, resp, err := CallStream(context.Background(), &http.Client{}, "anthropic", url, "k", timeout, req)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	return s, resp
}

type streamedCall struct{ id, name, args string }

// streamedChat is what a client adds up from a Chat chunk stream.
type streamedChat struct {
	role               bool
	content, reasoning string
	calls              []streamedCall // by index
	finish             string
	usage              map[string]any
	usageAt            int // frame position of the usage chunk, -1 when none
	done               bool
}

func collectChat(t *testing.T, frames []Frame) streamedChat {
	t.Helper()
	out := streamedChat{usageAt: -1}
	for i, f := range frames {
		if string(f.Data) == "[DONE]" {
			if i != len(frames)-1 {
				t.Errorf("[DONE] at %d of %d frames", i, len(frames))
			}
			out.done = true
			continue
		}
		m := frameJSON(t, f)
		if u, ok := m["usage"].(map[string]any); ok {
			out.usage, out.usageAt = u, i
		}
		d, fin := deltaOf(t, f)
		if d["role"] == "assistant" {
			if i != 0 {
				t.Errorf("role chunk at frame %d", i)
			}
			out.role = true
		}
		if s, _ := d["content"].(string); s != "" {
			out.content += s
		}
		if s, _ := d["reasoning_content"].(string); s != "" {
			out.reasoning += s
		}
		tcs, _ := d["tool_calls"].([]any)
		for _, raw := range tcs {
			tc, _ := raw.(map[string]any)
			idx, ok := tc["index"].(float64)
			if !ok {
				t.Fatalf("tool call delta without an index: %s", f.Data)
			}
			for len(out.calls) <= int(idx) {
				out.calls = append(out.calls, streamedCall{})
			}
			c := &out.calls[int(idx)]
			if s, _ := tc["id"].(string); s != "" {
				c.id = s
			}
			fn, _ := tc["function"].(map[string]any)
			if s, _ := fn["name"].(string); s != "" {
				c.name = s
			}
			s, _ := fn["arguments"].(string)
			c.args += s
		}
		if s, _ := fin.(string); s != "" {
			out.finish = s
		}
	}
	return out
}

func TestAnthropicStreamTranslatesEvents(t *testing.T) {
	cases := []struct {
		name       string
		events     []string
		content    string
		reasoning  string
		calls      []streamedCall
		finish     string
		prohibited bool
	}{
		{
			name:    "text only, with pings",
			events:  []string{aStart, aTextStart(0), aPing, aText(0, "Hel"), aPing, aText(0, "lo"), aBlockStop(0), aMessageDelta("end_turn", 5), aStop},
			content: "Hello", finish: "stop",
		},
		{
			name: "thinking, then text",
			events: []string{aStart, aBlockStart(0, `{"type":"thinking","thinking":""}`), aThinking(0, "Let me "), aThinking(0, "think."),
				aDelta(0, `{"type":"signature_delta","signature":"EqQBCgIYAhIM"}`), aBlockStop(0),
				aTextStart(1), aText(1, "Answer"), aBlockStop(1), aMessageDelta("end_turn", 9), aStop},
			content: "Answer", reasoning: "Let me think.", finish: "stop",
		},
		{
			name: "one tool call, arguments split",
			events: []string{aStart, aToolStart(0, "toolu_1", "get_weather"), aArgs(0, ""), aArgs(0, `{"city":`), aArgs(0, ` "Paris"}`),
				aBlockStop(0), aMessageDelta("tool_use", 20), aStop},
			calls: []streamedCall{{"toolu_1", "get_weather", `{"city": "Paris"}`}}, finish: "tool_calls",
		},
		{
			name: "text and two tool calls",
			events: []string{aStart, aTextStart(0), aText(0, "Checking."), aBlockStop(0),
				aToolStart(1, "toolu_a", "read"), aArgs(1, `{"path":`), aArgs(1, `"a.go"}`), aBlockStop(1),
				aToolStart(2, "toolu_b", "read"), aArgs(2, `{"path":"b.go"`), aPing, aArgs(2, `}`), aBlockStop(2),
				aMessageDelta("tool_use", 40), aStop},
			content: "Checking.",
			calls:   []streamedCall{{"toolu_a", "read", `{"path":"a.go"}`}, {"toolu_b", "read", `{"path":"b.go"}`}},
			finish:  "tool_calls",
		},
		{
			name:   "tool call without arguments",
			events: []string{aStart, aToolStart(0, "toolu_n", "now"), aBlockStop(0), aMessageDelta("tool_use", 3), aStop},
			calls:  []streamedCall{{"toolu_n", "now", `{}`}}, finish: "tool_calls",
		},
		{
			name:    "max_tokens",
			events:  []string{aStart, aTextStart(0), aText(0, "Once upon"), aMessageDelta("max_tokens", 2), aStop},
			content: "Once upon", finish: "length",
		},
		{
			name:    "refusal after text",
			events:  []string{aStart, aTextStart(0), aText(0, "Sure,"), aBlockStop(0), aMessageDelta("refusal", 2), aStop},
			content: "Sure,", finish: "content_filter", prohibited: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) { anthropicSSE(w, c.events...) })
			s, resp := anthropicStreamCall(t, srv.URL, OpChat, chatStreamBody(nil), time.Minute)
			if s == nil {
				t.Fatalf("want a stream, got %+v", resp)
			}
			frames, err := drain(t, s)
			if err != nil {
				t.Fatalf("stream failed: %v", err)
			}
			got := collectChat(t, frames)
			if !got.role || !got.done || got.usage != nil {
				t.Errorf("role=%v done=%v usage=%v; want the role first, [DONE] last, no usage chunk", got.role, got.done, got.usage)
			}
			if got.content != c.content || got.reasoning != c.reasoning || got.finish != c.finish {
				t.Errorf("content=%q reasoning=%q finish=%q; want %q %q %q", got.content, got.reasoning, got.finish, c.content, c.reasoning, c.finish)
			}
			if !reflect.DeepEqual(got.calls, c.calls) {
				t.Errorf("tool calls = %+v, want %+v", got.calls, c.calls)
			}
			for _, call := range got.calls {
				if !json.Valid([]byte(call.args)) {
					t.Errorf("arguments %q are not JSON", call.args)
				}
			}

			st := s.State()
			hasContent := c.content != "" || len(c.calls) > 0
			if !st.Output || st.HasContent != hasContent || st.FinishReason != c.finish || st.Prohibited != c.prohibited {
				t.Errorf("state = %+v", st)
			}
			msg := decodeCompletion(t, st.Body).Choices[0].Message
			if (msg.Content == nil && c.content != "") || (msg.Content != nil && *msg.Content != c.content) || msg.ReasoningContent != c.reasoning {
				t.Errorf("Body = %s", st.Body)
			}
			if len(msg.ToolCalls) != len(c.calls) {
				t.Fatalf("Body tool calls = %+v, want %d", msg.ToolCalls, len(c.calls))
			}
			for i, tc := range msg.ToolCalls {
				if tc.ID != c.calls[i].id || tc.Function.Name != c.calls[i].name || tc.Function.Arguments != c.calls[i].args {
					t.Errorf("Body tool call %d = %+v, want %+v", i, tc, c.calls[i])
				}
			}
		})
	}
}

func TestAnthropicStreamOutbound(t *testing.T) {
	var outbound map[string]any
	var path, key, version string
	srv := streamServer(t, &outbound, func(w http.ResponseWriter, r *http.Request) {
		path, key, version = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		anthropicSSE(w, aStart, aTextStart(0), aText(0, "x"), aMessageDelta("end_turn", 1), aStop)
	})
	body := chatStreamBody(map[string]any{"stream_options": map[string]any{"include_usage": true}, "reasoning_effort": "high"})
	s, _ := anthropicStreamCall(t, srv.URL, OpChat, body, time.Minute)
	if _, err := drain(t, s); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if path != "/messages" || key != "k" || version != anthropicVersion {
		t.Errorf("path=%q x-api-key=%q anthropic-version=%q", path, key, version)
	}
	oc, _ := outbound["output_config"].(map[string]any)
	if outbound["stream"] != true || outbound["model"] != "claude-opus-5-5" || outbound["stream_options"] != nil || oc["effort"] != "high" {
		t.Errorf("outbound = %v, want stream, the model, effort and no stream_options", outbound)
	}
}

// The usage chunk carries the same totals as a whole reply, and message_delta's
// counts replace message_start's rather than adding to them.
func TestAnthropicStreamUsage(t *testing.T) {
	events := []string{aStart, aTextStart(0), aText(0, "ok"), aBlockStop(0), aMessageDelta("end_turn", 7), aStop}
	whole, _, _ := anthropicToOpenAIChat([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":4,"cache_creation_input_tokens":300,"cache_read_input_tokens":19621,"output_tokens":7}}`), "claude-opus-5-5")
	want, _ := frameJSON(t, Frame{Data: whole})["usage"].(map[string]any)
	if want["prompt_tokens"] != float64(19925) || want["completion_tokens"] != float64(7) {
		t.Fatalf("whole reply usage = %v", want)
	}
	for _, include := range []bool{true, false} {
		srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) { anthropicSSE(w, events...) })
		var extra map[string]any
		if include {
			extra = map[string]any{"stream_options": map[string]any{"include_usage": true}}
		}
		s, _ := anthropicStreamCall(t, srv.URL, OpChat, chatStreamBody(extra), time.Minute)
		frames, err := drain(t, s)
		if err != nil {
			t.Fatalf("stream failed: %v", err)
		}
		got := collectChat(t, frames)
		switch {
		case include && (!reflect.DeepEqual(got.usage, want) || got.usageAt != len(frames)-2):
			t.Errorf("usage chunk = %v at %d of %d, want %v right before [DONE]", got.usage, got.usageAt, len(frames), want)
		case !include && got.usage != nil:
			t.Errorf("usage chunk sent unasked: %v", got.usage)
		}
		logged, _ := frameJSON(t, Frame{Data: s.State().Body})["usage"].(map[string]any)
		if !reflect.DeepEqual(logged, want) {
			t.Errorf("include_usage=%v: logged usage = %v, want %v", include, logged, want)
		}
	}
}

func TestAnthropicStreamHTTPErrorIsAResponse(t *testing.T) {
	for _, c := range []struct {
		status int
		typ    string
	}{{400, "invalid_request_error"}, {429, "rate_limit_error"}, {529, "overloaded_error"}} {
		body := fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":"m"}}`, c.typ)
		srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			fmt.Fprint(w, body)
		})
		s, resp := anthropicStreamCall(t, srv.URL, OpChat, chatStreamBody(nil), time.Minute)
		if s != nil || resp == nil || resp.HTTPStatus != c.status || string(resp.OpenAIBody) != body {
			t.Errorf("%d: want a Response with the error, got stream=%v resp=%+v", c.status, s, resp)
		}
	}
}

func TestAnthropicStreamJSONReplyIsAResponse(t *testing.T) {
	cases := []struct {
		reply, finish string
		prohibited    bool
	}{
		{`{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, "stop", false},
		{`{"content":[],"stop_reason":"refusal","usage":{"input_tokens":1,"output_tokens":0}}`, "content_filter", true},
	}
	for _, c := range cases {
		srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, c.reply)
		})
		s, resp := anthropicStreamCall(t, srv.URL, OpChat, chatStreamBody(nil), time.Minute)
		if s != nil || resp == nil || !resp.OK() {
			t.Fatalf("want a 2xx Response, got stream=%v resp=%+v", s, resp)
		}
		if resp.FinishReason != c.finish || resp.Prohibited != c.prohibited {
			t.Errorf("finish=%q prohibited=%v, want %q %v", resp.FinishReason, resp.Prohibited, c.finish, c.prohibited)
		}
	}
}

func TestAnthropicStreamErrorEvent(t *testing.T) {
	for _, afterOutput := range []bool{false, true} {
		events := []string{aStart, aPing, aOverloaded}
		if afterOutput {
			events = []string{aStart, aTextStart(0), aText(0, "Hi"), aOverloaded}
		}
		srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) { anthropicSSE(w, events...) })
		s, _ := anthropicStreamCall(t, srv.URL, OpChat, chatStreamBody(nil), time.Minute)
		_, err := drain(t, s)
		var se *StreamError
		if !errors.As(err, &se) {
			t.Fatalf("err = %v, want *StreamError", err)
		}
		if se.Status != 529 || se.Frame == nil || se.Timeout || se.Prohibited {
			t.Errorf("StreamError = %+v, want 529 with a frame", se)
		}
		want := map[string]any{"error": map[string]any{"type": "overloaded_error", "message": "Overloaded", "code": float64(529)}}
		if got := frameJSON(t, *se.Frame); !reflect.DeepEqual(got, want) {
			t.Errorf("error frame = %v, want %v", got, want)
		}
		if s.State().Output != afterOutput {
			t.Errorf("Output = %v, want %v", s.State().Output, afterOutput)
		}
	}
}

func TestAnthropicStreamEndedEarly(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		anthropicSSE(w, aStart, aTextStart(0), aText(0, "Hi"))
	})
	s, _ := anthropicStreamCall(t, srv.URL, OpChat, chatStreamBody(nil), time.Minute)
	_, err := drain(t, s)
	var se *StreamError
	if !errors.As(err, &se) || se.Status != 502 {
		t.Fatalf("err = %v, want a 502 *StreamError", err)
	}
}

func TestAnthropicStreamEmptyReply(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		anthropicSSE(w, aStart, aMessageDelta("end_turn", 0), aStop)
	})
	s, _ := anthropicStreamCall(t, srv.URL, OpChat, chatStreamBody(nil), time.Minute)
	if _, err := drain(t, s); err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if st := s.State(); st.Output || st.HasContent || st.FinishReason != "stop" {
		t.Errorf("state = %+v, want no output, no content", st)
	}
}

func TestAnthropicStreamFirstOutputTimeout(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		anthropicSSE(w, aStart)
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
				fmt.Fprintf(w, "event: ping\ndata: %s\n\n", aPing)
				w.(http.Flusher).Flush()
			}
		}
	})
	s, _ := anthropicStreamCall(t, srv.URL, OpChat, chatStreamBody(nil), 100*time.Millisecond)
	defer s.Close()
	if _, err := nextWithin(t, s, time.Second); err != nil {
		t.Fatalf("role chunk: %v", err)
	}
	_, err := nextWithin(t, s, time.Second)
	var se *StreamError
	if !errors.As(err, &se) || !se.Timeout || se.Status != 504 {
		t.Fatalf("err = %v, want a 504 timeout: pings are not output", err)
	}
}

func TestAnthropicStreamSilenceTimeout(t *testing.T) {
	srv := streamServer(t, nil, blockAfterAnthropic(aStart, aTextStart(0), aText(0, "Hi")))
	s, _ := anthropicStreamCall(t, srv.URL, OpChat, chatStreamBody(nil), 100*time.Millisecond)
	defer s.Close()
	for range 2 { // the role, then the text
		if _, err := nextWithin(t, s, time.Second); err != nil {
			t.Fatalf("frame: %v", err)
		}
	}
	if !s.State().Output {
		t.Fatal("text should count as output")
	}
	_, err := nextWithin(t, s, time.Second)
	var se *StreamError
	if !errors.As(err, &se) || !se.Timeout {
		t.Fatalf("err = %v, want a silence timeout", err)
	}
}

// blockAfterAnthropic sends events, then holds the connection open until the client goes away.
func blockAfterAnthropic(events ...string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		anthropicSSE(w, events...)
		<-r.Context().Done()
	}
}

// withoutCallIDs drops the ids and call ids that differ by provider, for
// comparing Responses output items.
func withoutCallIDs(output any) []any {
	items := withoutIDs(output)
	for _, it := range items {
		delete(it.(map[string]any), "call_id")
	}
	return items
}

// A Responses request over Anthropic's stream gets the same events and final
// output as over Gemini's, for the same reply: reasoning, text and a tool call.
func TestAnthropicResponsesStreamMatchesGemini(t *testing.T) {
	gemini := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		sseChunks(w,
			`{"candidates":[{"content":{"parts":[{"text":"Let me look.","thought":true}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"text":"Reading"}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"text":" it."}]}}]}`,
			`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read","args":{"path":"a.go"}}}]},"finishReason":"STOP"}]}`)
	})
	anthropic := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		anthropicSSE(w, aStart, aBlockStart(0, `{"type":"thinking","thinking":""}`), aThinking(0, "Let me look."), aBlockStop(0),
			aTextStart(1), aText(1, "Reading"), aText(1, " it."), aBlockStop(1),
			aToolStart(2, "toolu_1", "read"), aArgs(2, `{"path":"a.go"}`), aBlockStop(2),
			aMessageDelta("tool_use", 12), aStop)
	})
	body := map[string]any{"input": "read a.go", "stream": true}
	gs, _ := geminiStreamCall(t, gemini.URL, OpResponses, body, nil)
	as, resp := anthropicStreamCall(t, anthropic.URL, OpResponses, body, time.Minute)
	if as == nil {
		t.Fatalf("want a stream, got %+v", resp)
	}
	gevs, err := drainResponses(t, gs)
	if err != nil {
		t.Fatalf("Gemini stream failed: %v", err)
	}
	aevs, err := drainResponses(t, as)
	if err != nil {
		t.Fatalf("Anthropic stream failed: %v", err)
	}
	if g, a := eventTypes(gevs), eventTypes(aevs); !reflect.DeepEqual(g, a) {
		t.Fatalf("event types differ:\nGemini    %v\nAnthropic %v", g, a)
	}
	gf, af := finalResponse(t, gevs), finalResponse(t, aevs)
	if g, a := withoutCallIDs(gf["output"]), withoutCallIDs(af["output"]); !reflect.DeepEqual(g, a) {
		t.Errorf("final output differs:\nGemini    %v\nAnthropic %v", g, a)
	}
	if gf["status"] != "completed" || af["status"] != "completed" {
		t.Errorf("status = %v / %v", gf["status"], af["status"])
	}
	out, _ := af["output"].([]any)
	if call, _ := out[len(out)-1].(map[string]any); call["call_id"] != "toolu_1" || !strings.Contains(fmt.Sprint(call["arguments"]), "a.go") {
		t.Errorf("Anthropic tool call item = %v", call)
	}
}
