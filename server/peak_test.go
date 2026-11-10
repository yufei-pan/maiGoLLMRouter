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
// coercion copy runs). Set ROUTER_MEASURE_PEAK=<chat|responses>-<openai|
// anthropic|gemini> to run one case; each case needs its own process, because
// VmHWM only rises.
func TestMeasureWorstBodyPeak(t *testing.T) {
	c := os.Getenv("ROUTER_MEASURE_PEAK")
	if c == "" {
		t.Skip("set ROUTER_MEASURE_PEAK=<chat|responses>-<openai|anthropic|gemini> to measure one case")
	}
	endpoint, kind, _ := strings.Cut(c, "-")
	paths := map[string]string{"chat": "/v1/chat/completions", "responses": "/v1/responses"}
	fields := map[string]string{"chat": "messages", "responses": "input"}
	replies := map[string]string{
		"openai":    `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`,
		"anthropic": `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		"gemini":    `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`,
	}
	path, reply := paths[endpoint], replies[kind]
	if path == "" || reply == "" {
		t.Fatalf("ROUTER_MEASURE_PEAK=%q: want <chat|responses>-<openai|anthropic|gemini>", c)
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

	body := worstBody(fields[endpoint], 32<<20)
	runtime.GC()
	before := vmHWM(t)
	resp := post(t, ts, path, body)
	_, _ = io.Copy(io.Discard, resp.Body)
	deadline := time.Now().Add(time.Minute)
	for len(logs.Recent()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(logs.Recent()) == 0 {
		t.Fatal("the request was never logged, so its path did not finish")
	}
	after := vmHWM(t)
	t.Logf("PEAK case=%s body=%d delta_mib=%d status=%d upstream=%t", c, len(body), (after-before)>>20, resp.StatusCode, reached.Load())
}

// worstBody is a request whose field holds empty items up to just under
// limit bytes, then an audio clip and a trailing assistant turn.
func worstBody(field string, limit int) string {
	head := `{"model":"m","` + field + `":[`
	tail := `{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}]},{"role":"assistant","content":"x"}]}`
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
