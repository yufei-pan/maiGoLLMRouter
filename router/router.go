// Package router resolves inbound model names to downstream targets and
// executes requests across normal and fallback keys with blackout, output
// verification, and retries.
package router

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"maiGoLLMRouter/config"
	"maiGoLLMRouter/provider"
)

// Router holds resolved configuration and per-provider HTTP clients.
type Router struct {
	mu       sync.RWMutex
	cfg      *config.Config
	blackout *Blackout
	clients  map[string]*http.Client
	respCaps *responsesCapability
}

// New builds a Router from the resolved configuration.
func New(cfg *config.Config) *Router {
	r := &Router{
		cfg:      cfg,
		blackout: NewBlackout(cfg.Server.GlobalBlackout),
		clients:  make(map[string]*http.Client, len(cfg.Providers)),
		respCaps: newResponsesCapability(),
	}
	for name, p := range cfg.Providers {
		r.clients[name] = &http.Client{Timeout: p.Timeout}
	}
	return r
}

// Reload replaces routing configuration and rebuilds provider HTTP clients.
// In-flight blackout state is preserved; learned Responses capabilities are not.
func (r *Router) Reload(cfg *config.Config) {
	clients := make(map[string]*http.Client, len(cfg.Providers))
	for name, p := range cfg.Providers {
		clients[name] = &http.Client{Timeout: p.Timeout}
	}
	r.mu.Lock()
	r.cfg = cfg
	r.clients = clients
	// Base URLs and supports_responses may have changed, so learned Responses
	// verdicts no longer describe the current downstreams. Clear under the
	// write lock so Execute (RLock) never observes the new config with a
	// stale cache, and MarkChatOnly cannot race Clear after Unlock.
	r.respCaps.Clear()
	r.mu.Unlock()
	r.blackout.SetDuration(cfg.Server.GlobalBlackout)
}

// Attempt records a single downstream call for logging.
type Attempt struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Key          string `json:"key"` // masked
	KeyType      string `json:"key_type"`
	HTTPStatus   int    `json:"http_status"`
	FinishReason string `json:"finish_reason,omitempty"`
	Outcome      string `json:"outcome"`
	Error        string `json:"error,omitempty"`
	Response     string `json:"response,omitempty"` // raw downstream body for failed attempts
	OutboundURL  string `json:"outbound_url,omitempty"`
	LatencyMS    int64  `json:"latency_ms"`
}

// Result is the overall outcome of routing one inbound request.
type Result struct {
	Success  bool      `json:"success"`
	Status   int       `json:"status"`
	Body     []byte    `json:"-"`
	Provider string    `json:"provider,omitempty"`
	Model    string    `json:"model,omitempty"`
	Targets  []string  `json:"targets"`
	Attempts []Attempt `json:"attempts"`
}

type outcome int

const (
	outcomeSuccess outcome = iota
	outcomeBadOutput
	outcomeProviderError
	outcomeCanceled     // inbound request context canceled (caller went away)
	outcomeProhibited   // downstream blocked the content (deterministic policy decision)
	outcomeIncompatible // Responses body cannot be served over this provider's dialect
)

// attemptIncompatible is the Attempt.Outcome recorded for outcomeIncompatible.
const attemptIncompatible = "incompatible"

// Resolve maps an inbound model name to an ordered list of targets (#12,#14,#15).
func (r *Router) Resolve(model string) ([]config.Target, error) {
	if route, ok := r.cfg.Models[model]; ok {
		return orderedTargets(route.Targets, route.Selection), nil
	}
	// provider/model form.
	if idx := strings.Index(model, "/"); idx > 0 && idx < len(model)-1 {
		prov, rest := model[:idx], model[idx+1:]
		if _, ok := r.cfg.Providers[prov]; ok {
			return []config.Target{{Provider: prov, Model: rest}}, nil
		}
		// Unknown provider prefix: hand the FULL name to the fallback providers
		// (e.g. "nvidia/nemotron-3" -> openrouter model "nvidia/nemotron-3").
		if fb := r.fallbackTargets(model); len(fb) > 0 {
			return fb, nil
		}
		return nil, fmt.Errorf("unknown provider %q and no fallback_providers configured", prov)
	}
	// Bare, unmapped name routes to the fallback providers.
	if fb := r.fallbackTargets(model); len(fb) > 0 {
		return fb, nil
	}
	return nil, fmt.Errorf("unrecognized model %q and no fallback_providers configured", model)
}

// fallbackTargets builds the ordered list of fallback targets for an inbound
// model name, one target per configured fallback provider. The order follows
// the configured fallback selection method: sequential (config order) by
// default, or shuffled per request when set to shuffle/random.
func (r *Router) fallbackTargets(model string) []config.Target {
	provs := orderedProviders(r.cfg.FallbackProviders, r.cfg.FallbackSelection)
	if len(provs) == 0 {
		return nil
	}
	out := make([]config.Target, 0, len(provs))
	for _, prov := range provs {
		out = append(out, config.Target{Provider: prov, Model: model})
	}
	return out
}

// Execute runs the full routing algorithm for one request and returns the
// result along with a per-attempt log.
func (r *Router) Execute(ctx context.Context, op provider.Operation, inboundModel string, body map[string]any) *Result {
	r.mu.RLock()
	defer r.mu.RUnlock()

	res := &Result{}
	targets, err := r.Resolve(inboundModel)
	if err != nil {
		res.Status = http.StatusBadRequest
		res.Body = errorBody(err.Error(), "invalid_request_error")
		return res
	}
	for _, t := range targets {
		res.Targets = append(res.Targets, t.Provider+"/"+t.Model)
	}

	var last *provider.Response

	// prohibited tracks provider/model combos that returned a content-policy
	// block. Such a result is deterministic for the combo regardless of key, so
	// the remaining keys (and the fallback-key phase) for that combo are skipped.
	prohibited := make(map[string]bool)

	// Phase 1: normal keys, randomized order, with blackout on failure.
	for _, t := range targets {
		p := r.cfg.Providers[t.Provider]
		if p == nil {
			att := Attempt{
				Provider: t.Provider, Model: t.Model, Outcome: "error",
				Error: "provider not defined",
			}
			res.Attempts = append(res.Attempts, att)
			notifyAttempt(ctx, att)
			continue
		}
		for _, key := range shuffledKeys(p.Keys) {
			if r.blackout.Blocked(key, t.Model) {
				att := Attempt{
					Provider: p.Name, Model: t.Model, Key: maskKey(key),
					KeyType: "normal", Outcome: "skipped_blackout",
				}
				res.Attempts = append(res.Attempts, att)
				notifyAttempt(ctx, att)
				continue
			}
			done, skip := r.tryKey(ctx, res, p, t.Model, op, body, key, "normal", &last)
			if done {
				return res
			}
			if skip {
				prohibited[targetKey(t)] = true
				break
			}
		}
	}

	// Phase 2: fallback keys, in listed order, never blacked out, gated by
	// fallback_models (#6,#8).
	for _, t := range targets {
		p := r.cfg.Providers[t.Provider]
		if p == nil || !p.FallbackAllows(t.Model) {
			continue
		}
		if prohibited[targetKey(t)] {
			continue
		}
		for _, key := range p.FallbackKeySet() {
			done, skip := r.tryKey(ctx, res, p, t.Model, op, body, key, "fallback", &last)
			if done {
				return res
			}
			if skip {
				prohibited[targetKey(t)] = true
				break
			}
		}
	}

	// Everything exhausted. When no target could even accept the request, the
	// fault is in the request, not in the providers.
	if onlyIncompatible(res.Attempts) {
		res.Status = http.StatusBadRequest
		res.Body = errorBody("this request uses Responses-only features that no configured target for this model can serve; route it to a provider with a /responses route", "invalid_request_error")
		return res
	}
	if last != nil && len(last.OpenAIBody) > 0 {
		res.Status = exhaustedHTTPStatus(last)
		res.Body = last.OpenAIBody
	} else {
		res.Status = http.StatusBadGateway
		res.Body = errorBody("all providers, keys, and models were exhausted", "upstream_error")
	}
	return res
}

// tryKey performs a single call on one key. The done return is true when the
// request finishes the whole routing run (res is populated): success or caller
// cancellation. The skip return is true when the provider/model combo returned
// a content-policy block, signaling the caller to skip the combo's remaining
// keys and advance to the next target/fallback without blacking out the key.
//
// A key is never retried in place. A bad output advances to the next key
// without blackout (empty/unfinished HTTP 200 bodies are not a key-health
// signal). A provider error blacks out the normal key and advances. Retrying
// the same key would burn the key's rate limit (RPM) without changing a
// deterministic-looking failure, so the router moves on.
func (r *Router) tryKey(ctx context.Context, res *Result, p *config.Provider, model string, op provider.Operation, body map[string]any, key, keyType string, last **provider.Response) (done, skip bool) {
	resp, att, oc := r.callOnce(ctx, p, model, op, body, key, keyType)
	res.Attempts = append(res.Attempts, att)
	notifyAttempt(ctx, att)
	// Incompatible responses have HTTPStatus 0 and an empty body — they must
	// not overwrite a real provider_error used for exhaustion forwarding.
	if resp != nil && !resp.Incompatible {
		*last = resp
	}
	switch oc {
	case outcomeSuccess:
		res.Success = true
		res.Status = resp.HTTPStatus
		res.Body = resp.OpenAIBody
		res.Provider = p.Name
		res.Model = model
		return true, false
	case outcomeCanceled:
		// Caller went away: stop the whole routing run without blackout.
		res.Status = 499 // "Client Closed Request" (nginx convention)
		res.Body = errorBody("request canceled by caller", "request_canceled")
		return true, false
	case outcomeIncompatible:
		// The provider cannot express this Responses body at all. Nothing was
		// sent, so there is no key or provider fault to record: move on to the
		// next key/target, which may be Responses-capable.
		return false, false
	case outcomeProhibited:
		// Deterministic content-policy block: retrying with other keys for
		// this combo is futile and not a provider/key fault, so skip the
		// combo without blacking out the key.
		return false, true
	case outcomeBadOutput:
		// HTTP 200 with empty/unfinished output: try the next key, but do not
		// black out — this is not a reliable key/model health signal.
		return false, false
	default: // outcomeProviderError
		// Transport/HTTP errors black out the normal key and move on.
		// HTTP 400 is a client/request error, not a key/model/provider fault,
		// so it must not black out anything.
		if keyType == "normal" && !isHTTP400(resp) {
			r.blackout.Fail(key, model)
		}
		return false, false
	}
}

func (r *Router) callOnce(ctx context.Context, p *config.Provider, model string, op provider.Operation, body map[string]any, key, keyType string) (*provider.Response, Attempt, outcome) {
	att := Attempt{Provider: p.Name, Model: model, Key: maskKey(key), KeyType: keyType}
	start := time.Now()
	resp, err := provider.Call(ctx, r.clients[p.Name], p.Kind, p.BaseURL, key, provider.Request{
		Op: op, Model: model, Body: body, ResponsesMode: r.responsesMode(p, op),
	})
	att.LatencyMS = time.Since(start).Milliseconds()
	if resp != nil {
		att.HTTPStatus = resp.HTTPStatus
		att.FinishReason = resp.FinishReason
		att.OutboundURL = resp.OutboundURL
	}
	if resp != nil && resp.LearnChatOnly {
		r.respCaps.MarkChatOnly(p.Name)
	}

	// If the inbound request context is done, the caller disconnected or hit its
	// own deadline. Do not blacken keys or keep trying other keys/models: the
	// failure is on the caller side and the response can't be delivered anyway.
	if ctx.Err() != nil {
		att.Outcome = "canceled"
		att.Error = ctx.Err().Error()
		return resp, att, outcomeCanceled
	}

	// A non-portable Responses body is a property of the request and the
	// provider's dialect, so no HTTP call was made and none of the error/output
	// checks below apply.
	if resp != nil && resp.Incompatible {
		att.Outcome = attemptIncompatible
		att.Error = "responses request not portable to this provider"
		return resp, att, outcomeIncompatible
	}

	// A content-policy block is a deterministic output, even when delivered with
	// an HTTP 200. Treat it as such before the error/output checks so it is not
	// mistaken for a provider error (no blackout) or retriable bad output.
	if resp != nil && resp.Prohibited {
		att.Outcome = "prohibited_content"
		att.Error = "content blocked by provider policy (" + provider.ProhibitedContentMarker + ")"
		att.Response = string(resp.RawResponse)
		return resp, att, outcomeProhibited
	}

	if err != nil || resp == nil || !resp.OK() {
		att.Outcome = "provider_error"
		if resp != nil {
			att.Response = string(resp.RawResponse)
		}
		if err != nil {
			att.Error = err.Error()
		} else if resp != nil {
			att.Error = fmt.Sprintf("http status %d", resp.HTTPStatus)
		}
		return resp, att, outcomeProviderError
	}

	// HTTP succeeded: verify output (#5).
	if r.outputOK(op, resp) {
		att.Outcome = "success"
		return resp, att, outcomeSuccess
	}
	att.Outcome = "bad_output"
	att.Error = "output did not finish normally"
	att.Response = string(resp.RawResponse)
	reason := resp.FinishReason
	if reason == "" {
		reason = "<none>"
	}
	detail := ""
	if strings.EqualFold(resp.FinishReason, "length") {
		detail = " (response truncated by max output tokens)"
	}
	log.Printf("WARNING: output did not finish normally: provider=%s model=%s key=%s finish_reason=%s%s — moving on without blackout",
		p.Name, model, maskKey(key), reason, detail)
	return resp, att, outcomeBadOutput
}

// outputOK applies output verification: chat responses must have content and a
// finish reason in the configured good set; embeddings must carry data (#5).
func (r *Router) outputOK(op provider.Operation, resp *provider.Response) bool {
	if op == provider.OpEmbed {
		return resp.HasContent
	}
	if !resp.HasContent {
		return false
	}
	if op == provider.OpResponses && resp.FinishReason == "" {
		// A Responses reply that could not be normalized (unparseable body, or a
		// chat reply that failed to wrap) carries no finish reason to verify.
		return false
	}
	return r.cfg.Server.GoodFinishReasons[strings.ToLower(resp.FinishReason)]
}

// responsesMode decides how an OpResponses request should reach the provider:
// explicit config wins, then the probe cache, otherwise probe and learn.
func (r *Router) responsesMode(p *config.Provider, op provider.Operation) provider.ResponsesMode {
	if op != provider.OpResponses {
		return provider.ResponsesModeProbe
	}
	if p.SupportsResponses != nil {
		if *p.SupportsResponses {
			return provider.ResponsesModeForce
		}
		return provider.ResponsesModeChatOnly
	}
	if r.respCaps.IsChatOnly(p.Name) {
		return provider.ResponsesModeChatOnly
	}
	return provider.ResponsesModeProbe
}

// onlyIncompatible reports whether every attempt that reached a provider was
// rejected as non-portable. Blackout skips never reached a provider, so they
// do not count either way; at least one real incompatible attempt is required.
func onlyIncompatible(attempts []Attempt) bool {
	found := false
	for _, a := range attempts {
		switch a.Outcome {
		case "skipped_blackout":
		case attemptIncompatible:
			found = true
		default:
			return false
		}
	}
	return found
}

func shuffledKeys(keys []string) []string {
	out := make([]string, len(keys))
	copy(out, keys)
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// orderedTargets returns a copy of targets, optionally shuffled per request.
func orderedTargets(targets []config.Target, selection string) []config.Target {
	out := make([]config.Target, len(targets))
	copy(out, targets)
	if selection == config.TargetSelectionShuffle && len(out) > 1 {
		rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	}
	return out
}

// orderedProviders returns a copy of names, optionally shuffled per request
// when selection is shuffle/random.
func orderedProviders(names []string, selection string) []string {
	out := make([]string, len(names))
	copy(out, names)
	if selection == config.TargetSelectionShuffle && len(out) > 1 {
		rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	}
	return out
}

// targetKey identifies a provider/model combo for the prohibited-combo set. The
// NUL separator cannot appear in provider names or models, so it is unambiguous.
func targetKey(t config.Target) string {
	return t.Provider + "\x00" + t.Model
}

func maskKey(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}

func errorBody(msg, typ string) []byte {
	return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":%q}}`, msg, typ))
}

// exhaustedHTTPStatus picks the HTTP status returned to the inbound caller when
// all routing attempts failed. A downstream HTTP 200 that carried a
// content-policy block must not be forwarded as 200, or clients may treat a
// truncated/filtered body as a normal completion.
func exhaustedHTTPStatus(last *provider.Response) int {
	if last.Prohibited {
		return http.StatusForbidden
	}
	if last.HTTPStatus >= 200 && last.HTTPStatus < 300 {
		return last.HTTPStatus
	}
	if last.HTTPStatus > 0 {
		return last.HTTPStatus
	}
	return http.StatusBadGateway
}

// isHTTP400 reports a downstream Bad Request. Those are request-shaped faults,
// not key/model/provider health issues, so they must not trigger blackout.
func isHTTP400(resp *provider.Response) bool {
	return resp != nil && resp.HTTPStatus == http.StatusBadRequest
}
