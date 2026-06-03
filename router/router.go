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
	"time"

	"maiGoLLMRouter/config"
	"maiGoLLMRouter/provider"
)

// Router holds resolved configuration and per-provider HTTP clients.
type Router struct {
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
)

// Resolve maps an inbound model name to an ordered list of targets (#12,#14,#15).
func (r *Router) Resolve(model string) ([]config.Target, error) {
	if route, ok := r.cfg.Models[model]; ok {
		return route.Targets, nil
	}
	// provider/model form.
	if idx := strings.Index(model, "/"); idx > 0 && idx < len(model)-1 {
		prov, rest := model[:idx], model[idx+1:]
		if _, ok := r.cfg.Providers[prov]; ok {
			return []config.Target{{Provider: prov, Model: rest}}, nil
		}
		// Unknown provider prefix: hand the FULL name to the fallback provider
		// (e.g. "nvidia/nemotron-3" -> openrouter model "nvidia/nemotron-3").
		if r.cfg.FallbackProvider != "" {
			return []config.Target{{Provider: r.cfg.FallbackProvider, Model: model}}, nil
		}
		return nil, fmt.Errorf("unknown provider %q and no fallback_provider configured", prov)
	}
	// Bare, unmapped name routes to the fallback provider.
	if r.cfg.FallbackProvider != "" {
		return []config.Target{{Provider: r.cfg.FallbackProvider, Model: model}}, nil
	}
	return nil, fmt.Errorf("unrecognized model %q and no fallback_provider configured", model)
}

// Execute runs the full routing algorithm for one request and returns the
// result along with a per-attempt log.
func (r *Router) Execute(ctx context.Context, op provider.Operation, inboundModel string, body map[string]any) *Result {
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

	// Phase 1: normal keys, randomized order, with blackout on failure.
	for _, t := range targets {
		p := r.cfg.Providers[t.Provider]
		if p == nil {
			res.Attempts = append(res.Attempts, Attempt{
				Provider: t.Provider, Model: t.Model, Outcome: "error",
				Error: "provider not defined",
			})
			continue
		}
		for _, key := range shuffledKeys(p.Keys) {
			if r.blackout.Blocked(key) {
				res.Attempts = append(res.Attempts, Attempt{
					Provider: p.Name, Model: t.Model, Key: maskKey(key),
					KeyType: "normal", Outcome: "skipped_blackout",
				})
				continue
			}
			if r.tryKey(ctx, res, p, t.Model, op, body, key, "normal", &last) {
				return res
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
		for _, key := range p.FallbackKeySet() {
			if r.tryKey(ctx, res, p, t.Model, op, body, key, "fallback", &last) {
				return res
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

// tryKey performs up to 1+maxRetries calls on a single key, retrying only on
// bad output. It returns true when the request succeeds (res is populated).
func (r *Router) tryKey(ctx context.Context, res *Result, p *config.Provider, model string, op provider.Operation, body map[string]any, key, keyType string, last **provider.Response) bool {
	retries := r.cfg.Server.MaxRetries
	for i := 0; ; i++ {
		resp, att, oc := r.callOnce(ctx, p, model, op, body, key, keyType)
		res.Attempts = append(res.Attempts, att)
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
			return true
		case outcomeProviderError:
			if keyType == "normal" {
				r.blackout.Fail(key)
			}
			return false
		case outcomeBadOutput:
			if i >= retries {
				return false
			}
			// retry the same key without blackout
		}
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

	if err != nil || resp == nil || !resp.OK() {
		att.Outcome = "provider_error"
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
	reason := resp.FinishReason
	if reason == "" {
		reason = "<none>"
	}
	detail := ""
	if strings.EqualFold(resp.FinishReason, "length") {
		detail = " (response truncated by max output tokens)"
	}
	log.Printf("WARNING: output did not finish normally: provider=%s model=%s key=%s finish_reason=%s%s — will retry/fallback",
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

func maskKey(key string) string {
	if len(key) <= 4 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}

func errorBody(msg, typ string) []byte {
	return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":%q}}`, msg, typ))
}
