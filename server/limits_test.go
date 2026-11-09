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
