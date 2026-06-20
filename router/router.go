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
}

// New builds a Router from the resolved configuration.
func New(cfg *config.Config) *Router {
	r := &Router{
		cfg:      cfg,
		blackout: NewBlackout(cfg.Server.GlobalBlackout),
		clients:  make(map[string]*http.Client, len(cfg.Providers)),
	}
	for name, p := range cfg.Providers {
		r.clients[name] = &http.Client{Timeout: p.Timeout}
	}
	return r
}

// Reload replaces routing configuration and rebuilds provider HTTP clients.
// In-flight blackout state is preserved.
func (r *Router) Reload(cfg *config.Config) {
	clients := make(map[string]*http.Client, len(cfg.Providers))
	for name, p := range cfg.Providers {
		clients[name] = &http.Client{Timeout: p.Timeout}
	}
	r.mu.Lock()
	r.cfg = cfg
	r.clients = clients
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
	outcomeCanceled   // inbound request context canceled (caller went away)
	outcomeProhibited // downstream blocked the content (deterministic policy decision)
)

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

	// Everything exhausted.
	if last != nil && len(last.OpenAIBody) > 0 {
		res.Status = last.HTTPStatus
		if res.Status == 0 {
			res.Status = http.StatusBadGateway
		}
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
// A key is never retried in place. A bad output is treated exactly like a
// provider error (see callOnce): both black out the normal key and advance to
// the next key. Retrying the same key would burn the key's rate limit (RPM)
// without changing a deterministic-looking failure, so the router moves on.
func (r *Router) tryKey(ctx context.Context, res *Result, p *config.Provider, model string, op provider.Operation, body map[string]any, key, keyType string, last **provider.Response) (done, skip bool) {
	resp, att, oc := r.callOnce(ctx, p, model, op, body, key, keyType)
	res.Attempts = append(res.Attempts, att)
	notifyAttempt(ctx, att)
	if resp != nil {
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
	case outcomeProhibited:
		// Deterministic content-policy block: retrying with other keys for
		// this combo is futile and not a provider/key fault, so skip the
		// combo without blacking out the key.
		return false, true
	default: // outcomeProviderError and outcomeBadOutput
		// Both an HTTP/transport error and a bad output (e.g. an HTTP 200 that
		// carries a provider error in its body, or a response that did not
		// finish normally) black out the normal key and move on to the next.
		if keyType == "normal" {
			r.blackout.Fail(key, model)
		}
		return false, false
	}
}

func (r *Router) callOnce(ctx context.Context, p *config.Provider, model string, op provider.Operation, body map[string]any, key, keyType string) (*provider.Response, Attempt, outcome) {
	att := Attempt{Provider: p.Name, Model: model, Key: maskKey(key), KeyType: keyType}
	start := time.Now()
	resp, err := provider.Call(ctx, r.clients[p.Name], p.Kind, p.BaseURL, key, provider.Request{
		Op: op, Model: model, Body: body,
	})
	att.LatencyMS = time.Since(start).Milliseconds()
	if resp != nil {
		att.HTTPStatus = resp.HTTPStatus
		att.FinishReason = resp.FinishReason
		att.OutboundURL = resp.OutboundURL
	}

	// If the inbound request context is done, the caller disconnected or hit its
	// own deadline. Do not blacken keys or keep trying other keys/models: the
	// failure is on the caller side and the response can't be delivered anyway.
	if ctx.Err() != nil {
		att.Outcome = "canceled"
		att.Error = ctx.Err().Error()
		return resp, att, outcomeCanceled
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
	log.Printf("WARNING: output did not finish normally: provider=%s model=%s key=%s finish_reason=%s%s — blacking out key and moving on",
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
	return r.cfg.Server.GoodFinishReasons[strings.ToLower(resp.FinishReason)]
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
