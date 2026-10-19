package router

import (
	"context"
	"net/http"
	"time"

	"maiGoLLMRouter/config"
	"maiGoLLMRouter/provider"
)

// ExecuteSpeech routes a /v1/audio/speech request. Each target speaks with the
// voice its route names for it (else the client's), and OpenAI-kind targets
// are asked for the format the route names (else pcm, which the server turns
// into the client's format).
func (r *Router) ExecuteSpeech(ctx context.Context, inboundModel string, sp provider.SpeechRequest) *Result {
	st := r.state.Load()
	settings := st.cfg.Models[inboundModel].Speech
	return r.executeAudio(ctx, st, provider.OpSpeech, inboundModel, func(p *config.Provider, model string) provider.Request {
		s := sp
		set := settings[p.Name+"/"+model]
		if set.Voice != "" {
			s.Voice = set.Voice
		}
		s.Format = "pcm"
		if set.Format != "" {
			s.Format = set.Format
		}
		return provider.Request{Speech: &s}
	})
}

// ExecuteTranscription routes a /v1/audio/transcriptions request.
func (r *Router) ExecuteTranscription(ctx context.Context, inboundModel string, tr provider.TranscriptionRequest) *Result {
	st := r.state.Load()
	return r.executeAudio(ctx, st, provider.OpTranscribe, inboundModel, func(*config.Provider, string) provider.Request {
		t := tr
		return provider.Request{Transcription: &t}
	})
}

// executeAudio runs an audio request through the usual routing, each attempt
// limited by the shorter of audio_timeout and the provider's timeout.
func (r *Router) executeAudio(ctx context.Context, st *routerState, op provider.Operation, inboundModel string, build func(p *config.Provider, model string) provider.Request) *Result {
	try := func(ctx context.Context, st *routerState, res *Result, p *config.Provider, model string, op provider.Operation, _ map[string]any, key, keyType string, last **provider.Response) (bool, bool) {
		req := build(p, model)
		req.Op, req.Model, req.Provider = op, model, p.Name
		req.AudioAPI, req.FFmpeg = p.Audio, st.cfg.Server.FFmpeg

		limit := st.cfg.Server.AudioTimeout
		if p.Timeout > 0 && p.Timeout < limit {
			limit = p.Timeout
		}
		actx, cancel := context.WithTimeout(ctx, limit)
		defer cancel()

		att := Attempt{Provider: p.Name, Model: model, Key: maskKey(key), KeyType: keyType}
		start := time.Now()
		resp, err := provider.Call(actx, st.streamClients[p.Name], p.Kind, p.BaseURL, key, req)
		att.LatencyMS = time.Since(start).Milliseconds()
		if op == provider.OpTranscribe && ctx.Err() == nil && actx.Err() == context.DeadlineExceeded {
			// How long transcription takes depends on the clip, which the client
			// chooses, and the same key may serve chat: out of time is a
			// failover, not a sign of a bad key.
			if resp == nil {
				resp = &provider.Response{}
			}
			resp.NoBlackout = true
		}
		// judge gets the inbound context: running out of time is the
		// provider's fault, the caller going away is not.
		att, oc := r.judge(ctx, st, p, model, op, key, resp, err, att)
		done, skip := r.settle(ctx, res, p, model, key, keyType, resp, att, oc, last)
		if done && res.Success {
			res.ContentType = resp.ContentType
		}
		return done, skip
	}
	res, last, done := r.run(ctx, st, op, inboundModel, nil, try)
	if done {
		return res
	}
	switch {
	case onlyIncompatible(res.Attempts):
		res.Status = http.StatusBadRequest
		res.Body = errorBody("no configured target for this model can serve audio", "invalid_request_error")
	// A last reply that was a 2xx without audio or text (e.g. a text reply to
	// a speech request) is never passed on: it falls to the 502 below.
	case last != nil && len(last.OpenAIBody) > 0 && exhaustedHTTPStatus(last) >= 300:
		res.Status = exhaustedHTTPStatus(last)
		res.Body = last.OpenAIBody
	default:
		res.Status = http.StatusBadGateway
		res.Body = errorBody(exhaustedMessage, "upstream_error")
	}
	return res
}
