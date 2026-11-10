package server

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"maiGoLLMRouter/inflight"
	"maiGoLLMRouter/logstore"
	"maiGoLLMRouter/router"
)

// TestMeasureWorstBodyPeak measures how far one worst-case request raises the
// process's peak memory: a body just under the 32 MB cap made of ~11 million
// empty items, which costs the most memory per byte once decoded, with an
// audio clip (so the log scrub runs) and a trailing assistant turn (so the
// coercion copy runs). The value cap now refuses that body right after it is
// decoded. Two suffixes send bodies the cap lets through: "-capped" sends
// cappedBody, the costliest shape of plain messages, and "-args" sends
// argsBody, whose tool call arguments conversion parses past the cap. Set
// ROUTER_MEASURE_PEAK=<chat|responses>-<openai|anthropic|gemini>[-capped|-args]
// to run one case; each case needs its own process, because VmHWM only rises.
func TestMeasureWorstBodyPeak(t *testing.T) {
	c := os.Getenv("ROUTER_MEASURE_PEAK")
	if c == "" {
		t.Skip("set ROUTER_MEASURE_PEAK=<chat|responses>-<openai|anthropic|gemini>[-capped|-args] to measure one case")
	}
	endpoint, rest, _ := strings.Cut(c, "-")
	kind, variant, _ := strings.Cut(rest, "-")
	paths := map[string]string{"chat": "/v1/chat/completions", "responses": "/v1/responses"}
	fields := map[string]string{"chat": "messages", "responses": "input"}
	replies := map[string]string{
		"openai":    `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`,
		"anthropic": `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		"gemini":    `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`,
	}
	path, reply := paths[endpoint], replies[kind]
	bodies := map[string]func() string{
		"": func() string { return worstBody(fields[endpoint], 32<<20) },
		// A Responses request translated to Chat must be portable, and an
		// input_audio part isn't, so only the direct cases keep the clip.
		"capped": func() string { return cappedBody(fields[endpoint], 32<<20, endpoint == "chat" || kind == "openai") },
		"args":   func() string { return argsBody(endpoint, 32<<20) },
	}
	makeBody := bodies[variant]
	if path == "" || reply == "" || makeBody == nil {
		t.Fatalf("ROUTER_MEASURE_PEAK=%q: want <chat|responses>-<openai|anthropic|gemini>[-capped|-args]", c)
	}
	if endpoint == "responses" && kind == "openai" {
		reply = `{"id":"r1","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`
	}

	var reached atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, reply)
	}))
	defer backend.Close()

	extra := ""
	if kind == "openai" {
		extra = "supports_responses = true" // Responses goes direct
	}
	cfg := loadCfg(t, fmt.Sprintf(`
[server]
client_keys = ["sk-test"]
coerce_trailing_assistant = true

[[provider]]
name = %q
kind = %q
base_url = %q
keys = ["sk-upstream"]
%s

[model."m"]
targets = ["%s/real"]
`, kind, kind, backend.URL, extra, kind))
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, router.New(cfg), logs, inflight.New())
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(srv.WithCORS(mux))
	defer ts.Close()

	body := makeBody()
	runtime.GC()
	before := vmHWM(t)
	resp := post(t, ts, path, body)
	_, _, code := apiError(t, resp) // reads the whole reply
	// A refused request is never logged; its handler is done once it replies.
	if code != "too_many_values" {
		deadline := time.Now().Add(time.Minute)
		for len(logs.Recent()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if len(logs.Recent()) == 0 {
			t.Fatal("the request was never logged, so its path did not finish")
		}
	}
	after := vmHWM(t)
	t.Logf("PEAK case=%s body=%d delta_mib=%d status=%d code=%q upstream=%t", c, len(body), (after-before)>>20, resp.StatusCode, code, reached.Load())
}

// The turns that end a measured body: an audio clip (8 JSON values) and an
// assistant turn (3).
const (
	clipTurn      = `{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}]}`
	assistantTurn = `{"role":"assistant","content":"x"}`
)

// worstBody is a request whose field holds empty items up to just under
// limit bytes, then an audio clip and a trailing assistant turn.
func worstBody(field string, limit int) string {
	head := `{"model":"m","` + field + `":[`
	tail := clipTurn + "," + assistantTurn + "]}"
	n := (limit - 1 - len(head) - len(tail)) / len("{},")
	var b strings.Builder
	b.Grow(len(head) + n*len("{},") + len(tail))
	b.WriteString(head)
	for range n {
		b.WriteString("{},")
	}
	b.WriteString(tail)
	return b.String()
}

// cappedBody is a request with as many {"role":"user","content":…} items (3
// JSON values each) as fit under maxBodyValues, their content padded with "<"
// so the body is just under limit bytes, then the audio clip when clip is set
// and a trailing assistant turn. "<" is one byte in the body but six
// ("\u003c") in every copy json.Marshal makes of it: the body sent upstream
// and the log entry. Measured 2026-11-09, it costs ~3x what "x" does and more
// than invalid UTF-8, which decodes to a three-byte U+FFFD.
func cappedBody(field string, limit int, clip bool) string {
	head := `{"model":"m","` + field + `":[` // 3 values: the object, "m", the array
	tail, fixed := assistantTurn+"]}", 3+3
	if clip {
		tail, fixed = clipTurn+","+tail, fixed+8
	}
	n := (maxBodyValues - fixed) / 3
	open, end := `{"role":"user","content":"`, `"},`
	pad := strings.Repeat("<", (limit-1-len(head)-len(tail)-n*len(open+end))/n)
	var b strings.Builder
	b.Grow(limit)
	b.WriteString(head)
	for range n {
		b.WriteString(open)
		b.WriteString(pad)
		b.WriteString(end)
	}
	b.WriteString(tail)
	return b.String()
}

// argsBody is a request of under 25 JSON values: a user turn, an assistant
// tool call whose arguments string holds ~11 million empty objects as JSON
// text, the tool's result and a user turn. Anthropic and Gemini conversion
// parse the arguments into Go maps, which the value cap never counts. On those
// kinds this tree measured costlier (2026-11-09) than arguments of one long
// "<" string.
func argsBody(endpoint string, limit int) string {
	head := `{"model":"m","messages":[{"role":"user","content":"call f"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":[`
	tail := `{}]}"}}]},{"role":"tool","tool_call_id":"c1","content":"ok"},{"role":"user","content":"go on"}]}`
	if endpoint == "responses" {
		head = `{"model":"m","input":[{"role":"user","content":"call f"},{"type":"function_call","call_id":"c1","name":"f","arguments":"{\"a\":[`
		tail = `{}]}"},{"type":"function_call_output","call_id":"c1","output":"ok"},{"role":"user","content":"go on"}]}`
	}
	n := (limit - 1 - len(head) - len(tail)) / len("{},")
	var b strings.Builder
	b.Grow(len(head) + n*len("{},") + len(tail))
	b.WriteString(head)
	for range n {
		b.WriteString("{},")
	}
	b.WriteString(tail)
	return b.String()
}

// vmHWM is the process's peak resident memory so far, in bytes.
func vmHWM(t *testing.T) int64 {
	t.Helper()
	f, err := os.Open("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "VmHWM:"); ok {
			kb, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "kB")), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return kb << 10
		}
	}
	t.Fatal("no VmHWM in /proc/self/status")
	return 0
}
