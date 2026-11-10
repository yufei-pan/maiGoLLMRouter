package server

import (
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxBody is the most of a request body any handler reads: today's 32 MiB
// cap, now one constant for every reader. A larger declared Content-Length is
// refused with 413 before anything is read; a body that states no length and
// passes it is refused with the same 413 by readBody.
const maxBody = 32 << 20

// writePiece is the most of a response handed to the connection under one
// write deadline. 64 KiB at 1 Mbit/s takes 0.5 s, so only a client that has
// stopped reading misses a 60 s deadline.
const writePiece = 64 << 10

var bodyTooLargeMessage = fmt.Sprintf("request body is larger than %d MB", maxBody>>20)

// writeBodyTooLarge is the one answer for a body over maxBody, declared or not.
func writeBodyTooLarge(w http.ResponseWriter) {
	writeCodedError(w, http.StatusRequestEntityTooLarge, bodyTooLargeMessage, "invalid_request_error", "body_too_large")
}

// readBody reads a request body of at most maxBody bytes. One that goes on past
// that gets the 413 a declared Content-Length over maxBody gets, so a client that
// states no length hears the same answer, not a 400 about JSON cut off at the
// cap. ok is false once the reply is written.
func readBody(w http.ResponseWriter, r *http.Request) (raw []byte, ok bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if len(raw) > maxBody {
		writeBodyTooLarge(w)
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return nil, false
	}
	return raw, true
}

// ConnLimits bounds how long one client can hold a connection without making
// progress.
type ConnLimits struct {
	Body  time.Duration // reading a request body must finish within this
	Write time.Duration // each writePiece of a response must be taken within this
	Idle  time.Duration // http.Server.IdleTimeout
}

// DefaultConnLimits are the limits the router serves with.
var DefaultConnLimits = ConnLimits{
	Body:  5 * time.Minute,   // 32 MB at 1 Mbit/s takes about 4.5 min
	Write: 60 * time.Second,  // 64 KiB at 1 Mbit/s takes 0.5 s; only a client that stopped reading misses 60 s
	Idle:  120 * time.Second, // the UI polls every 3 s and annai sends turn after turn
}

// NewHTTPServer serves h behind the connection limits. It sets no ReadTimeout
// or WriteTimeout: those bound a whole request or response, and would cut off
// a long stream.
func NewHTTPServer(h http.Handler, lim ConnLimits) *http.Server {
	return &http.Server{
		Handler:           withConnLimits(h, lim),
		ReadHeaderTimeout: 10 * time.Second, // unchanged from the single listener main served before
		IdleTimeout:       lim.Idle,
	}
}

// withConnLimits bounds the body read, refuses a declared body over maxBody,
// makes every response write progress, and keeps an unread body from holding
// the connection. The order of its steps follows Go 1.27.3's net/http
// server.go, cited at each step.
func withConnLimits(next http.Handler, lim ConnLimits) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		var body *eofBody
		// ContentLength is -1 for a chunked body. A request without a body
		// gets no deadline: its background read is already running, and a
		// deadline would fail that read and cancel the connection's context.
		if r.ContentLength != 0 {
			// net/http clears this deadline itself when the body reaches EOF
			// (startBackgroundRead).
			_ = rc.SetReadDeadline(time.Now().Add(lim.Body))
			body = &eofBody{ReadCloser: r.Body}
			r.Body = body
		}
		pw := &progressWriter{ResponseWriter: w, rc: rc, stall: lim.Write, body: body}
		if r.ContentLength > maxBody {
			// Before auth, on every path: such a body can never succeed.
			writeBodyTooLarge(pw)
		} else {
			next.ServeHTTP(pw, r)
		}
		if body != nil && !body.eof {
			// An unread body (a 401, the mux's 404, a preflight, a truncated
			// read): finishRequest would drain up to 256 KB of it and wait
			// for bytes that may never come. Only when unread: after EOF the
			// background read is running, and failing it cancels the
			// connection's context (backgroundRead, handleReadErrorLocked),
			// so every later request on a keep-alive connection would start
			// cancelled.
			_ = rc.SetReadDeadline(time.Now())
		}
		// net/http flushes the rest of the response after we return
		// (finishRequest), and the handler may have worked on after its
		// last write.
		_ = rc.SetWriteDeadline(time.Now().Add(lim.Write))
	})
}

// progressWriter hands a response to the connection at most writePiece at a
// time, each under its own write deadline, so a client that stops reading is
// cut off while a slow one still gets everything. A missed deadline fails the
// write; net/http then records the error and cancels the request's context,
// as for a client that went away (checkConnErrorWriter).
//
// A response header committed while the request body is unread says
// Connection: close. That sets closeAfterReply before chunkWriter.writeHeader
// runs, so net/http does not drain the body itself (server.go: the drain
// needs !closeAfterReply). Such a drain goes around eofBody: one that
// reached EOF mid-handler would start the background read that the
// wrapper's unread-body deadline then fails. And the client learns that the
// connection won't be reused, instead of sending its next request into a
// closed one. A handler that writes nothing needs no commit here: its header
// is written in finishRequest, after the unread-body deadline, where
// net/http's own drain either reads a buffered rest to EOF and keeps the
// connection, or fails fast and says Connection: close itself.
type progressWriter struct {
	http.ResponseWriter
	rc        *http.ResponseController // over the connection's own writer
	stall     time.Duration
	body      *eofBody // nil for a request without a body
	committed bool     // the response header has been committed
}

// commit runs as the response header is committed.
func (pw *progressWriter) commit() {
	if pw.committed {
		return
	}
	pw.committed = true
	if pw.body != nil && !pw.body.eof {
		pw.Header().Set("Connection", "close")
	}
}

func (pw *progressWriter) WriteHeader(code int) {
	pw.commit()
	pw.ResponseWriter.WriteHeader(code)
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	pw.commit() // a first Write commits the header, as WriteHeader(200)
	if len(p) == 0 {
		return pw.ResponseWriter.Write(p)
	}
	n := 0
	for len(p) > 0 {
		piece := p[:min(len(p), writePiece)]
		_ = pw.rc.SetWriteDeadline(time.Now().Add(pw.stall))
		m, err := pw.ResponseWriter.Write(piece)
		n += m
		if err != nil {
			return n, err
		}
		p = p[len(piece):]
	}
	return n, nil
}

// FlushError flushes under a fresh deadline. ResponseController prefers it to
// Flush, so sseSink's flushes get one.
func (pw *progressWriter) FlushError() error {
	pw.commit() // a flush commits the header, as WriteHeader(200)
	_ = pw.rc.SetWriteDeadline(time.Now().Add(pw.stall))
	return pw.rc.Flush()
}

func (pw *progressWriter) Flush() { _ = pw.FlushError() }

// Unwrap lets ResponseController's other methods reach the connection.
func (pw *progressWriter) Unwrap() http.ResponseWriter { return pw.ResponseWriter }

// eofBody records whether a request body was read to EOF.
type eofBody struct {
	io.ReadCloser
	eof bool
}

func (b *eofBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.eof = true
	}
	return n, err
}
