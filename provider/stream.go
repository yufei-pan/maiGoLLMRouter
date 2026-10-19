package provider

import (
	"errors"
	"fmt"
	"math"
)

// Stream is an open downstream reply in the inbound API's streaming format:
// Chat Completions chunks for OpChat, Responses events for OpResponses. The
// router reads it frame by frame, and State tells it when real output started
// (the point where it commits to this attempt).
type Stream struct {
	HTTPStatus    int
	OutboundURL   string
	OutboundBody  []byte
	LearnChatOnly bool // a probe found that the downstream has no /responses route

	src streamSource
	end error // io.EOF or the failure, once reached; every later Next returns it
}

// streamSource is one way of producing a stream's frames: a dialect read from
// the downstream, or a wrapper over another stream.
type streamSource interface {
	next() (Frame, error)
	state() StreamState
	errorFrame(status int, message string) Frame
	close()
}

// Next returns the next frame for the client: io.EOF after the final frame, a
// *StreamError when the stream failed.
func (s *Stream) Next() (Frame, error) {
	if s.end != nil {
		return Frame{}, s.end
	}
	f, err := s.src.next()
	if err != nil {
		s.end = err
		return Frame{}, err
	}
	return f, nil
}

// State reports what the frames so far add up to.
func (s *Stream) State() StreamState { return s.src.state() }

// ErrorFrame renders a failure the router detected itself as an error event in
// this stream's format.
func (s *Stream) ErrorFrame(status int, message string) Frame {
	return s.src.errorFrame(status, message)
}

// Close releases the downstream connection. It is safe to call more than once.
func (s *Stream) Close() { s.src.close() }

// StreamState is what a stream has produced so far.
type StreamState struct {
	Output       bool   // real output has been produced: the commit point
	FinishReason string // normalized, as Response.FinishReason
	HasContent   bool
	Prohibited   bool
	Body         []byte // the reply so far as one complete JSON body (chat.completion or response object)
	Raw          []byte // downstream bytes read, first rawLogLimit bytes
}

// StreamError is a failure reported by the downstream inside its stream, or
// detected by the router while reading it.
type StreamError struct {
	Status     int    // status for the client if this failure ends the run
	Body       []byte // {"error": <object>}
	Frame      *Frame // the downstream's own error event in the client's format; nil when the router writes its own
	Prohibited bool   // the failure is a content-policy block
	Timeout    bool   // the first-output or idle limit ran out
	Err        error
}

func (e *StreamError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("stream failed (status %d): %v", e.Status, e.Err)
	}
	return fmt.Sprintf("stream failed (status %d)", e.Status)
}

func (e *StreamError) Unwrap() error { return e.Err }

// errorCodeStatus maps the error codes OpenAI-style APIs send inside a stream
// to the HTTP status a client would have seen for the same failure.
var errorCodeStatus = map[string]int{
	"rate_limit_exceeded":     429,
	"slow_down":               429,
	"insufficient_quota":      429,
	"server_is_overloaded":    503,
	"invalid_api_key":         401,
	"model_not_found":         404,
	"context_length_exceeded": 400,
}

// errorStatus picks the status for a downstream error code: a number from 400
// to 599 is taken as is, a known string code is mapped, anything else is 502.
func errorStatus(code any) int {
	switch c := code.(type) {
	case float64:
		if c >= 400 && c <= 599 && c == math.Trunc(c) {
			return int(c)
		}
	case int:
		if c >= 400 && c <= 599 {
			return c
		}
	case string:
		if s, ok := errorCodeStatus[c]; ok {
			return s
		}
	}
	return 502
}

// streamErrorFromObject builds a StreamError from a downstream error object
// ({"message", "code", ...}). frame is the downstream's own event to forward
// once the router has committed, or nil.
func streamErrorFromObject(obj map[string]any, frame *Frame) *StreamError {
	msg := asString(obj, "message")
	prohibited := matchesProhibited(msg) || matchesProhibited(asString(obj, "status"))
	if code, ok := obj["code"]; ok && code != nil {
		prohibited = prohibited || matchesProhibited(fmt.Sprint(code))
	}
	if msg == "" {
		msg = "the provider reported an error"
	}
	return &StreamError{
		Status:     errorStatus(obj["code"]),
		Body:       mustJSON(map[string]any{"error": obj}),
		Frame:      frame,
		Prohibited: prohibited,
		Err:        errors.New(msg),
	}
}

// chatErrorFrame is the router's own error event on a Chat Completions stream.
func chatErrorFrame(status int, message string) Frame {
	return Frame{Data: mustJSON(map[string]any{
		"error": map[string]any{"message": message, "type": "upstream_error", "code": status},
	})}
}

// responsesErrorFrame is the router's own error event on a Responses stream.
func responsesErrorFrame(seq int, message string) Frame {
	return Frame{Event: "error", Data: mustJSON(map[string]any{
		"type": "error", "code": "server_error", "message": message, "sequence_number": seq,
	})}
}

// WantsUsage reports whether a streaming client asked for usage
// (stream_options.include_usage).
func WantsUsage(body map[string]any) bool {
	so, _ := body["stream_options"].(map[string]any)
	v, _ := so["include_usage"].(bool)
	return v
}

// rawLogLimit caps the downstream bytes kept for a failed attempt's log.
const rawLogLimit = 64 << 10

// rawLog keeps the first rawLogLimit bytes written to it.
type rawLog struct{ b []byte }

func (r *rawLog) write(p []byte) {
	room := rawLogLimit - len(r.b)
	if room <= 0 {
		return
	}
	if len(p) > room {
		p = p[:room]
	}
	r.b = append(r.b, p...)
}
