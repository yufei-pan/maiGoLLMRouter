package provider

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

const sseSample = ": keep-alive\r\nevent: response.created\r\ndata: {\"a\":1}\r\n\r\ndata: x\ndata: y\n\ndata: [DONE]"

var sseSampleEvents = []sseEvent{{"response.created", `{"a":1}`}, {"", "x\ny"}, {"", "[DONE]"}}

func readAllEvents(t *testing.T, r *sseReader) []sseEvent {
	t.Helper()
	var out []sseEvent
	for {
		ev, err := r.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, ev)
	}
}

func TestSSEReaderEvents(t *testing.T) {
	r := newSSEReader(strings.NewReader(sseSample))
	for _, want := range sseSampleEvents {
		got, err := r.Next()
		if err != nil || got != want {
			t.Fatalf("got %+v, %v; want %+v", got, err, want)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
}

func TestSSEReaderLongLine(t *testing.T) {
	long := strings.Repeat("a", 1<<20)
	got := readAllEvents(t, newSSEReader(strings.NewReader("data: "+long+"\n\n")))
	if len(got) != 1 || got[0].Data != long {
		t.Fatalf("got %d events, first data length %d; want one event of %d bytes", len(got), len(got[0].Data), len(long))
	}
}

func TestSSEReaderSplitReads(t *testing.T) {
	got := readAllEvents(t, newSSEReader(iotest.OneByteReader(strings.NewReader(sseSample))))
	if len(got) != len(sseSampleEvents) {
		t.Fatalf("got %+v, want %+v", got, sseSampleEvents)
	}
	for i := range got {
		if got[i] != sseSampleEvents[i] {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], sseSampleEvents[i])
		}
	}
}

func TestFrameEncode(t *testing.T) {
	cases := []struct {
		f    Frame
		want string
	}{
		{Frame{Data: []byte(`{"a":1}`)}, "data: {\"a\":1}\n\n"},
		{Frame{Event: "response.completed", Data: []byte("{}")}, "event: response.completed\ndata: {}\n\n"},
		{Frame{Data: []byte("x\ny")}, "data: x\ndata: y\n\n"},
	}
	for _, c := range cases {
		if got := string(c.f.Encode()); got != c.want {
			t.Errorf("Encode(%+v) = %q, want %q", c.f, got, c.want)
		}
	}
}
