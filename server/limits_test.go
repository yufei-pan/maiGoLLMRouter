package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maiGoLLMRouter/inflight"
	"maiGoLLMRouter/logstore"
	"maiGoLLMRouter/router"
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

// budgetServer runs the API against a fake OpenAI-kind provider that answers
// chat, speech and transcription for model "m", and counts the requests
// reaching it. It returns the Server, so a test can replace its budget before
// the first request.
func budgetServer(t *testing.T) (*Server, *httptest.Server, *atomic.Int64) {
	t.Helper()
	hits := new(atomic.Int64)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/audio/speech"):
			w.Header().Set("Content-Type", "audio/mpeg")
			fmt.Fprint(w, "ID3")
		case strings.HasSuffix(r.URL.Path, "/audio/transcriptions"):
			fmt.Fprint(w, `{"text":"hi"}`)
		default:
			fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`)
		}
	}))
	t.Cleanup(backend.Close)
	cfg := loadCfg(t, `
[server]
client_keys = ["sk-test"]

[[provider]]
name = "p"
keys = ["sk-upstream"]
`+openAIBlock(backend.URL)+`

[model."m"]
targets = ["p/real"]
`)
	srv := New(cfg, router.New(cfg), nil, inflight.New())
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(srv.WithCORS(mux))
	t.Cleanup(ts.Close)
	return srv, ts, hits
}

// budgetSenders post a small request to each body reader that admits through
// the budget.
var budgetSenders = []struct {
	endpoint string
	send     func(t *testing.T, ts *httptest.Server) *http.Response
}{
	{"chat", func(t *testing.T, ts *httptest.Server) *http.Response {
		return post(t, ts, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	}},
	{"speech", func(t *testing.T, ts *httptest.Server) *http.Response {
		return post(t, ts, "/v1/audio/speech", `{"model":"m","input":"Hello"}`)
	}},
	{"transcription multipart", func(t *testing.T, ts *httptest.Server) *http.Response {
		return postMultipart(t, ts, map[string]string{"model": "m"}, "speech.wav", "audio/wav", []byte("RIFFdata"))
	}},
	{"transcription JSON", func(t *testing.T, ts *httptest.Server) *http.Response {
		return post(t, ts, "/v1/audio/transcriptions", `{"model":"m","input_audio":{"data":"UklGRg==","format":"wav"}}`)
	}},
}

func TestBusyAfterWait(t *testing.T) {
	srv, ts, hits := budgetServer(t)
	srv.budget = newBudget(1024)
	srv.budgetWait = 100 * time.Millisecond
	release, _ := srv.budget.acquire(t.Context(), 1024, 0)
	defer release()
	for _, s := range budgetSenders {
		start := time.Now()
		resp := s.send(t, ts)
		took := time.Since(start)
		msg, typ, code := apiError(t, resp)
		if resp.StatusCode != http.StatusServiceUnavailable || typ != "server_error" || code != "busy" ||
			msg != "the router is busy with other large requests; retry shortly" || resp.Header.Get("Retry-After") != "5" {
			t.Errorf("%s: status %d, type %q, code %q, message %q, Retry-After %q", s.endpoint, resp.StatusCode, typ, code, msg, resp.Header.Get("Retry-After"))
		}
		if took < 100*time.Millisecond {
			t.Errorf("%s: answered after %v, before the 100 ms wait ran out", s.endpoint, took)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the provider got %d requests the budget never admitted", n)
	}
}

// lockedBuffer is a log sink the test may read while handlers write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestBusyIsJournaled verifies each 503 busy leaves one journal line with the
// body size and the wait, and nothing the client supplied.
func TestBusyIsJournaled(t *testing.T) {
	srv, ts, _ := budgetServer(t)
	srv.budget = newBudget(1024)
	srv.budgetWait = 50 * time.Millisecond
	release, _ := srv.budget.acquire(t.Context(), 1024, 0)
	defer release()
	var logged lockedBuffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	for _, s := range budgetSenders {
		s.send(t, ts)
	}
	line := regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d budget: busy after [0-9.]+m?s waiting for [1-9]\d* bytes$`)
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != len(budgetSenders) {
		t.Fatalf("%d journal lines for %d refused requests:\n%s", len(lines), len(budgetSenders), logged.String())
	}
	for _, l := range lines {
		if !line.MatchString(l) {
			t.Errorf("journal line %q does not match %v", l, line)
		}
	}
}

func TestBudgetWaitThenServe(t *testing.T) {
	srv, ts, _ := budgetServer(t)
	srv.budget = newBudget(1024)
	srv.budgetWait = 2 * time.Second
	release, _ := srv.budget.acquire(t.Context(), 1024, 0)
	start := time.Now()
	time.AfterFunc(50*time.Millisecond, release)
	resp := budgetSenders[0].send(t, ts)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"content":"hi"`) {
		t.Errorf("status %d, body %s", resp.StatusCode, body)
	}
	if took := time.Since(start); took < 50*time.Millisecond {
		t.Errorf("answered after %v, before the budget had room", took)
	}
}

func TestStalledUploadHoldsNoBudget(t *testing.T) {
	srv, ts, _ := budgetServer(t)
	srv.budget = newBudget(1024)
	srv.budgetWait = 2 * time.Second // so a regression fails with 503 in 2 s, not 25
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Expect: 100-continue makes the server say when the handler first reads
	// the body, so the second request goes only once the first is reading.
	fmt.Fprint(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: router\r\nAuthorization: Bearer sk-test\r\n"+
		"Content-Type: application/json\r\nContent-Length: 1000\r\nExpect: 100-continue\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || !strings.HasPrefix(line, "HTTP/1.1 100 ") {
		t.Fatalf("waiting for 100 Continue: %q, %v", line, err)
	}
	fmt.Fprint(conn, `{"model":"`) // 10 of the 1000 bytes, then nothing

	head, tail := `{"model":"m","messages":[{"role":"user","content":"`, `"}]}`
	body := head + strings.Repeat("x", 900-len(head)-len(tail)) + tail
	start := time.Now()
	resp := post(t, ts, "/v1/chat/completions", body)
	took := time.Since(start)
	if resp.StatusCode != http.StatusOK || took > time.Second {
		t.Errorf("a 900-byte chat beside a stalled 1000-byte upload: status %d after %v, want 200 within 1 s", resp.StatusCode, took)
	}
}

func TestBudgetReleasedAtRequestEnd(t *testing.T) {
	srv, ts, _ := budgetServer(t)
	srv.budget = newBudget(1024)
	for _, s := range budgetSenders {
		resp := s.send(t, ts)
		_, _ = io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", s.endpoint, resp.StatusCode)
		}
	}
	ts.Close() // waits for every handler, so each has returned
	release, ok := srv.budget.acquire(t.Context(), 1024, 0)
	if !ok {
		t.Fatalf("%d bytes still held after every request ended", held(srv.budget))
	}
	release()
}
