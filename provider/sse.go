package provider

import (
	"bufio"
	"bytes"
	"io"
	"strings"
)

// Frame is one server-sent event for the client: an optional event name and
// one data payload. Event is empty for Chat Completions chunks, which carry no
// "event:" line.
type Frame struct {
	Event string
	Data  []byte
}

// Encode renders f as SSE bytes. Each line of Data becomes its own "data:"
// line, so a payload with newlines survives the trip.
func (f Frame) Encode() []byte {
	var b bytes.Buffer
	if f.Event != "" {
		b.WriteString("event: ")
		b.WriteString(f.Event)
		b.WriteByte('\n')
	}
	for line := range bytes.SplitSeq(f.Data, []byte("\n")) {
		b.WriteString("data: ")
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// sseEvent is one event read from a downstream stream.
type sseEvent struct {
	Event string
	Data  string
}

// sseReader reads events from a downstream body. It reads whole lines of any
// length (a bufio.Scanner would fail on a long tool-call delta), skips comment
// lines, and joins the data lines of one event with "\n".
type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReader(r)}
}

// Next returns the next event, or io.EOF when the body ends. An event cut off
// by the end of the body, without its blank line, is still returned first.
func (s *sseReader) Next() (sseEvent, error) {
	var ev sseEvent
	var data []string
	hasData := false
	for {
		line, err := s.r.ReadString('\n')
		if err != nil && err != io.EOF {
			return sseEvent{}, err
		}
		atEOF := err == io.EOF
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case line == "" && !atEOF:
			if hasData {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			ev = sseEvent{}
		case line == "" || strings.HasPrefix(line, ":"):
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "data":
				data = append(data, value)
				hasData = true
			case "event":
				ev.Event = value
			}
		}
		if atEOF {
			if hasData {
				ev.Data = strings.Join(data, "\n")
				return ev, nil
			}
			return sseEvent{}, io.EOF
		}
	}
}
