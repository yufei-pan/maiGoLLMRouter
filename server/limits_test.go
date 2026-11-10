package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"maiGoLLMRouter/logstore"
)

// modelSenders post a request naming a model to each endpoint that takes one,
// transcription in both of its shapes.
var modelSenders = []struct {
	endpoint string
	send     func(t *testing.T, ts *httptest.Server, model string) *http.Response
}{
	{"chat", func(t *testing.T, ts *httptest.Server, model string) *http.Response {
		return post(t, ts, "/v1/chat/completions", `{"model":`+jsonString(model)+`,"messages":[{"role":"user","content":"hi"}]}`)
	}},
	{"embeddings", func(t *testing.T, ts *httptest.Server, model string) *http.Response {
		return post(t, ts, "/v1/embeddings", `{"model":`+jsonString(model)+`,"input":"hi"}`)
	}},
	{"responses", func(t *testing.T, ts *httptest.Server, model string) *http.Response {
		return post(t, ts, "/v1/responses", `{"model":`+jsonString(model)+`,"input":"hi"}`)
	}},
	{"speech", func(t *testing.T, ts *httptest.Server, model string) *http.Response {
		return post(t, ts, "/v1/audio/speech", `{"model":`+jsonString(model)+`,"input":"Hello"}`)
	}},
	{"transcription JSON", func(t *testing.T, ts *httptest.Server, model string) *http.Response {
		return post(t, ts, "/v1/audio/transcriptions", `{"model":`+jsonString(model)+`,"input_audio":{"data":"UklGRg==","format":"wav"}}`)
	}},
	{"transcription multipart", func(t *testing.T, ts *httptest.Server, model string) *http.Response {
		return postMultipart(t, ts, map[string]string{"model": model}, "speech.wav", "audio/wav", []byte("RIFFdata"))
	}},
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// modelServer runs the API with a log store and one OpenAI-kind provider "p"
// that counts the requests reaching it. routing goes in as a [routing] table.
func modelServer(t *testing.T, routing string) (*httptest.Server, *logstore.Store, *atomic.Int64) {
	t.Helper()
	hits := new(atomic.Int64)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"text":"hi"}`)
	}))
	t.Cleanup(backend.Close)
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return audioServer(t, "\n[routing]\n"+routing, openAIBlock(backend.URL), logs), logs, hits
}

// apiError decodes an error reply.
func apiError(t *testing.T, resp *http.Response) (msg, typ, code string) {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Errorf("%s: reply is not JSON: %v", resp.Request.URL.Path, err)
	}
	return body.Error.Message, body.Error.Type, body.Error.Code
}

func TestModelNameRefused(t *testing.T) {
	// A fallback provider takes any name, so only the check keeps these from it.
	ts, logs, hits := modelServer(t, `fallback_providers = ["p"]`)
	for _, name := range []string{strings.Repeat("m", 257), "a\nb", "a\x7fb", "a\u0085b"} {
		for _, s := range modelSenders {
			resp := s.send(t, ts, name)
			msg, typ, code := apiError(t, resp)
			if resp.StatusCode != http.StatusBadRequest || typ != "invalid_request_error" || code != "invalid_model" || !strings.Contains(msg, "256") {
				t.Errorf("%s, model %.20q: status %d, type %q, code %q, message %q", s.endpoint, name, resp.StatusCode, typ, code, msg)
			}
		}
	}
	ts.Close() // waits for every handler, so no log write is still on its way
	if n := len(logs.Recent()); n != 0 {
		t.Errorf("%d refused requests were logged", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the provider got %d refused requests", n)
	}
}

func TestModelNameAccepted(t *testing.T) {
	// No fallback provider: a name that passes the check meets the router's own
	// answer for a model it doesn't know.
	ts, _, _ := modelServer(t, "")
	for _, name := range []string{strings.Repeat("m", 256), "模型/名"} {
		for _, s := range modelSenders {
			resp := s.send(t, ts, name)
			msg, _, code := apiError(t, resp)
			if code == "invalid_model" || !strings.Contains(msg, "no fallback_providers configured") {
				t.Errorf("%s, model %.20q: status %d, code %q, message %q", s.endpoint, name, resp.StatusCode, code, msg)
			}
		}
	}
}

func TestCountExceeds(t *testing.T) {
	for _, c := range []struct {
		body   string
		values int
	}{
		{`{}`, 1},
		{`{"a":[1,2]}`, 4},
		{`[[],{}]`, 3},
		{`"x"`, 1},
		{`null`, 1},
		{`{"a":{"b":true,"c":null},"d":"e"}`, 5},
	} {
		var v any
		if err := json.Unmarshal([]byte(c.body), &v); err != nil {
			t.Fatal(err)
		}
		if got := countValues(v, 1000); got != c.values {
			t.Errorf("%s: %d values, want %d", c.body, got, c.values)
		}
		if countExceeds(v, c.values) || !countExceeds(v, c.values-1) {
			t.Errorf("%s: countExceeds is wrong at its %d values", c.body, c.values)
		}
	}
	for _, c := range []struct {
		values int
		over   bool
	}{{maxBodyValues, false}, {maxBodyValues + 1, true}} {
		var v any
		if err := json.Unmarshal([]byte(paddedBody("[", "]", 1, c.values)), &v); err != nil {
			t.Fatal(err)
		}
		if countExceeds(v, maxBodyValues) != c.over {
			t.Errorf("a body of %d values: over the cap = %t, want %t", c.values, !c.over, c.over)
		}
	}
	// 10,000,002 values; the rows share one backing array, so the value is small.
	row := make([]any, 9_999)
	rows := make([]any, 1_000)
	for i := range rows {
		rows[i] = row
	}
	if got := countValues(map[string]any{"rows": rows}, maxBodyValues); got != maxBodyValues+1 {
		t.Errorf("on 10 million values the walk visited %d, want it to stop at %d", got, maxBodyValues+1)
	}
}

// paddedBodies are a request to each endpoint with a JSON body, with an array
// to pad. Their model has no route, so a request that passes every check meets
// the router's answer for a model it doesn't know.
var paddedBodies = []struct {
	path, head, tail string
	fixed            int // values outside the padding, its array included
}{
	{"/v1/chat/completions", `{"model":"x","messages":[`, `]}`, 3},
	{"/v1/embeddings", `{"model":"x","input":[`, `]}`, 3},
	{"/v1/responses", `{"model":"x","input":[`, `]}`, 3},
	{"/v1/audio/speech", `{"model":"x","input":"Hello","pad":[`, `]}`, 4},
	{"/v1/audio/transcriptions", `{"model":"x","input_audio":{"data":"UklGRg==","format":"wav"},"pad":[`, `]}`, 6},
}

// paddedBody fills the array between head and tail with empty objects, so the
// body holds values JSON values in all; fixed are the ones outside the padding.
func paddedBody(head, tail string, fixed, values int) string {
	return head + strings.TrimSuffix(strings.Repeat("{},", values-fixed), ",") + tail
}

func TestTooManyValuesRefused(t *testing.T) {
	// A fallback provider takes any name, so only the check keeps these from it.
	ts, logs, hits := modelServer(t, `fallback_providers = ["p"]`)
	for _, b := range paddedBodies {
		resp := post(t, ts, b.path, paddedBody(b.head, b.tail, b.fixed, maxBodyValues+1))
		msg, typ, code := apiError(t, resp)
		if resp.StatusCode != http.StatusBadRequest || typ != "invalid_request_error" || code != "too_many_values" || msg != "request body has more than 100000 JSON values" {
			t.Errorf("%s: status %d, type %q, code %q, message %q", b.path, resp.StatusCode, typ, code, msg)
		}
	}
	ts.Close() // waits for every handler, so no log write is still on its way
	if n := len(logs.Recent()); n != 0 {
		t.Errorf("%d refused requests were logged", n)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the provider got %d refused requests", n)
	}
}

func TestValuesAtCapAccepted(t *testing.T) {
	ts, _, _ := modelServer(t, "")
	for _, b := range paddedBodies {
		resp := post(t, ts, b.path, paddedBody(b.head, b.tail, b.fixed, maxBodyValues))
		msg, _, code := apiError(t, resp)
		if code == "too_many_values" || !strings.Contains(msg, "no fallback_providers configured") {
			t.Errorf("%s: status %d, code %q, message %q", b.path, resp.StatusCode, code, msg)
		}
	}
}
