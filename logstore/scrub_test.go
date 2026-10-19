package logstore

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestScrubAudioChat(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}]}]}`
	want := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"input_audio","input_audio":{"data":"<3 bytes audio/wav>","format":"wav"}}]}]}`
	var got, exp any
	if err := json.Unmarshal(ScrubAudio([]byte(in)), &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(want), &exp)
	if !reflect.DeepEqual(got, exp) {
		t.Errorf("scrubbed = %v\nwant %v", got, exp)
	}
}

func TestScrubAudioResponsesInput(t *testing.T) {
	in := `{"model":"m","input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"data:audio/ogg;base64,QUJDRA==","format":""}}]}]}`
	out := string(ScrubAudio([]byte(in)))
	if want := `"<4 bytes audio/ogg>"`; !contains(out, want) {
		t.Errorf("scrubbed = %s, want %s", out, want)
	}
}

func TestScrubAudioUntouched(t *testing.T) {
	in := []byte(`{"model":"m",  "messages":[{"role":"user","content":"hi"}]}`)
	if out := ScrubAudio(in); string(out) != string(in) {
		t.Errorf("a body without audio changed: %s", out)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
