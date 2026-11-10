package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"maiGoLLMRouter/inflight"
	"maiGoLLMRouter/provider"
	"maiGoLLMRouter/router"
)

const okChat = `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`

const authHeader = "Authorization: Bearer sk-test"

// testListener hands the server each connection as a testConn with a 16 KiB
// send buffer, so a client that stops reading blocks the server's writes
// after little data.
type testListener struct{ net.Listener }

func (l testListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := c.(*net.TCPConn)
	_ = tc.SetWriteBuffer(16 << 10)
	return testConn{tc}, nil
}

// testConn yields for a moment after a read deadline at or before now, so the
// background read that deadline fails runs first, as it may on a loaded host.
// Without that, finishRequest's abortPendingRead usually marks the read
// aborted before it runs, and hides what the deadline did. The yield makes
// that order likely, not certain: on a loaded host the woken read may still
// run late, so a regression of that kind is caught by most runs, not all.
type testConn struct{ *net.TCPConn }

func (c testConn) SetReadDeadline(t time.Time) error {
	err := c.TCPConn.SetReadDeadline(t)
	if !t.IsZero() && !t.After(time.Now()) {
		time.Sleep(5 * time.Millisecond)
	}
	return err
}

// startServer serves NewHTTPServer(h, lim) on a loopback port and returns its address.
func startServer(t *testing.T, h http.Handler, lim ConnLimits) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewHTTPServer(h, lim)
	go func() { _ = srv.Serve(testListener{ln}) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// fullHandler is the API as main hands it to NewHTTPServer (srv.WithCORS
// around the registered mux), serving model "m" from a fake OpenAI-kind
// provider. The provider answers transcriptions with {"text":"hi"} and
// everything else with chat, or with okChat when chat is nil.
func fullHandler(t *testing.T, chat http.HandlerFunc) http.Handler {
	t.Helper()
	if chat == nil {
		chat = func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, okChat) }
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/audio/transcriptions") {
			fmt.Fprint(w, `{"text":"hi"}`)
			return
		}
		chat(w, r)
	}))
	t.Cleanup(backend.Close)
	cfg := loadCfg(t, fmt.Sprintf(`
[server]
client_keys = ["sk-test"]

[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["sk-upstream"]

[model."m"]
targets = ["openai/real-model"]
`, backend.URL))
	srv := New(cfg, router.New(cfg), nil, inflight.New())
	mux := http.NewServeMux()
	srv.Register(mux)
	return srv.WithCORS(mux)
}

// eventStream is a fake provider stream: script sends each event with send,
// which flushes it at once.
func eventStream(script func(send func(data string))) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		script(func(data string) {
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		})
	}
}

// rawClient writes requests by hand on one connection.
type rawClient struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
}

func dialRaw(t *testing.T, addr string) *rawClient {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &rawClient{t: t, c: c, br: bufio.NewReader(c)}
}

func (c *rawClient) send(s string) {
	c.t.Helper()
	if _, err := io.WriteString(c.c, s); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

// response reads one response and its body, failing if they take longer than within.
func (c *rawClient) response(within time.Duration) (*http.Response, string) {
	c.t.Helper()
	_ = c.c.SetReadDeadline(time.Now().Add(within))
	resp, err := http.ReadResponse(c.br, nil)
	if err != nil {
		c.t.Fatalf("no response within %v: %v", within, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("response body: %v", err)
	}
	return resp, string(body)
}

// expectClosed checks that the server closes the connection within d.
func (c *rawClient) expectClosed(within time.Duration) {
	c.t.Helper()
	_ = c.c.SetReadDeadline(time.Now().Add(within))
	if _, err := c.br.ReadByte(); err != io.EOF {
		c.t.Errorf("next read: %v, want EOF within %v", err, within)
	}
}

// request renders an HTTP/1.1 request; each header is a "Name: value" line.
func request(method, path, body string, headers ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: router.test\r\n", method, path)
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

// chunked is s as one chunk and the last chunk.
func chunked(s string) string { return fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(s), s) }

func contentLength(body string) string { return fmt.Sprintf("Content-Length: %d", len(body)) }

// multipartUpload is a transcription upload of model "m" and a file of size
// bytes, which comes last, and its Content-Type.
func multipartUpload(t *testing.T, size int) (body, contentType string) {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("model", "m")
	fw, err := mw.CreateFormFile("file", "speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(bytes.Repeat([]byte("R"), size))
	_ = mw.Close()
	return b.String(), mw.FormDataContentType()
}

// postKey posts body to url with the client key.
func postKey(t *testing.T, url, contentType, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestUnreadBodyClosesConn(t *testing.T) {
	addr := startServer(t, fullHandler(t, nil), DefaultConnLimits)
	for _, tc := range []struct {
		name, method, path string
		headers            []string
		status             int
	}{
		{"no auth", "POST", "/v1/chat/completions", nil, 401},
		{"bad key", "POST", "/v1/chat/completions", []string{"Authorization: Bearer sk-wrong"}, 401},
		{"POST /healthz", "POST", "/healthz", nil, 200},
		{"POST /nope", "POST", "/nope", nil, 404},
		{"preflight", "OPTIONS", "/v1/chat/completions", []string{"Origin: https://app.example", "Access-Control-Request-Method: POST"}, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dialRaw(t, addr)
			// The body is announced and never sent.
			c.send(request(tc.method, tc.path, "", slices.Concat(tc.headers, []string{"Transfer-Encoding: chunked"})...))
			resp, body := c.response(time.Second)
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d %s, want %d", resp.StatusCode, body, tc.status)
			}
			if !resp.Close {
				t.Error("the reply does not say Connection: close")
			}
			c.expectClosed(time.Second)
		})
	}
}

// A handler that answers past net/http's 2 KB response buffer (GET
// /v1/models with a long model list, a UI page), or flushes, sends its header
// mid-handler. With the body unread, the reply says Connection: close and the
// connection closes: net/http must not drain the body itself then, around
// eofBody, as a drain that reached EOF would start the background read that
// the wrapper's unread-body deadline then fails, and the next request would
// start cancelled. With the body read to EOF, the connection stays, with a
// live context.
func TestHeaderCommittedMidHandler(t *testing.T) {
	addr := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/ctx":
			fmt.Fprint(w, r.Context().Err())
			return
		case q.Has("read"):
			_, _ = io.ReadAll(r.Body)
		case q.Has("flush"):
			_ = http.NewResponseController(w).Flush()
			_, _ = io.WriteString(w, "ok")
			return
		case q.Has("status"): // as handleModels does
			w.WriteHeader(http.StatusOK)
		}
		_, _ = w.Write(make([]byte, 8<<10))
	}), DefaultConnLimits)

	for _, tc := range []struct {
		name, path string
		read       bool
	}{
		{"unread, large reply", "/big", false},
		{"unread, flushed first", "/big?flush=1", false},
		{"unread, WriteHeader then large reply", "/big?status=1", false},
		{"read, large reply", "/big?read=1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dialRaw(t, addr)
			c.send(request("POST", tc.path, "hello", "Content-Length: 5"))
			resp, _ := c.response(time.Second)
			if resp.StatusCode != 200 || resp.Close == tc.read {
				t.Fatalf("status %d, Connection: close %v; want 200 and close %v", resp.StatusCode, resp.Close, !tc.read)
			}
			if !tc.read {
				c.expectClosed(time.Second)
				return
			}
			c.send(request("GET", "/ctx", ""))
			if _, body := c.response(time.Second); body != "<nil>" {
				t.Errorf("the next request on the connection started with context error %q", body)
			}
		})
	}
}

// A client that reuses connections, as a reverse proxy in front of the router
// does, must be told when a refusal closes one: a proxied body can't be
// replayed, so a request sent into a connection the server is closing fails
// with EOF instead of being retried.
func TestRefusedUploadsReuseCleanly(t *testing.T) {
	addr := startServer(t, fullHandler(t, nil), DefaultConnLimits)
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	const n = 300 // the reviewer's repro failed ~6% of 600 when the close went unsaid
	failed := 0
	var last error
	for range n {
		// No GetBody: like a proxied body, it can't be replayed.
		body := io.NopCloser(bytes.NewReader(make([]byte, 60_000)))
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions", body)
		req.ContentLength = 60_000
		resp, err := client.Do(req) // no key: 401 without reading the body
		if err != nil {
			failed++
			last = err
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("status %d, want 401", resp.StatusCode)
		}
	}
	if failed > 0 {
		t.Errorf("%d of %d requests failed (last: %v)", failed, n, last)
	}
}

func TestKeepAliveAfterReadBodyKeepsContext(t *testing.T) {
	addr := startServer(t, fullHandler(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // long enough for a cancelled context to show
		fmt.Fprint(w, okChat)
	}), DefaultConnLimits)
	chat := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	c := dialRaw(t, addr)
	for i, req := range []string{
		request("POST", "/v1/chat/completions", chat, authHeader, "Content-Type: application/json", contentLength(chat)),
		request("POST", "/v1/chat/completions", chunked(chat), authHeader, "Content-Type: application/json", "Transfer-Encoding: chunked"),
		request("GET", "/v1/models", "", authHeader),
		// A request without a body must leave the connection's context alone too.
		request("POST", "/v1/chat/completions", chat, authHeader, "Content-Type: application/json", contentLength(chat)),
	} {
		c.send(req)
		if resp, body := c.response(5 * time.Second); resp.StatusCode != 200 {
			t.Fatalf("request %d on the connection: status %d %s", i+1, resp.StatusCode, body)
		}
	}
}

func TestBodyDeadline(t *testing.T) {
	lim := DefaultConnLimits
	lim.Body = 200 * time.Millisecond
	addr := startServer(t, fullHandler(t, nil), lim)

	t.Run("chat", func(t *testing.T) {
		c := dialRaw(t, addr)
		c.send(request("POST", "/v1/chat/completions", `{"model":"`, authHeader, "Content-Type: application/json", "Content-Length: 100"))
		if resp, body := c.response(time.Second); resp.StatusCode != 400 {
			t.Fatalf("status %d %s, want 400", resp.StatusCode, body)
		}
		c.expectClosed(time.Second)
	})
	t.Run("multipart", func(t *testing.T) {
		body, ct := multipartUpload(t, 64<<10)
		c := dialRaw(t, addr)
		c.send(request("POST", "/v1/audio/transcriptions", body[:len(body)/2], authHeader, "Content-Type: "+ct, contentLength(body)))
		if resp, body := c.response(time.Second); resp.StatusCode != 400 {
			t.Fatalf("status %d %s, want 400", resp.StatusCode, body)
		}
	})
}

func TestStreamOutlivesBodyDeadline(t *testing.T) {
	lim := DefaultConnLimits
	lim.Body = 100 * time.Millisecond
	addr := startServer(t, fullHandler(t, eventStream(func(send func(string)) {
		send(tRole)
		for range 5 {
			time.Sleep(100 * time.Millisecond)
			send(tContent)
		}
		send(tFinish)
		send("[DONE]")
	})), lim)
	resp := postKey(t, "http://"+addr+"/v1/chat/completions", "application/json", streamChat)
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("status %d, read error %v, body %q", resp.StatusCode, err, body)
	}
	if n := strings.Count(string(body), `"content":"Hi"`); n != 5 || !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
		t.Errorf("got %d content events, body %q", n, body)
	}
}

func TestStreamPauseLongerThanWriteDeadline(t *testing.T) {
	lim := DefaultConnLimits
	lim.Write = 100 * time.Millisecond
	addr := startServer(t, fullHandler(t, eventStream(func(send func(string)) {
		send(tRole)
		send(tContent)
		time.Sleep(400 * time.Millisecond) // a model thinking
		send(tFinish)
		send("[DONE]")
	})), lim)
	resp := postKey(t, "http://"+addr+"/v1/chat/completions", "application/json", streamChat)
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("status %d, read error %v, body %q", resp.StatusCode, err, body)
	}
	s := string(body)
	if !strings.Contains(s, `"content":"Hi"`) || !strings.Contains(s, `"finish_reason":"stop"`) || !strings.HasSuffix(s, "data: [DONE]\n\n") {
		t.Errorf("body %q", body)
	}
}

func TestWriteStallCutsOff(t *testing.T) {
	lim := DefaultConnLimits
	lim.Write = 200 * time.Millisecond
	for _, tc := range []struct {
		name   string
		handle func(w http.ResponseWriter) error
	}{
		{"plain", func(w http.ResponseWriter) error {
			_, err := w.Write(make([]byte, 4<<20))
			return err
		}},
		{"stream", func(w http.ResponseWriter) error {
			sink := newSSESink(w)
			if err := sink.Commit(); err != nil {
				return err
			}
			frame := provider.Frame{Data: bytes.Repeat([]byte("x"), 1<<10)}
			for {
				if err := sink.Write(frame); err != nil {
					return err
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type result struct {
				err, ctxErr error
				took        time.Duration
			}
			done := make(chan result, 1)
			addr := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				start := time.Now()
				err := tc.handle(w)
				done <- result{err: err, ctxErr: r.Context().Err(), took: time.Since(start)}
			}), lim)
			c := dialRaw(t, addr)
			_ = c.c.(*net.TCPConn).SetReadBuffer(16 << 10)
			c.send(request("GET", "/", ""))
			_ = c.c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if line, err := c.br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "HTTP/1.1 200") {
				t.Fatalf("status line %q, %v", line, err)
			}
			// The client reads nothing more.
			select {
			case res := <-done:
				if res.err == nil || res.took > 2*time.Second {
					t.Errorf("write returned %v after %v, want an error within 2s", res.err, res.took)
				}
				if res.ctxErr == nil {
					t.Error("the request context is still live after the write failed")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the write never failed")
			}
		})
	}
}

func TestSlowReaderGetsAll(t *testing.T) {
	lim := DefaultConnLimits
	// Each 64 KiB piece takes this reader about 80 ms; 500 ms leaves room for
	// a loaded host, and is still well under the 1.3 s the whole response
	// takes, so one deadline for the whole write would cut it off.
	lim.Write = 500 * time.Millisecond
	want := make([]byte, 1<<20)
	for i := range want {
		want[i] = byte(i % 251)
	}
	addr := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(want)
	}), lim)
	c := dialRaw(t, addr)
	// A bounded receive buffer, so the whole-write case above fails here
	// without depending on the kernel's autotuning. Not 16 KiB as in
	// TestWriteStallCutsOff: a window under loopback's 64 KiB segment leaves
	// the sender on persist-timer probes (200 ms and up), so a steady reader
	// looks stalled (it failed 5 of 5 runs; 32 KiB and up passed).
	_ = c.c.(*net.TCPConn).SetReadBuffer(64 << 10)
	c.send(request("GET", "/", ""))
	_ = c.c.SetReadDeadline(time.Now().Add(30 * time.Second))
	resp, err := http.ReadResponse(c.br, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	buf := make([]byte, 16<<10)
	for {
		n, err := io.ReadFull(resp.Body, buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %d bytes, want %d intact", len(got), len(want))
	}
}

func TestFinalFlushAfterSlowHandlerTail(t *testing.T) {
	lim := DefaultConnLimits
	lim.Write = 100 * time.Millisecond
	addr := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
		time.Sleep(300 * time.Millisecond) // s.log compressing a large entry
	}), lim)
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "ok" {
		t.Errorf("body %q, %v", body, err)
	}
}

// A flush gets its own deadline: one left by an earlier write has long passed.
func TestFlushAfterPause(t *testing.T) {
	lim := DefaultConnLimits
	lim.Write = 100 * time.Millisecond
	flushed := make(chan error, 1)
	addr := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
		time.Sleep(300 * time.Millisecond)
		flushed <- http.NewResponseController(w).Flush()
	}), lim)
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "ok" {
		t.Errorf("body %q, %v", body, err)
	}
	if err := <-flushed; err != nil {
		t.Errorf("flush after the pause: %v", err)
	}
}

func TestDeclaredSizeTooLarge(t *testing.T) {
	addr := startServer(t, fullHandler(t, nil), DefaultConnLimits)

	c := dialRaw(t, addr)
	c.send(request("POST", "/v1/chat/completions", "", "Content-Type: application/json", "Content-Length: 33554433"))
	resp, body := c.response(time.Second)
	var e struct {
		Error struct{ Message, Type, Code string }
	}
	_ = json.Unmarshal([]byte(body), &e)
	if resp.StatusCode != 413 || e.Error.Code != "body_too_large" || e.Error.Type != "invalid_request_error" || e.Error.Message != "request body is larger than 32 MB" {
		t.Errorf("status %d %s", resp.StatusCode, body)
	}
	if !resp.Close {
		t.Error("the 413 does not say Connection: close")
	}

	c = dialRaw(t, addr)
	c.send(request("POST", "/v1/chat/completions", "", "Content-Type: application/json", "Content-Length: 33554432"))
	if resp, body := c.response(time.Second); resp.StatusCode != 401 || !resp.Close {
		t.Errorf("at the cap: status %d %s, Connection: close %v; want 401 and close", resp.StatusCode, body, resp.Close)
	}
}

// sizedJSON is a JSON object of exactly size bytes (a string field pads it) that
// starts with head, which must be the object's opening up to a field's start.
func sizedJSON(head string, size int) []byte {
	const tail = `"}`
	pad := size - len(head) - len(tail)
	return []byte(head + strings.Repeat("a", pad) + tail)
}

// postChunked sends body as one chunked request, as a client that doesn't
// state a length does, and reads the reply.
func postChunked(t *testing.T, addr, path, contentType string, body []byte) (*http.Response, string) {
	t.Helper()
	c := dialRaw(t, addr)
	c.send(request("POST", path, "", authHeader, "Content-Type: "+contentType, "Transfer-Encoding: chunked"))
	c.send(fmt.Sprintf("%x\r\n", len(body)))
	c.send(string(body))
	c.send("\r\n0\r\n\r\n")
	return c.response(10 * time.Second)
}

// TestChunkedBodyOverCap verifies a body that doesn't state its length gets the
// same answer as one that does when it is over 32 MiB: 413 body_too_large, not a
// 400 about JSON cut off at the cap. Exactly 32 MiB is not refused for size.
func TestChunkedBodyOverCap(t *testing.T) {
	log.SetOutput(io.Discard) // the fake provider's reply to a speech request makes the router warn
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	addr := startServer(t, fullHandler(t, nil), DefaultConnLimits)
	zero, _ := multipartUpload(t, 0) // the overhead of an upload, whatever its file's size
	jsonBody := func(head string) func(int) ([]byte, string) {
		return func(n int) ([]byte, string) { return sizedJSON(head, n), "application/json" }
	}
	cases := []struct {
		name, path string
		body       func(total int) (body []byte, contentType string)
	}{
		{"chat", "/v1/chat/completions", jsonBody(`{"model":"m","messages":[{"role":"user","content":"hi"}],"pad":"`)},
		{"speech", "/v1/audio/speech", jsonBody(`{"model":"m","input":"hi","pad":"`)},
		{"JSON transcription", "/v1/audio/transcriptions", jsonBody(`{"model":"m","input_audio":{"data":"UklGRg==","format":"wav"},"pad":"`)},
		{"multipart transcription", "/v1/audio/transcriptions", func(n int) ([]byte, string) {
			b, ct := multipartUpload(t, n-len(zero))
			return []byte(b), ct
		}},
	}
	for _, c := range cases {
		for _, over := range []bool{true, false} {
			total := maxBody
			if over {
				total++
			}
			t.Run(fmt.Sprintf("%s over=%v", c.name, over), func(t *testing.T) {
				body, ct := c.body(total)
				if len(body) != total {
					t.Fatalf("built %d bytes, want %d", len(body), total)
				}
				resp, reply := postChunked(t, addr, c.path, ct, body)
				checkBodySize(t, resp, reply, over)
			})
		}
	}
}

// checkBodySize checks a reply is the 413 for an oversized body, or, for one
// at the cap, anything else.
func checkBodySize(t *testing.T, resp *http.Response, body string, over bool) {
	t.Helper()
	var e struct {
		Error struct{ Message, Type, Code string }
	}
	_ = json.Unmarshal([]byte(body), &e)
	if !over {
		if resp.StatusCode == 413 || e.Error.Code == "body_too_large" {
			t.Errorf("a body of exactly 32 MiB was refused for size: %d %s", resp.StatusCode, body)
		}
		return
	}
	if resp.StatusCode != 413 || e.Error.Code != "body_too_large" || e.Error.Type != "invalid_request_error" || e.Error.Message != "request body is larger than 32 MB" {
		t.Errorf("status %d %s, want the 413 body_too_large", resp.StatusCode, body)
	}
}

func TestIdleTimeout(t *testing.T) {
	lim := DefaultConnLimits
	lim.Idle = 200 * time.Millisecond
	addr := startServer(t, fullHandler(t, nil), lim)
	c := dialRaw(t, addr)
	c.send(request("GET", "/healthz", ""))
	if resp, body := c.response(time.Second); resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, body)
	}
	c.expectClosed(time.Second)
}

func TestExpectContinueUpload(t *testing.T) {
	addr := startServer(t, fullHandler(t, nil), DefaultConnLimits)
	// Long enough that an upload sent without the server's 100 Continue shows.
	const continueWait = 10 * time.Second
	client := &http.Client{Transport: &http.Transport{ExpectContinueTimeout: continueWait}}
	t.Cleanup(client.CloseIdleConnections)
	upload, ct := multipartUpload(t, 2<<20)
	send := func(key bool) (status int, body string, took time.Duration) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/v1/audio/transcriptions", strings.NewReader(upload))
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Expect", "100-continue")
		if key {
			req.Header.Set("Authorization", "Bearer sk-test")
		}
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), time.Since(start)
	}
	if status, body, took := send(true); status != 200 || body != `{"text":"hi"}` || took >= continueWait/2 {
		t.Errorf("with a key: status %d %s after %v", status, body, took)
	}
	if status, body, took := send(false); status != 401 || took > time.Second {
		t.Errorf("without a key: status %d %s after %v, want 401 within 1s", status, body, took)
	}
}

func TestProgressWriterControllers(t *testing.T) {
	errs := make(chan []error, 1)
	addr := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(*progressWriter); !ok {
			t.Errorf("the handler got %T, not the wrapper's writer", w)
		}
		rc := http.NewResponseController(w)
		errs <- []error{rc.Flush(), rc.SetReadDeadline(time.Time{}), rc.SetWriteDeadline(time.Time{})}
	}), DefaultConnLimits)
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := <-errs
	for i, name := range []string{"Flush", "SetReadDeadline", "SetWriteDeadline"} {
		if got[i] != nil {
			t.Errorf("%s: %v", name, got[i])
		}
	}
}
