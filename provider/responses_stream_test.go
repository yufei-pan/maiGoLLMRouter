package provider

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"
)

// cannedChat is a stream source over fixed Chat chunk data, ending with end
// (io.EOF when nil). Its state is what the chunks add up to.
type cannedChat struct {
	data []string
	end  error
	acc  *chatAccumulator
}

func (c *cannedChat) next() (Frame, error) {
	if len(c.data) == 0 {
		if c.end != nil {
			return Frame{}, c.end
		}
		return Frame{}, io.EOF
	}
	d := c.data[0]
	c.data = c.data[1:]
	if d != "[DONE]" {
		var m map[string]any
		_ = json.Unmarshal([]byte(d), &m)
		c.acc.add(m)
	}
	return Frame{Data: []byte(d)}, nil
}
func (c *cannedChat) state() StreamState                    { return c.acc.state() }
func (c *cannedChat) errorFrame(status int, m string) Frame { return chatErrorFrame(status, m) }
func (c *cannedChat) close()                                {}

func cannedChatStream(end error, data ...string) *Stream {
	return &Stream{HTTPStatus: 200, src: &cannedChat{data: data, end: end, acc: newChatAccumulator("real")}}
}

type responsesEvent struct {
	frame Frame
	m     map[string]any
}

func drainResponses(t *testing.T, s *Stream) ([]responsesEvent, error) {
	t.Helper()
	frames, err := drain(t, s)
	out := make([]responsesEvent, 0, len(frames))
	for _, f := range frames {
		out = append(out, responsesEvent{f, frameJSON(t, f)})
	}
	return out, err
}

func eventTypes(evs []responsesEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i], _ = e.m["type"].(string)
	}
	return out
}

func finalResponse(t *testing.T, evs []responsesEvent) map[string]any {
	t.Helper()
	last := evs[len(evs)-1].m
	resp, ok := last["response"].(map[string]any)
	if !ok {
		t.Fatalf("last event has no response object: %v", last)
	}
	return resp
}

// withoutIDs drops "id" keys from output items, for comparing shapes.
func withoutIDs(output any) []any {
	items, _ := output.([]any)
	out := make([]any, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		c := make(map[string]any, len(m))
		for k, v := range m {
			if k != "id" {
				c[k] = v
			}
		}
		out = append(out, c)
	}
	return out
}

func TestChatToResponsesTextSequence(t *testing.T) {
	inner := cannedChatStream(nil,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hi"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":" there"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"[DONE]")
	evs, err := drainResponses(t, chatToResponses(inner, "real"))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	want := []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.delta", "response.output_text.done", "response.content_part.done",
		"response.output_item.done", "response.completed"}
	if got := eventTypes(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("event types = %v\nwant %v", got, want)
	}
	for i, e := range evs {
		if e.m["sequence_number"] != float64(i) {
			t.Errorf("event %d sequence_number = %v", i, e.m["sequence_number"])
		}
		if e.frame.Event != e.m["type"] {
			t.Errorf("event %d: SSE event %q != type %v", i, e.frame.Event, e.m["type"])
		}
		if string(e.frame.Data) == "[DONE]" {
			t.Error("a Responses stream has no [DONE]")
		}
	}
	resp := finalResponse(t, evs)
	out, _ := resp["output"].([]any)
	msg, _ := out[0].(map[string]any)
	content, _ := msg["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["text"] != "Hi there" {
		t.Errorf("final text = %v, want Hi there", part["text"])
	}
	if usage, _ := resp["usage"].(map[string]any); usage["input_tokens"] == nil {
		t.Errorf("final usage = %v, want input_tokens", resp["usage"])
	}
}

func TestChatToResponsesToolCallSequence(t *testing.T) {
	inner := cannedChatStream(nil,
		`{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c0","type":"function","function":{"name":"speak","arguments":"{\"te"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"xt\":\"a\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"[DONE]")
	evs, err := drainResponses(t, chatToResponses(inner, "real"))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	want := []string{"response.created", "response.in_progress", "response.output_item.added",
		"response.function_call_arguments.delta", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done", "response.completed"}
	if got := eventTypes(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("event types = %v\nwant %v", got, want)
	}
	item, _ := evs[2].m["item"].(map[string]any)
	if item["type"] != "function_call" || item["call_id"] != "c0" || item["name"] != "speak" || item["arguments"] != "" {
		t.Errorf("added item = %v", item)
	}
	if evs[3].m["output_index"] != evs[4].m["output_index"] {
		t.Error("argument deltas should share one output_index")
	}
	if evs[5].m["arguments"] != `{"text":"a"}` {
		t.Errorf("arguments.done = %v", evs[5].m["arguments"])
	}
}

func TestChatToResponsesFinalMatchesChatToResponses(t *testing.T) {
	inner := cannedChatStream(nil,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Let me say it."}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c0","type":"function","function":{"name":"speak","arguments":"{\"text\":\"a\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"[DONE]")
	s := chatToResponses(inner, "real")
	evs, err := drainResponses(t, s)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	ref, err := ChatToResponses(inner.State().Body, "real", "tool_calls")
	if err != nil {
		t.Fatal(err)
	}
	var refObj map[string]any
	_ = json.Unmarshal(ref, &refObj)
	got := withoutIDs(finalResponse(t, evs)["output"])
	if want := withoutIDs(refObj["output"]); !reflect.DeepEqual(got, want) {
		t.Errorf("final output = %v\nwant %v", got, want)
	}
}

func TestChatToResponsesReasoningSummaryOnly(t *testing.T) {
	inner := cannedChatStream(nil,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"Thinking"}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":" hard"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Done"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"[DONE]")
	evs, err := drainResponses(t, chatToResponses(inner, "real"))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	var reasoningDone map[string]any
	deltas := 0
	for _, e := range evs {
		switch e.m["type"] {
		case "response.reasoning_summary_text.delta":
			deltas++
		case "response.output_item.done":
			if item, _ := e.m["item"].(map[string]any); item["type"] == "reasoning" {
				reasoningDone = item
			}
		}
	}
	if deltas != 2 || reasoningDone == nil {
		t.Fatalf("reasoning deltas = %d, reasoning item = %v", deltas, reasoningDone)
	}
	summary, _ := reasoningDone["summary"].([]any)
	first, _ := summary[0].(map[string]any)
	if first["text"] != "Thinking hard" {
		t.Errorf("summary = %v", summary)
	}
	if _, has := reasoningDone["content"]; has {
		t.Error("reasoning item must not carry content")
	}
	if _, has := reasoningDone["encrypted_content"]; has {
		t.Error("reasoning item must not carry encrypted_content")
	}
}

func TestChatToResponsesIncomplete(t *testing.T) {
	for finish, reason := range map[string]string{"length": "max_output_tokens", "content_filter": "content_filter"} {
		inner := cannedChatStream(nil,
			`{"choices":[{"index":0,"delta":{"content":"Partial"}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"`+finish+`"}]}`,
			"[DONE]")
		evs, err := drainResponses(t, chatToResponses(inner, "real"))
		if err != nil {
			t.Fatalf("%s: stream failed: %v", finish, err)
		}
		last := evs[len(evs)-1].m
		resp, _ := last["response"].(map[string]any)
		details, _ := resp["incomplete_details"].(map[string]any)
		if last["type"] != "response.incomplete" || details["reason"] != reason {
			t.Errorf("%s: last event %v with details %v, want response.incomplete / %s", finish, last["type"], details, reason)
		}
	}
}

func TestChatToResponsesInnerError(t *testing.T) {
	chatErr := &StreamError{
		Status: 429,
		Body:   []byte(`{"error":{"message":"Rate limit","code":429}}`),
		Frame:  &Frame{Data: []byte(`{"error":{"message":"Rate limit","code":429}}`)},
	}
	inner := cannedChatStream(chatErr, `{"choices":[{"index":0,"delta":{"content":"Hi"}}]}`)
	evs, err := drainResponses(t, chatToResponses(inner, "real"))
	var se *StreamError
	if !errors.As(err, &se) || se.Status != 429 {
		t.Fatalf("err = %v, want *StreamError 429", err)
	}
	if se.Frame == nil || se.Frame.Event != "error" {
		t.Fatalf("Frame = %+v, want a Responses error event", se.Frame)
	}
	m := frameJSON(t, *se.Frame)
	if m["type"] != "error" || m["message"] != "Rate limit" || m["sequence_number"] != float64(len(evs)) {
		t.Errorf("error event = %v (after %d events)", m, len(evs))
	}
}

func TestChatToResponsesKeepsThoughtSignature(t *testing.T) {
	inner := cannedChatStream(nil,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","thought_signature":"sig1","function":{"name":"speak","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"[DONE]")
	evs, err := drainResponses(t, chatToResponses(inner, "real"))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	for _, e := range evs {
		if e.m["type"] != "response.output_item.done" {
			continue
		}
		if item, _ := e.m["item"].(map[string]any); item["thought_signature"] != "sig1" {
			t.Errorf("output_item.done item = %v, want thought_signature sig1", item)
		}
	}
	out, _ := finalResponse(t, evs)["output"].([]any)
	if item, _ := out[0].(map[string]any); item["thought_signature"] != "sig1" {
		t.Errorf("final item = %v, want thought_signature sig1", item)
	}
}

// TestChatToResponsesTextMidCallKeepsArguments verifies text arriving between
// two fragments of a tool call's arguments neither closes the call early nor
// drops a fragment, and the final output follows the streamed order.
func TestChatToResponsesTextMidCallKeepsArguments(t *testing.T) {
	inner := cannedChatStream(nil,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c0","type":"function","function":{"name":"speak","arguments":"{\"te"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"\n"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"xt\":\"a\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"[DONE]")
	evs, err := drainResponses(t, chatToResponses(inner, "real"))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	deltas := 0
	var callDone map[string]any
	for _, e := range evs {
		if e.m["type"] == "response.function_call_arguments.delta" {
			deltas++
		}
		if item, _ := e.m["item"].(map[string]any); e.m["type"] == "response.output_item.done" && item["type"] == "function_call" {
			callDone = item
		}
	}
	if deltas != 2 || callDone == nil || callDone["arguments"] != `{"text":"a"}` {
		t.Fatalf("argument deltas = %d, call done = %v", deltas, callDone)
	}
	out, _ := finalResponse(t, evs)["output"].([]any)
	if first, _ := out[0].(map[string]any); first["type"] != "function_call" {
		t.Errorf("final output order = %v, want the call first, as streamed", out)
	}
}
