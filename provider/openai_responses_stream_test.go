package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// sseNamed writes {event, data} pairs as SSE events, flushing after each.
func sseNamed(w http.ResponseWriter, events ...[2]string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, e := range events {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e[0], e[1])
		w.(http.Flusher).Flush()
	}
}

func responsesStreamReq(mode ResponsesMode, body map[string]any) Request {
	if body == nil {
		body = map[string]any{"model": "inbound", "input": "hi", "stream": true}
	}
	return Request{Op: OpResponses, Model: "real", Body: body, ResponsesMode: mode}
}

func openResponsesStream(t *testing.T, url string, req Request) (*Stream, *Response) {
	t.Helper()
	s, resp, err := CallStream(context.Background(), &http.Client{}, "openai", url, "k", time.Minute, req)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	return s, resp
}

var (
	evCreated     = [2]string{"response.created", `{"type":"response.created","sequence_number":0,"response":{"id":"r1","object":"response","status":"in_progress","output":[]}}`}
	evMsgAdded    = [2]string{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"m1","type":"message","role":"assistant","content":[]}}`}
	evTextDelta   = [2]string{"response.output_text.delta", `{"type":"response.output_text.delta","sequence_number":2,"item_id":"m1","output_index":0,"content_index":0,"delta":"Hi"}`}
	evMsgDone     = [2]string{"response.output_item.done", `{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":"m1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi"}]}}`}
	evCompleted   = [2]string{"response.completed", `{"type":"response.completed","sequence_number":4,"response":{"id":"r1","object":"response","status":"completed","output":[{"id":"m1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi"}]}]}}`}
	evCallAdded   = [2]string{"response.output_item.added", `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"fc1","type":"function_call","call_id":"c1","name":"speak","arguments":""}}`}
	evFailedLimit = [2]string{"response.failed", `{"type":"response.failed","sequence_number":1,"response":{"id":"r1","status":"failed","error":{"code":"rate_limit_exceeded","message":"slow down"}}}`}
)

func TestResponsesStreamPassesEventsThrough(t *testing.T) {
	sent := [][2]string{evCreated, evMsgAdded, evTextDelta, evMsgDone, evCompleted}
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) { sseNamed(w, sent...) })
	s, _ := openResponsesStream(t, srv.URL, responsesStreamReq(ResponsesModeForce, nil))
	if s == nil {
		t.Fatal("want a stream")
	}
	for i, e := range sent {
		f, err := s.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if f.Event != e[0] || string(f.Data) != e[1] {
			t.Errorf("frame %d = %s %s, want %s %s", i, f.Event, f.Data, e[0], e[1])
		}
		if i == 1 && s.State().Output {
			t.Error("an empty message item is not output yet")
		}
		if i == 2 && !s.State().Output {
			t.Error("a text delta is output")
		}
	}
	if _, err := s.Next(); err == nil {
		t.Error("want the stream to end after response.completed")
	}
	if st := s.State(); st.FinishReason != "stop" || !st.HasContent {
		t.Errorf("state = %+v", st)
	}
}

func TestResponsesStreamFunctionCallAddedIsOutput(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) { sseNamed(w, evCreated, evCallAdded) })
	s, _ := openResponsesStream(t, srv.URL, responsesStreamReq(ResponsesModeForce, nil))
	defer s.Close()
	_, _ = s.Next()
	_, _ = s.Next()
	if !s.State().Output {
		t.Error("an added function_call item is output")
	}
}

func TestResponsesStreamFailedBeforeOutput(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) { sseNamed(w, evCreated, evFailedLimit) })
	s, _ := openResponsesStream(t, srv.URL, responsesStreamReq(ResponsesModeForce, nil))
	_, err := drain(t, s)
	var se *StreamError
	if !errors.As(err, &se) || se.Status != 429 {
		t.Fatalf("err = %v, want *StreamError 429", err)
	}
	if se.Frame == nil || se.Frame.Event != "response.failed" {
		t.Errorf("Frame = %+v, want the downstream's response.failed event", se.Frame)
	}
}

func TestResponsesStreamProbeFallsBackToChat(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"Unknown path /responses","code":"unknown_url"}}`)
			return
		}
		sseChunks(w, roleChunk, contentChunk, finishChunk, "[DONE]")
	})
	s, resp := openResponsesStream(t, srv.URL, responsesStreamReq(ResponsesModeProbe, nil))
	if s == nil {
		t.Fatalf("want a stream, got Response %+v", resp)
	}
	if !s.LearnChatOnly {
		t.Error("the probe should report the route missing")
	}
	f, err := s.Next()
	if err != nil || f.Event != "response.created" {
		t.Fatalf("first frame = %+v, %v; want response.created", f, err)
	}
}

func TestResponsesStreamChatOnlyIncompatible(t *testing.T) {
	srv := streamServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected downstream call to %s", r.URL.Path)
	})
	body := map[string]any{"model": "inbound", "input": "hi", "stream": true, "previous_response_id": "x"}
	s, resp := openResponsesStream(t, srv.URL, responsesStreamReq(ResponsesModeChatOnly, body))
	if s != nil || resp == nil || !resp.Incompatible {
		t.Fatalf("want an incompatible Response, got stream=%v resp=%+v", s, resp)
	}
}

func TestResponsesStreamAddsNoStreamOptions(t *testing.T) {
	var outbound map[string]any
	srv := streamServer(t, &outbound, func(w http.ResponseWriter, r *http.Request) { sseNamed(w, evCreated, evCompleted) })
	s, _ := openResponsesStream(t, srv.URL, responsesStreamReq(ResponsesModeForce, nil))
	_, _ = drain(t, s)
	if outbound["stream"] != true || outbound["model"] != "real" {
		t.Errorf("outbound stream/model = %v/%v", outbound["stream"], outbound["model"])
	}
	if _, has := outbound["stream_options"]; has {
		b, _ := json.Marshal(outbound)
		t.Errorf("outbound /responses body has stream_options: %s", b)
	}
}
