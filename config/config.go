// Package config loads and normalizes the TOML configuration for the router.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
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
)

// DefaultGoodFinishReasons are the normalized finish reasons treated as a
// successful completion when none are configured.
var DefaultGoodFinishReasons = []string{"stop", "length", "tool_calls", "function_call", "end_turn"}

// Config is the fully resolved, validated runtime configuration.
type Config struct {
	Server           Server
	Providers        map[string]*Provider // keyed by provider name
	Models           map[string]ModelRoute
	FallbackProvider string
}

// Server holds top-level server settings.
type Server struct {
	Listen            string
	LogDir            string
	ClientKeys        []string
	GlobalBlackout    time.Duration
	MaxRetries        int
	GoodFinishReasons map[string]bool

	// GeneratedClientKey is set when client_keys was empty and a random key
	// was generated to secure the inbound API. Empty otherwise.
	GeneratedClientKey string
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
	Targets []Target
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
	ClientKeys        []string `toml:"client_keys"`
	GlobalBlackout    string   `toml:"global_blackout"`
	MaxRetries        int      `toml:"max_retries"`
	GoodFinishReasons []string `toml:"good_finish_reasons"`
}

type rawProvider struct {
	Name           string   `toml:"name"`
	Kind           string   `toml:"kind"`
	BaseURL        string   `toml:"base_url"`
	Timeout        string   `toml:"timeout"`
	Keys           []string `toml:"keys"`
	FallbackKeys   []string `toml:"fallback_keys"`
	FallbackModels []string `toml:"fallback_models"`
}

type rawModel struct {
	Targets []string `toml:"targets"`
}

type rawRouting struct {
	FallbackProvider string `toml:"fallback_provider"`
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
		Providers:        make(map[string]*Provider),
		Models:           make(map[string]ModelRoute),
		FallbackProvider: strings.TrimSpace(raw.Routing.FallbackProvider),
	}

	// Server section with defaults.
	srv := Server{
		Listen:     firstNonEmpty(raw.Server.Listen, DefaultListen),
		LogDir:     firstNonEmpty(raw.Server.LogDir, DefaultLogDir),
		ClientKeys: raw.Server.ClientKeys,
		MaxRetries: raw.Server.MaxRetries,
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
		p.fallbackModelSet = make(map[string]bool, len(p.FallbackModels))
		for _, m := range p.FallbackModels {
			p.fallbackModelSet[m] = true
		}
	}

	// Model routes. A target is either a "provider/model" pair or the name of
	// another defined model, which is expanded in place (recursively).
	rawTargets := make(map[string][]string, len(raw.Model))
	for name, rm := range raw.Model {
		if len(rm.Targets) == 0 {
			return nil, fmt.Errorf("model %q: no targets", name)
		}
		rawTargets[name] = rm.Targets
	}
	for name := range rawTargets {
		targets, err := expandModel(name, rawTargets, nil)
		if err != nil {
			return nil, err
		}
		cfg.Models[name] = ModelRoute{Targets: targets}
	}

	if cfg.FallbackProvider != "" {
		if _, ok := cfg.Providers[cfg.FallbackProvider]; !ok {
			log.Printf("WARNING: routing.fallback_provider %q is not a defined provider; treating as no fallback", cfg.FallbackProvider)
			cfg.FallbackProvider = ""
		}
	}

	return cfg, nil
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
