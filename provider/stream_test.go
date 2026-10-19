package provider

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestErrorStatus(t *testing.T) {
	cases := []struct {
		code any
		want int
	}{
		{float64(429), 429},
		{float64(600), 502},
		{"429", 502},
		{"rate_limit_exceeded", 429},
		{"slow_down", 429},
		{"insufficient_quota", 429},
		{"server_is_overloaded", 503},
		{"invalid_api_key", 401},
		{"model_not_found", 404},
		{"context_length_exceeded", 400},
		{"server_error", 502},
		{nil, 502},
	}
	for _, c := range cases {
		if got := errorStatus(c.code); got != c.want {
			t.Errorf("errorStatus(%#v) = %d, want %d", c.code, got, c.want)
		}
	}
}

func TestStreamErrorFromObject(t *testing.T) {
	obj := map[string]any{"message": "Rate limit", "code": float64(429)}
	frame := &Frame{Data: []byte(`{"error":{"message":"Rate limit","code":429}}`)}
	se := streamErrorFromObject(obj, frame)
	if se.Status != 429 {
		t.Errorf("Status = %d, want 429", se.Status)
	}
	var body map[string]any
	if err := json.Unmarshal(se.Body, &body); err != nil {
		t.Fatalf("Body is not JSON: %v", err)
	}
	if !reflect.DeepEqual(body, map[string]any{"error": obj}) {
		t.Errorf("Body = %v, want {error: %v}", body, obj)
	}
	if se.Frame != frame {
		t.Error("Frame not passed through")
	}
	if se.Prohibited {
		t.Error("a rate limit is not a policy block")
	}
	if !streamErrorFromObject(map[string]any{"message": "PROHIBITED_CONTENT"}, nil).Prohibited {
		t.Error("PROHIBITED_CONTENT should be a policy block")
	}
}

func frameJSON(t *testing.T, f Frame) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(f.Data, &m); err != nil {
		t.Fatalf("frame data %q is not JSON: %v", f.Data, err)
	}
	return m
}

func TestErrorFrames(t *testing.T) {
	c := chatErrorFrame(504, "m")
	if c.Event != "" {
		t.Errorf("chat error frame has event %q", c.Event)
	}
	want := map[string]any{"error": map[string]any{"message": "m", "type": "upstream_error", "code": float64(504)}}
	if got := frameJSON(t, c); !reflect.DeepEqual(got, want) {
		t.Errorf("chat error frame = %v, want %v", got, want)
	}

	r := responsesErrorFrame(7, "m")
	if r.Event != "error" {
		t.Errorf("responses error frame event = %q, want error", r.Event)
	}
	want = map[string]any{"type": "error", "code": "server_error", "message": "m", "sequence_number": float64(7)}
	if got := frameJSON(t, r); !reflect.DeepEqual(got, want) {
		t.Errorf("responses error frame = %v, want %v", got, want)
	}
}

func TestWantsUsage(t *testing.T) {
	cases := []struct {
		body map[string]any
		want bool
	}{
		{map[string]any{"stream": true}, false},
		{map[string]any{"stream_options": map[string]any{"include_usage": false}}, false},
		{map[string]any{"stream_options": map[string]any{"include_usage": true}}, true},
		{map[string]any{"stream_options": "yes"}, false},
	}
	for _, c := range cases {
		if got := WantsUsage(c.body); got != c.want {
			t.Errorf("WantsUsage(%v) = %v, want %v", c.body, got, c.want)
		}
	}
}
