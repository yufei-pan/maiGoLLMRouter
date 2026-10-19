// Package config loads and normalizes the TOML configuration for the router.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Default values applied when the corresponding config field is omitted.
const (
	DefaultListen         = ":8470"
	DefaultLogDir         = "./logs"
	DefaultGlobalBlackout = 60 * time.Second
	DefaultMaxRetries     = 2
	DefaultTimeout        = 60 * time.Second
	DefaultFFmpeg         = "ffmpeg"
	DefaultAudioTimeout   = 10 * time.Second
	// DefaultLogRetentionDays is how long detail log files are kept before
	// cleanup. Roughly two months. Set log_retention_days = 0 to disable.
	DefaultLogRetentionDays = 60
	// DefaultConfigReloadInterval is how often the config file is polled for
	// changes. Set config_reload_interval = "0" to disable polling; SIGHUP and
	// systemctl reload still apply changes.
	DefaultConfigReloadInterval = 3 * time.Second
)

// Target selection methods for model alias routing.
const (
	TargetSelectionSequential = "sequential"
	TargetSelectionShuffle    = "shuffle"
)

// DefaultGoodFinishReasons are the normalized finish reasons treated as a
// successful completion when none are configured.
var DefaultGoodFinishReasons = []string{"stop", "length", "tool_calls", "function_call", "end_turn"}

// Config is the fully resolved, validated runtime configuration.
type Config struct {
	Server    Server
	Providers map[string]*Provider // keyed by provider name
	Models    map[string]ModelRoute

	// FallbackProviders is the ordered list of providers used for inbound
	// model names that do not resolve to a configured model or known provider
	// prefix. They are tried according to FallbackSelection.
	FallbackProviders []string
	// FallbackSelection controls the order fallback providers are tried in:
	// TargetSelectionSequential (default) or TargetSelectionShuffle.
	FallbackSelection string
}

// Server holds top-level server settings.
type Server struct {
	Listen         string
	LogDir         string
	LogRetention   time.Duration // detail logs older than this are deleted; 0 disables
	ClientKeys     []string
	GlobalBlackout time.Duration
	// MaxRetries is deprecated and no longer has any effect: a key is never
	// retried in place. A bad output advances to the next key without blackout;
	// a provider error blacks out the key and advances. The field is retained so
	// existing configs that still set max_retries continue to load without error.
	MaxRetries        int
	GoodFinishReasons map[string]bool

	// ConfigReloadInterval controls automatic config reload when the file
	// changes. Zero disables polling; SIGHUP / systemctl reload still reloads.
	ConfigReloadInterval time.Duration

	// CoerceTrailingAssistant rewrites a trailing non-user/tool/function turn
	// to role "user" (and flips pronouns in that turn) at request ingest.
	// Default true when the TOML key is omitted.
	CoerceTrailingAssistant bool

	// GeneratedClientKey is set when client_keys was empty and a random key
	// was generated to secure the inbound API. Empty otherwise.
	GeneratedClientKey string

	// CORS lets web apps in a browser (such as annai) call the API (/v1/... and
	// /healthz, never the web UI). On by default: the API asks for a client key
	// in the Authorization header and uses no cookies, so allowing other origins
	// gives a page nothing it could not do with the key anyway. cors = false
	// turns it off.
	CORS bool
	// CORSOrigins limits CORS to these exact origins (e.g. "https://annai.guide").
	// Empty means any origin.
	CORSOrigins []string

	// Streaming sends a request that asks for a stream ("stream": true) as one,
	// piece by piece as the provider writes it. On by default; streaming = false
	// still answers such a request as a stream, but only once the reply is
	// complete.
	Streaming bool

	// FFmpeg is the ffmpeg program used to convert speech (e.g. to Opus).
	FFmpeg string
	// AudioTimeout limits each speech or transcription attempt (the shorter of
	// it and the provider's timeout), so a hung provider is failed over in time.
	AudioTimeout time.Duration
}

// SpeechFormats are the speech formats a client may ask for, and a speech
// target may ask its provider for.
var SpeechFormats = []string{"opus", "mp3", "aac", "flac", "wav", "pcm"}

// SpeechTarget holds a route's speech settings for one of its targets.
type SpeechTarget struct {
	Voice  string // the voice to use on this target; empty: the client's
	Format string // what to ask an OpenAI-kind provider for; empty: pcm
}

// Provider is a single downstream provider after same-name entries are merged.
type Provider struct {
	Name           string
	Kind           string // openai | anthropic | gemini
	BaseURL        string
	Timeout        time.Duration
	Keys           []string // normal keys (random selection, blackout on failure)
	FallbackKeys   []string // tried in order, never blacked out
	FallbackModels []string // models allowed on fallback keys (empty => all allowed)

	// Audio selects the audio API dialect for kind=openai: "openai" (default)
	// or "stepfun", whose transcription lives on its own SSE route.
	Audio string

	// SupportsResponses controls Responses API routing for kind=openai.
	// nil = unset (probe then cache); non-nil true forces /responses;
	// non-nil false forces Chat translation (never probe).
	SupportsResponses *bool

	fallbackModelSet map[string]bool
}

// FallbackAllows reports whether a model may be served by this provider's
// fallback keys. An empty FallbackModels list allows every model.
func (p *Provider) FallbackAllows(model string) bool {
	if len(p.FallbackModels) == 0 {
		return true
	}
	return p.fallbackModelSet[model]
}

// FallbackKeySet returns the keys used during the fallback phase. If
// fallback_keys is set, it is used as-is. Otherwise, when fallback_models is
// specified without fallback_keys, the provider's normal keys are reused for
// the fallback round (still never blacked out). When neither is set, there is
// no fallback round for this provider.
func (p *Provider) FallbackKeySet() []string {
	if len(p.FallbackKeys) > 0 {
		return p.FallbackKeys
	}
	if len(p.FallbackModels) > 0 {
		return p.Keys
	}
	return nil
}

// Target is a resolved downstream provider/model pair.
type Target struct {
	Provider string
	Model    string
}

// ModelRoute is the ordered list of downstream targets for an inbound model.
type ModelRoute struct {
	Targets   []Target
	Selection string // TargetSelectionSequential or TargetSelectionShuffle
	// Speech holds per-target speech settings, keyed by "provider/model".
	Speech map[string]SpeechTarget
}

// raw* types mirror the on-disk TOML layout before normalization.
type rawConfig struct {
	Server   rawServer           `toml:"server"`
	Provider []rawProvider       `toml:"provider"`
	Model    map[string]rawModel `toml:"model"`
	Routing  rawRouting          `toml:"routing"`
}

type rawServer struct {
	Listen            string   `toml:"listen"`
	LogDir            string   `toml:"log_dir"`
	LogRetentionDays  *int     `toml:"log_retention_days"`
	ClientKeys        []string `toml:"client_keys"`
	GlobalBlackout    string   `toml:"global_blackout"`
	MaxRetries        int      `toml:"max_retries"`
	GoodFinishReasons []string `toml:"good_finish_reasons"`
	// Poll interval for automatic config reload (e.g. "3s"). "0" disables
	// polling; SIGHUP / systemctl reload still reloads. Default: 3s.
	ConfigReloadInterval    string   `toml:"config_reload_interval"`
	CoerceTrailingAssistant *bool    `toml:"coerce_trailing_assistant"`
	CORS                    *bool    `toml:"cors"`
	CORSOrigins             []string `toml:"cors_origins"`
	Streaming               *bool    `toml:"streaming"`
	FFmpeg                  string   `toml:"ffmpeg"`
	AudioTimeout            string   `toml:"audio_timeout"`
}

type rawProvider struct {
	Name              string   `toml:"name"`
	Kind              string   `toml:"kind"`
	BaseURL           string   `toml:"base_url"`
	Timeout           string   `toml:"timeout"`
	Keys              []string `toml:"keys"`
	FallbackKeys      []string `toml:"fallback_keys"`
	FallbackModels    []string `toml:"fallback_models"`
	SupportsResponses *bool    `toml:"supports_responses"`
	Audio             string   `toml:"audio"`
}

type rawModel struct {
	Targets   []string                   `toml:"targets"`
	Selection string                     `toml:"selection"` // sequential (default) | shuffle
	Speech    map[string]rawSpeechTarget `toml:"speech"`    // keyed by "provider/model"
}

type rawSpeechTarget struct {
	Voice  string `toml:"voice"`
	Format string `toml:"format"`
}

type rawRouting struct {
	// FallbackProvider is the deprecated singular form; still accepted and
	// merged into FallbackProviders for backward compatibility.
	FallbackProvider  string   `toml:"fallback_provider"`
	FallbackProviders []string `toml:"fallback_providers"`
	FallbackSelection string   `toml:"fallback_selection"` // sequential (default) | shuffle | random
}

// Load reads and validates the configuration at path.
func Load(path string) (*Config, error) {
	var raw rawConfig
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	return build(raw)
}

func build(raw rawConfig) (*Config, error) {
	cfg := &Config{
		Providers: make(map[string]*Provider),
		Models:    make(map[string]ModelRoute),
	}

	// Server section with defaults.
	srv := Server{
		Listen:      firstNonEmpty(raw.Server.Listen, DefaultListen),
		LogDir:      firstNonEmpty(raw.Server.LogDir, DefaultLogDir),
		ClientKeys:  raw.Server.ClientKeys,
		MaxRetries:  raw.Server.MaxRetries,
		CORS:        raw.Server.CORS == nil || *raw.Server.CORS,
		CORSOrigins: raw.Server.CORSOrigins,
	}
	// When no client keys are configured, generate a random one so the inbound
	// API is secured by default rather than left open.
	if len(srv.ClientKeys) == 0 {
		key, err := generateClientKey()
		if err != nil {
			return nil, fmt.Errorf("generate client key: %w", err)
		}
		srv.ClientKeys = []string{key}
		srv.GeneratedClientKey = key
	}
	if srv.MaxRetries < 0 {
		srv.MaxRetries = 0
	}
	// Log retention: default ~2 months; explicit 0 (or negative) disables cleanup.
	retentionDays := DefaultLogRetentionDays
	if raw.Server.LogRetentionDays != nil {
		retentionDays = *raw.Server.LogRetentionDays
	}
	if retentionDays < 0 {
		retentionDays = 0
	}
	srv.LogRetention = time.Duration(retentionDays) * 24 * time.Hour
	if raw.Server.GlobalBlackout == "" {
		srv.GlobalBlackout = DefaultGlobalBlackout
	} else {
		d, err := time.ParseDuration(raw.Server.GlobalBlackout)
		if err != nil {
			return nil, fmt.Errorf("server.global_blackout: %w", err)
		}
		srv.GlobalBlackout = d
	}
	reasons := raw.Server.GoodFinishReasons
	if len(reasons) == 0 {
		reasons = DefaultGoodFinishReasons
	}
	srv.GoodFinishReasons = make(map[string]bool, len(reasons))
	for _, r := range reasons {
		srv.GoodFinishReasons[strings.ToLower(strings.TrimSpace(r))] = true
	}
	if raw.Server.ConfigReloadInterval == "" {
		srv.ConfigReloadInterval = DefaultConfigReloadInterval
	} else {
		s, intervalRaw := strings.TrimSpace(raw.Server.ConfigReloadInterval), raw.Server.ConfigReloadInterval
		if s == "0" || s == "0s" {
			srv.ConfigReloadInterval = 0
		} else {
			d, err := time.ParseDuration(intervalRaw)
			if err != nil {
				return nil, fmt.Errorf("server.config_reload_interval: %w", err)
			}
			if d < 0 {
				return nil, fmt.Errorf("server.config_reload_interval: must be >= 0")
			}
			srv.ConfigReloadInterval = d
		}
	}
	srv.CoerceTrailingAssistant = true
	if raw.Server.CoerceTrailingAssistant != nil {
		srv.CoerceTrailingAssistant = *raw.Server.CoerceTrailingAssistant
	}
	srv.Streaming = true
	if raw.Server.Streaming != nil {
		srv.Streaming = *raw.Server.Streaming
	}
	srv.FFmpeg = DefaultFFmpeg
	if f := strings.TrimSpace(raw.Server.FFmpeg); f != "" {
		srv.FFmpeg = f
	}
	srv.AudioTimeout = DefaultAudioTimeout
	if raw.Server.AudioTimeout != "" {
		d, err := time.ParseDuration(raw.Server.AudioTimeout)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("server.audio_timeout: must be a positive duration, got %q", raw.Server.AudioTimeout)
		}
		srv.AudioTimeout = d
	}
	cfg.Server = srv

	// Merge providers by name (#17 combine keys, #18 later timeout/base/kind wins).
	for i := range raw.Provider {
		rp := raw.Provider[i]
		name := strings.TrimSpace(rp.Name)
		if name == "" {
			return nil, fmt.Errorf("provider #%d: missing name", i)
		}
		var timeout time.Duration
		hasTimeout := rp.Timeout != ""
		if hasTimeout {
			d, err := time.ParseDuration(rp.Timeout)
			if err != nil {
				return nil, fmt.Errorf("provider %q timeout: %w", name, err)
			}
			timeout = d
		}

		p, ok := cfg.Providers[name]
		if !ok {
			p = &Provider{Name: name, Timeout: DefaultTimeout}
			cfg.Providers[name] = p
		}
		// Combine credential lists.
		p.Keys = append(p.Keys, rp.Keys...)
		p.FallbackKeys = append(p.FallbackKeys, rp.FallbackKeys...)
		p.FallbackModels = append(p.FallbackModels, rp.FallbackModels...)
		// Later definitions overwrite scalar fields when provided.
		if rp.Kind != "" {
			p.Kind = strings.ToLower(strings.TrimSpace(rp.Kind))
		}
		if rp.BaseURL != "" {
			p.BaseURL = strings.TrimRight(strings.TrimSpace(rp.BaseURL), "/")
		}
		if hasTimeout {
			p.Timeout = timeout
		}
		if rp.SupportsResponses != nil {
			v := *rp.SupportsResponses
			p.SupportsResponses = &v
		}
		if a := strings.ToLower(strings.TrimSpace(rp.Audio)); a != "" {
			p.Audio = a
		}
	}

	for name, p := range cfg.Providers {
		if p.Kind == "" {
			return nil, fmt.Errorf("provider %q: missing kind", name)
		}
		switch p.Kind {
		case "openai", "anthropic", "gemini":
		default:
			return nil, fmt.Errorf("provider %q: unknown kind %q", name, p.Kind)
		}
		if p.BaseURL == "" {
			return nil, fmt.Errorf("provider %q: missing base_url", name)
		}
		switch {
		case p.Audio == "":
			p.Audio = "openai"
		case p.Audio != "openai" && p.Audio != "stepfun":
			return nil, fmt.Errorf("provider %q: unknown audio %q (want \"openai\" or \"stepfun\")", name, p.Audio)
		case p.Kind != "openai":
			return nil, fmt.Errorf("provider %q: audio is only for kind \"openai\"", name)
		}
		p.fallbackModelSet = make(map[string]bool, len(p.FallbackModels))
		for _, m := range p.FallbackModels {
			p.fallbackModelSet[m] = true
		}
	}

	// Model routes. A target is either a "provider/model" pair or the name of
	// another defined model, which is expanded in place (recursively).
	rawTargets := make(map[string][]string, len(raw.Model))
	modelSelection := make(map[string]string, len(raw.Model))
	for name, rm := range raw.Model {
		if len(rm.Targets) == 0 {
			return nil, fmt.Errorf("model %q: no targets", name)
		}
		rawTargets[name] = rm.Targets
		sel, err := normalizeTargetSelection(rm.Selection, name)
		if err != nil {
			return nil, err
		}
		modelSelection[name] = sel
	}
	for name := range rawTargets {
		targets, err := expandModel(name, rawTargets, nil)
		if err != nil {
			return nil, err
		}
		speech, err := routeSpeech(name, raw.Model, targets)
		if err != nil {
			return nil, err
		}
		cfg.Models[name] = ModelRoute{Targets: targets, Selection: modelSelection[name], Speech: speech}
	}

	// Resolve the fallback providers list. The deprecated singular
	// fallback_provider is merged in after the plural fallback_providers,
	// preserving order and dropping duplicates and empties.
	seen := make(map[string]bool)
	var fallbacks []string
	addFallback := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		fallbacks = append(fallbacks, name)
	}
	for _, name := range raw.Routing.FallbackProviders {
		addFallback(name)
	}
	addFallback(raw.Routing.FallbackProvider)

	for _, name := range fallbacks {
		if _, ok := cfg.Providers[name]; ok {
			cfg.FallbackProviders = append(cfg.FallbackProviders, name)
		} else {
			log.Printf("WARNING: routing fallback provider %q is not a defined provider; ignoring", name)
		}
	}

	sel, err := normalizeSelection(raw.Routing.FallbackSelection, "routing.fallback_selection")
	if err != nil {
		return nil, err
	}
	cfg.FallbackSelection = sel

	return cfg, nil
}

// routeSpeech builds a route's per-target speech settings: those of the routes
// it includes (in order), then its own on top. Its own keys must name one of
// its expanded targets. expandModel has already ruled out cycles.
func routeSpeech(name string, raw map[string]rawModel, targets []Target) (map[string]SpeechTarget, error) {
	valid := make(map[string]bool, len(targets))
	for _, t := range targets {
		valid[t.Provider+"/"+t.Model] = true
	}
	out := make(map[string]SpeechTarget)
	for _, t := range raw[name].Targets {
		if _, nested := raw[t]; !nested {
			continue
		}
		inner, err := routeSpeech(t, raw, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range inner {
			out[k] = v
		}
	}
	for key, rs := range raw[name].Speech {
		if targets != nil && !valid[key] {
			return nil, fmt.Errorf("model %q: speech settings for %q, which is not one of its targets", name, key)
		}
		format := strings.ToLower(strings.TrimSpace(rs.Format))
		if format != "" && !slices.Contains(SpeechFormats, format) {
			return nil, fmt.Errorf("model %q: speech %q: unknown format %q (want one of %s)", name, key, rs.Format, strings.Join(SpeechFormats, ", "))
		}
		out[key] = SpeechTarget{Voice: strings.TrimSpace(rs.Voice), Format: format}
	}
	return out, nil
}

// expandModel resolves one model's target list into a flat slice of Targets.
// A target that exactly matches another defined model name is expanded in
// place (recursively); otherwise it must be a "provider/model" pair. Cyclic
// references are reported as errors.
func expandModel(name string, raw map[string][]string, stack []string) ([]Target, error) {
	for _, s := range stack {
		if s == name {
			return nil, fmt.Errorf("model %q: cyclic target reference: %s",
				name, strings.Join(append(append([]string{}, stack...), name), " -> "))
		}
	}
	next := append(append([]string{}, stack...), name)

	var out []Target
	for _, t := range raw[name] {
		t = strings.TrimSpace(t)
		if _, isModel := raw[t]; isModel {
			sub, err := expandModel(t, raw, next)
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
			continue
		}
		prov, model, err := splitTarget(t)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", name, err)
		}
		out = append(out, Target{Provider: prov, Model: model})
	}
	return out, nil
}

// splitTarget parses a "provider/model" string. The model portion may itself
// contain slashes (e.g. gemini "models/x"), so only the first slash splits.
func splitTarget(s string) (provider, model string, err error) {
	s = strings.TrimSpace(s)
	idx := strings.Index(s, "/")
	if idx <= 0 || idx == len(s)-1 {
		return "", "", fmt.Errorf("target %q must be in provider/model form", s)
	}
	return s[:idx], s[idx+1:], nil
}

// generateClientKey returns a random "sk-mai-" prefixed token.
func generateClientKey() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "sk-mai-" + hex.EncodeToString(b[:]), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func normalizeTargetSelection(s, modelName string) (string, error) {
	return normalizeSelection(s, fmt.Sprintf("model %q", modelName))
}

// normalizeSelection maps a selection string to a canonical selection method.
// An empty value defaults to sequential. "random" is an alias for "shuffle".
// ctx is used to prefix error messages (e.g. `model "free"`).
func normalizeSelection(s, ctx string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "":
		return TargetSelectionSequential, nil
	case TargetSelectionSequential:
		return TargetSelectionSequential, nil
	case TargetSelectionShuffle, "random":
		return TargetSelectionShuffle, nil
	default:
		return "", fmt.Errorf("%s: unknown selection %q (want %q, %q, or random)",
			ctx, s, TargetSelectionSequential, TargetSelectionShuffle)
	}
}
