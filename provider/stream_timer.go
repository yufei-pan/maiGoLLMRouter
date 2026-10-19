package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// errStreamTimeout is the cause an attempt is cancelled with when its provider
// sent no output in time, or went silent after output had started.
var errStreamTimeout = errors.New("no output from the provider in time")

// timeoutErrorBody is the client's body when a first-output timeout ends the run.
var timeoutErrorBody = []byte(`{"error":{"message":"no output from the provider in time","type":"upstream_error"}}`)

// streamTimer cancels an attempt when no output has arrived within limit or,
// once output has started, when the body has been silent for limit. Until
// output starts, reads (role chunks, keep-alive comments) do not extend it.
type streamTimer struct {
	mu      sync.Mutex
	limit   time.Duration
	t       *time.Timer
	started bool
}

// newStreamTimer starts the first-output limit. A limit <= 0 never fires.
func newStreamTimer(limit time.Duration, cancel context.CancelCauseFunc) *streamTimer {
	st := &streamTimer{limit: limit}
	if limit > 0 {
		st.t = time.AfterFunc(limit, func() { cancel(errStreamTimeout) })
	}
	return st
}

// outputStarted switches to the idle limit: from now on each read resets it.
func (t *streamTimer) outputStarted() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.started = true
	if t.t != nil {
		t.t.Reset(t.limit)
	}
}

// read records that a body read returned bytes.
func (t *streamTimer) read() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started && t.t != nil {
		t.t.Reset(t.limit)
	}
}

func (t *streamTimer) stop() {
	if t.t != nil {
		t.t.Stop()
	}
}

// streamConn is one open streamed request: the response, its body read through
// the timer and the raw log, and the attempt context the timer cancels.
type streamConn struct {
	parent context.Context
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *streamTimer
	resp   *http.Response
	raw    rawLog
	once   sync.Once
}

// openStream sends a streaming request. On a transport failure it returns the
// error (a *StreamError when the first-output limit ran out first).
func openStream(ctx context.Context, client *http.Client, url string, headers map[string]string, body []byte, timeout time.Duration) (*streamConn, error) {
	actx, cancel := context.WithCancelCause(ctx)
	c := &streamConn{parent: ctx, ctx: actx, cancel: cancel}
	c.timer = newStreamTimer(timeout, cancel)
	req, err := http.NewRequestWithContext(actx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		c.close()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		fail := c.failure(err)
		c.close()
		return nil, fail
	}
	c.resp = resp
	return c, nil
}

// isEventStream reports a 2xx reply that is a server-sent event stream.
func (c *streamConn) isEventStream() bool {
	ok := c.resp.StatusCode >= 200 && c.resp.StatusCode < 300
	ct := strings.ToLower(c.resp.Header.Get("Content-Type"))
	return ok && strings.HasPrefix(ct, "text/event-stream")
}

// Read reads the body, feeding the timer and the raw log.
func (c *streamConn) Read(p []byte) (int, error) {
	n, err := c.resp.Body.Read(p)
	if n > 0 {
		c.timer.read()
		c.raw.write(p[:n])
	}
	return n, err
}

// readAll reads a reply that is not a stream, for judging like a buffered one.
func (c *streamConn) readAll() ([]byte, error) {
	defer c.close()
	raw, err := io.ReadAll(c.resp.Body)
	if err != nil {
		return raw, c.failure(err)
	}
	return raw, nil
}

// failure turns a transport error into what the stream reports: the inbound
// context's own error when the caller went away, a 504 *StreamError when the
// timer ran out, else a 502 *StreamError for a dropped connection. The body,
// which may reach the client, never carries the transport error: it names the
// provider's address. Err keeps it for the log.
func (c *streamConn) failure(err error) error {
	if c.parent.Err() != nil {
		return c.parent.Err()
	}
	if context.Cause(c.ctx) == errStreamTimeout {
		return &StreamError{Status: 504, Body: timeoutErrorBody, Timeout: true, Err: errStreamTimeout}
	}
	return &StreamError{
		Status: 502,
		Body:   upstreamErrorBody("the connection to the provider dropped"),
		Err:    err,
	}
}

func (c *streamConn) close() {
	c.once.Do(func() {
		c.timer.stop()
		c.cancel(context.Canceled)
		if c.resp != nil {
			c.resp.Body.Close()
		}
	})
}

// upstreamErrorBody is a JSON error body for a failure on the provider's side.
func upstreamErrorBody(message string) []byte {
	return mustJSON(map[string]any{"error": map[string]any{"message": message, "type": "upstream_error"}})
}
