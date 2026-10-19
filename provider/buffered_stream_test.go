package provider

import (
	"encoding/json"
	"reflect"
	"testing"
)

const bufferedChatBody = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"real","choices":[{"index":0,"message":{"role":"assistant","content":"hi","tool_calls":[{"id":"c1","type":"function","thought_signature":"s","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

func bufferedChatResponse() *Response {
	return &Response{HTTPStatus: 200, OpenAIBody: []byte(bufferedChatBody), FinishReason: "tool_calls", HasContent: true}
}

func TestBufferedChatStream(t *testing.T) {
	s := BufferedStream(OpChat, bufferedChatResponse(), false)
	frames, err := drain(t, s)
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if last := frames[len(frames)-1]; string(last.Data) != "[DONE]" {
		t.Fatalf("last frame = %s, want [DONE]", last.Data)
	}
	acc := newChatAccumulator("real")
	for _, f := range frames[:len(frames)-1] {
		var m map[string]any
		if err := json.Unmarshal(f.Data, &m); err != nil {
			t.Fatalf("frame %s: %v", f.Data, err)
		}
		if _, has := m["usage"]; has {
			t.Errorf("usage sent though not asked: %s", f.Data)
		}
		acc.add(m)
	}
	c := decodeCompletion(t, acc.state().Body)
	msg := c.Choices[0].Message
	if *msg.Content != "hi" || len(msg.ToolCalls) != 1 {
		t.Fatalf("rebuilt message = %+v", msg)
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "c1" || tc.Function.Name != "f" || tc.Function.Arguments != `{"a":1}` || tc.ThoughtSignature != "s" {
		t.Errorf("rebuilt tool call = %+v", tc)
	}
	if *c.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish = %v", *c.Choices[0].FinishReason)
	}
	if st := s.State(); !st.Output || st.FinishReason != "tool_calls" || string(st.Body) != bufferedChatBody {
		t.Errorf("state = %+v", st)
	}
}

func TestBufferedChatStreamUsageOnlyWhenAsked(t *testing.T) {
	frames, _ := drain(t, BufferedStream(OpChat, bufferedChatResponse(), true))
	usage := frameJSON(t, frames[len(frames)-2])["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(3) {
		t.Errorf("usage chunk = %s", frames[len(frames)-2].Data)
	}
}

const bufferedResponsesBody = `{"id":"r1","object":"response","created_at":1,"status":"completed","model":"real","output":[{"id":"m1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hi","annotations":[]}]},{"id":"fc1","type":"function_call","call_id":"c1","name":"f","arguments":"{}","status":"completed"}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`

func TestBufferedResponsesStream(t *testing.T) {
	resp := &Response{HTTPStatus: 200, OpenAIBody: []byte(bufferedResponsesBody), FinishReason: "tool_calls", HasContent: true}
	evs, err := drainResponses(t, BufferedStream(OpResponses, resp, false))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	want := []string{"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done", "response.completed"}
	if got := eventTypes(evs); !reflect.DeepEqual(got, want) {
		t.Fatalf("event types = %v\nwant %v", got, want)
	}
	var orig map[string]any
	_ = json.Unmarshal([]byte(bufferedResponsesBody), &orig)
	if got := finalResponse(t, evs); !reflect.DeepEqual(got, orig) {
		t.Errorf("final response = %v\nwant %v", got, orig)
	}
}

func TestRefusalStreams(t *testing.T) {
	s := RefusalStream(OpChat, "m")
	frames, err := drain(t, s)
	if err != nil || len(frames) != 3 {
		t.Fatalf("chat refusal frames = %q, err = %v", frames, err)
	}
	if d, _ := deltaOf(t, frames[0]); d["role"] != "assistant" {
		t.Errorf("frame 0 = %s", frames[0].Data)
	}
	if _, fin := deltaOf(t, frames[1]); fin != "content_filter" {
		t.Errorf("frame 1 = %s", frames[1].Data)
	}
	if string(frames[2].Data) != "[DONE]" {
		t.Errorf("frame 2 = %s", frames[2].Data)
	}
	if !s.State().Prohibited {
		t.Error("a refusal stream is a policy block")
	}

	r := RefusalStream(OpResponses, "m")
	evs, err := drainResponses(t, r)
	if err != nil {
		t.Fatalf("responses refusal: %v", err)
	}
	if got := eventTypes(evs); !reflect.DeepEqual(got, []string{"response.created", "response.incomplete"}) {
		t.Fatalf("event types = %v", got)
	}
	details, _ := finalResponse(t, evs)["incomplete_details"].(map[string]any)
	if details["reason"] != "content_filter" {
		t.Errorf("incomplete_details = %v", details)
	}
	if !r.State().Prohibited {
		t.Error("a refusal stream is a policy block")
	}
}

func TestBufferedResponsesStreamClosesEveryItem(t *testing.T) {
	body := `{"id":"r1","object":"response","created_at":1,"status":"completed","model":"real","output":[{"id":"m1","type":"message","role":"assistant","content":[{"type":"output_text","text":"A"}]},{"id":"rs1","type":"reasoning","summary":[{"type":"summary_text","text":"then think"}]},{"id":"m2","type":"message","role":"assistant","content":[{"type":"output_text","text":"B"}]}]}`
	resp := &Response{HTTPStatus: 200, OpenAIBody: []byte(body), FinishReason: "stop", HasContent: true}
	evs, err := drainResponses(t, BufferedStream(OpResponses, resp, false))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	added, done := 0, 0
	for _, e := range evs {
		switch e.m["type"] {
		case "response.output_item.added":
			added++
		case "response.output_item.done":
			done++
		}
	}
	if added != 3 || done != 3 {
		t.Errorf("items added %d, done %d; want 3 and 3 (types %v)", added, done, eventTypes(evs))
	}
}

// TestBufferedResponsesKeepsItems verifies each item's done event carries the
// original item: encrypted reasoning (even with no summary) and citations.
func TestBufferedResponsesKeepsItems(t *testing.T) {
	body := `{"id":"r1","object":"response","created_at":1,"status":"completed","model":"real","output":[` +
		`{"id":"rs0","type":"reasoning","summary":[],"encrypted_content":"ENC0"},` +
		`{"id":"rs1","type":"reasoning","summary":[{"type":"summary_text","text":"think"}],"encrypted_content":"ENC1"},` +
		`{"id":"m1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hi","annotations":[{"type":"url_citation","url":"https://x.example","title":"X"}]}]}]}`
	resp := &Response{HTTPStatus: 200, OpenAIBody: []byte(body), FinishReason: "stop", HasContent: true}
	evs, err := drainResponses(t, BufferedStream(OpResponses, resp, false))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	done := map[string]map[string]any{}
	for _, e := range evs {
		if item, _ := e.m["item"].(map[string]any); e.m["type"] == "response.output_item.done" {
			done[item["id"].(string)] = item
		}
	}
	if done["rs0"]["encrypted_content"] != "ENC0" {
		t.Errorf("summary-less reasoning item = %v, want it sent with encrypted_content", done["rs0"])
	}
	if done["rs1"]["encrypted_content"] != "ENC1" {
		t.Errorf("reasoning item = %v, want encrypted_content kept", done["rs1"])
	}
	content, _ := done["m1"]["content"].([]any)
	part, _ := content[0].(map[string]any)
	if anns, _ := part["annotations"].([]any); len(anns) != 1 {
		t.Errorf("message item = %v, want its citation kept", done["m1"])
	}
}

// TestBufferedChatKeepsReasoningAnnotationsLogprobs verifies the fields annai
// reads from chunks survive a buffered reply: reasoning (OpenRouter's name),
// citations and the first token's logprobs.
func TestBufferedChatKeepsReasoningAnnotationsLogprobs(t *testing.T) {
	body := `{"id":"c","object":"chat.completion","created":1,"model":"real","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"Hi","reasoning":"thinking","annotations":[{"type":"url_citation","url_citation":{"url":"https://x.example","title":"X"}}]},` +
		`"logprobs":{"content":[{"token":"Hi","logprob":-0.1,"top_logprobs":[]}]},"finish_reason":"stop"}]}`
	resp := &Response{HTTPStatus: 200, OpenAIBody: []byte(body), FinishReason: "stop", HasContent: true}
	frames, err := drain(t, BufferedStream(OpChat, resp, false))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	var reasoning, annotations, logprobs bool
	for _, f := range frames[:len(frames)-1] {
		m := frameJSON(t, f)
		choices, _ := m["choices"].([]any)
		for _, c := range choices {
			ch, _ := c.(map[string]any)
			d, _ := ch["delta"].(map[string]any)
			reasoning = reasoning || d["reasoning"] == "thinking" || d["reasoning_content"] == "thinking"
			annotations = annotations || d["annotations"] != nil
			logprobs = logprobs || ch["logprobs"] != nil
		}
	}
	if !reasoning || !annotations || !logprobs {
		t.Errorf("reasoning=%v annotations=%v logprobs=%v, want all kept", reasoning, annotations, logprobs)
	}
}
