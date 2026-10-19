package router

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"maiGoLLMRouter/config"
	"maiGoLLMRouter/provider"
)

// StreamSink receives a streamed reply. The router calls Commit once, when
// the first output is ready, then Write for each frame in order.
type StreamSink interface {
	Commit() error                // send the 200 and the event-stream headers
	Write(f provider.Frame) error // write and flush one event
}

// emptyStreamBody answers a streamed request whose last attempt ended without
// any output.
var emptyStreamBody = errorBody("the provider's stream ended without output", "upstream_error")

// ExecuteStream routes a request that asked for a stream. Until the first
// output arrives it routes exactly as Execute does: keys, blackout, fallback.
// Once it has committed through sink it never switches attempts; a later
// failure goes to the client as an error event. When the result is not
// Committed, Status and Body are the JSON answer, never a 2xx.
func (r *Router) ExecuteStream(ctx context.Context, op provider.Operation, inboundModel string, body map[string]any, sink StreamSink) *Result {
	st := r.state.Load()
	try := func(ctx context.Context, st *routerState, res *Result, p *config.Provider, model string, op provider.Operation, body map[string]any, key, keyType string, last **provider.Response) (bool, bool) {
		return r.tryKeyStream(ctx, st, res, p, model, op, body, key, keyType, last, sink)
	}
	res, last, done := r.run(ctx, st, op, inboundModel, body, try)
	if done {
		return res
	}

	switch {
	case onlyIncompatible(res.Attempts):
		res.Status = http.StatusBadRequest
		res.Body = errorBody(incompatibleMessage, "invalid_request_error")
	case onlyProhibited(res.Attempts):
		// Every target refused: the client gets a refusal, not a failure.
		r.sendRefusal(ctx, res, op, inboundModel, sink)
	case last != nil && len(last.OpenAIBody) > 0 && exhaustedHTTPStatus(last) >= 300:
		res.Status = exhaustedHTTPStatus(last)
		res.Body = last.OpenAIBody
	default:
		res.Status = http.StatusBadGateway
		res.Body = errorBody(exhaustedMessage, "upstream_error")
	}
	return res
}

func (r *Router) sendRefusal(ctx context.Context, res *Result, op provider.Operation, model string, sink StreamSink) {
	s := provider.RefusalStream(op, model)
	res.Committed, res.Status, res.Body = true, http.StatusOK, s.State().Body
	if sink.Commit() != nil {
		return
	}
	for {
		f, err := s.Next()
		if err != nil || sink.Write(f) != nil {
			return
		}
	}
}

// tryKeyStream makes one streamed call on one key. Before output it behaves
// like tryKey; once output arrives it commits, pumps the rest, and finishes the
// run (done) whatever happens next.
func (r *Router) tryKeyStream(ctx context.Context, st *routerState, res *Result, p *config.Provider, model string, op provider.Operation, body map[string]any, key, keyType string, last **provider.Response, sink StreamSink) (done, skip bool) {
	att := Attempt{Provider: p.Name, Model: model, Key: maskKey(key), KeyType: keyType}
	start := time.Now()
	req := provider.Request{
		Op: op, Model: model, Body: body, ResponsesMode: st.responsesMode(p, op),
		Provider: p.Name, Signatures: r.signatures,
	}
	var stream *provider.Stream
	var resp *provider.Response
	var err error
	if st.cfg.Server.Streaming {
		stream, resp, err = provider.CallStream(ctx, st.streamClients[p.Name], p.Kind, p.BaseURL, key, p.Timeout, req)
	} else {
		resp, err = provider.Call(ctx, st.clients[p.Name], p.Kind, p.BaseURL, key, req)
	}

	if stream == nil {
		// Not a stream: judge it like a buffered reply, and send a good one as a stream.
		att.LatencyMS = time.Since(start).Milliseconds()
		var se *provider.StreamError
		if errors.As(err, &se) && resp != nil {
			resp.HTTPStatus, resp.OpenAIBody = se.Status, se.Body
		}
		var oc outcome
		att, oc = r.judge(ctx, st, p, model, op, key, resp, err, att)
		if oc != outcomeSuccess {
			return r.settle(ctx, res, p, model, key, keyType, resp, att, oc, last)
		}
		stream = provider.BufferedStream(op, resp, provider.WantsUsage(body))
		return r.pump(ctx, st, res, p, model, op, key, keyType, stream, nil, att, start, sink), false
	}

	if stream.LearnChatOnly {
		st.respCaps.MarkChatOnly(p.Name)
	}
	att.HTTPStatus = stream.HTTPStatus
	att.OutboundURL = stream.OutboundURL

	// Before output: keep what arrives, and fail over on any failure.
	var kept []provider.Frame
	for {
		f, nerr := stream.Next()
		if nerr == nil {
			kept = append(kept, f)
			if stream.State().Output {
				return r.pump(ctx, st, res, p, model, op, key, keyType, stream, kept, att, start, sink), false
			}
			continue
		}
		s := stream.State()
		if nerr == io.EOF && s.HasContent && !s.Prohibited {
			// A complete reply whose output never showed as a delta (a final event only).
			return r.pump(ctx, st, res, p, model, op, key, keyType, stream, kept, att, start, sink), false
		}
		stream.Close()
		failed, att, oc := r.judgeEarlyEnd(ctx, stream, s, nerr, att, start)
		return r.settle(ctx, res, p, model, key, keyType, failed, att, oc, last)
	}
}

// judgeEarlyEnd classifies a stream that ended or failed before any output,
// and builds the reply that stands for it if the run is exhausted.
func (r *Router) judgeEarlyEnd(ctx context.Context, stream *provider.Stream, s provider.StreamState, err error, att Attempt, start time.Time) (*provider.Response, Attempt, outcome) {
	att.LatencyMS = time.Since(start).Milliseconds()
	att.FinishReason = s.FinishReason
	att.Response = string(s.Raw)
	failed := &provider.Response{
		HTTPStatus: stream.HTTPStatus, OutboundURL: stream.OutboundURL, OutboundBody: stream.OutboundBody,
		RawResponse: s.Raw, Prohibited: s.Prohibited,
	}
	if ctx.Err() != nil {
		att.Outcome, att.Error = "canceled", ctx.Err().Error()
		return failed, att, outcomeCanceled
	}
	var se *provider.StreamError
	switch {
	case errors.As(err, &se):
		failed.HTTPStatus, failed.OpenAIBody = se.Status, se.Body
		failed.Prohibited = failed.Prohibited || se.Prohibited
		att.HTTPStatus = se.Status
		if failed.Prohibited {
			att.Outcome = "prohibited_content"
			att.Error = "content blocked by provider policy (" + provider.ProhibitedContentMarker + ")"
			return failed, att, outcomeProhibited
		}
		att.Outcome, att.Error = "provider_error", se.Error()
		return failed, att, outcomeProviderError
	case err == io.EOF && s.Prohibited:
		att.Outcome = "prohibited_content"
		att.Error = "content blocked by provider policy (" + provider.ProhibitedContentMarker + ")"
		return failed, att, outcomeProhibited
	case err == io.EOF:
		failed.HTTPStatus, failed.OpenAIBody = http.StatusBadGateway, emptyStreamBody
		att.Outcome, att.Error = "bad_output", "the provider's stream ended without output"
		return failed, att, outcomeBadOutput
	default:
		failed.HTTPStatus, failed.OpenAIBody = http.StatusBadGateway, errorBody(err.Error(), "upstream_error")
		att.Outcome, att.Error = "provider_error", err.Error()
		return failed, att, outcomeProviderError
	}
}

// pump commits to this attempt, sends the kept frames and everything that
// follows, and records how the stream ended. It always finishes the run.
func (r *Router) pump(ctx context.Context, st *routerState, res *Result, p *config.Provider, model string, op provider.Operation, key, keyType string, stream *provider.Stream, kept []provider.Frame, att Attempt, start time.Time, sink StreamSink) bool {
	res.Committed, res.Status, res.Provider, res.Model = true, http.StatusOK, p.Name, model
	clientGone := sink.Commit() != nil
	if !clientGone {
		notifyCommit(ctx, p.Name, model)
		for _, f := range kept {
			if sink.Write(f) != nil {
				clientGone = true
				break
			}
		}
	}
	var end error
	for !clientGone {
		f, err := stream.Next()
		if err != nil {
			end = err
			break
		}
		if sink.Write(f) != nil {
			clientGone = true
		}
	}
	stream.Close()

	s := stream.State()
	res.Body = s.Body
	att.LatencyMS = time.Since(start).Milliseconds()
	att.FinishReason = s.FinishReason
	var se *provider.StreamError
	switch {
	case clientGone || ctx.Err() != nil:
		att.Outcome, att.Error = "canceled", "the client went away"
		if ctx.Err() != nil {
			att.Error = ctx.Err().Error()
		}
	case end == io.EOF && st.outputOK(op, &provider.Response{FinishReason: s.FinishReason, HasContent: s.HasContent}):
		att.Outcome = "success"
		r.blackout.Success(key, model)
		res.Success = true
	case end == io.EOF && s.Prohibited:
		att.Outcome = "prohibited_content"
		att.Response = string(s.Raw)
	case end == io.EOF:
		att.Outcome, att.Error = "bad_output", "output did not finish normally"
		att.Response = string(s.Raw)
	default:
		// The stream failed after the client had output: tell the client in
		// the stream itself, since the status line is long gone.
		status, msg := http.StatusBadGateway, end.Error()
		frame := (*provider.Frame)(nil)
		if errors.As(end, &se) {
			status, msg, frame = se.Status, se.Error(), se.Frame
			if se.Err != nil {
				msg = se.Err.Error()
			}
		}
		if frame == nil {
			f := stream.ErrorFrame(status, msg)
			frame = &f
		}
		_ = sink.Write(*frame)
		att.HTTPStatus, att.Error, att.Response = status, msg, string(s.Raw)
		if (se != nil && se.Prohibited) || s.Prohibited {
			att.Outcome = "prohibited_content"
		} else {
			att.Outcome = "provider_error"
			if keyType == "normal" && status != http.StatusBadRequest {
				r.blackout.Fail(key, model)
			}
		}
	}
	res.Attempts = append(res.Attempts, att)
	notifyAttempt(ctx, att)
	return true
}

// onlyProhibited reports whether every attempt that reached a provider was a
// content-policy block. Blackout skips and incompatible bodies reached none;
// at least one real block is required.
func onlyProhibited(attempts []Attempt) bool {
	found := false
	for _, a := range attempts {
		switch a.Outcome {
		case "skipped_blackout", attemptIncompatible:
		case "prohibited_content":
			found = true
		default:
			return false
		}
	}
	return found
}
