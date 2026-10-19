package server

import (
	"net/http"

	"maiGoLLMRouter/provider"
)

// isStreamRequest reports a request that asked for a stream: chat or
// responses with "stream": true (a JSON boolean, not the string "true").
func isStreamRequest(op provider.Operation, body map[string]any) bool {
	if op != provider.OpChat && op != provider.OpResponses {
		return false
	}
	v, _ := body["stream"].(bool)
	return v
}

// sseSink writes a committed stream to the client as server-sent events,
// flushing after each one so every piece leaves as soon as it arrives.
type sseSink struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSESink(w http.ResponseWriter) *sseSink {
	return &sseSink{w: w, rc: http.NewResponseController(w)}
}

func (s *sseSink) Commit() error {
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // ask a reverse proxy in front not to buffer
	s.w.WriteHeader(http.StatusOK)
	return s.rc.Flush()
}

func (s *sseSink) Write(f provider.Frame) error {
	if _, err := s.w.Write(f.Encode()); err != nil {
		return err
	}
	return s.rc.Flush()
}
