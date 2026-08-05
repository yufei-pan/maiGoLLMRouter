package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSameNameProviderMergeAndTimeoutOverride(t *testing.T) {
	path := writeTemp(t, `
[[provider]]
name = "google"
kind = "gemini"
base_url = "https://a.example"
timeout = "10s"
keys = ["k1"]
fallback_keys = ["fb1"]

[[provider]]
name = "google"
kind = "gemini"
base_url = "https://b.example"
timeout = "45s"
keys = ["k2", "k3"]
fallback_keys = ["fb2"]
fallback_models = ["m1"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := cfg.Providers["google"]
	if p == nil {
		t.Fatal("google provider missing")
	}
	if got, want := len(p.Keys), 3; got != want { // combined (#17)
		t.Errorf("keys = %d, want %d", got, want)
	}
	if got, want := len(p.FallbackKeys), 2; got != want {
		t.Errorf("fallback_keys = %d, want %d", got, want)
	}
	if p.Timeout != 45*time.Second { // later wins (#18)
		t.Errorf("timeout = %v, want 45s", p.Timeout)
	}
	if p.BaseURL != "https://b.example" {
		t.Errorf("base_url = %q, want later value", p.BaseURL)
	}
	if !p.FallbackAllows("m1") || p.FallbackAllows("other") { // gating (#8)
		t.Errorf("FallbackAllows gating incorrect")
	}
}

func TestModelTargetsParsed(t *testing.T) {
	path := writeTemp(t, `
[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com"
keys = ["k"]

[model."gpt-4o"]
targets = ["openai/gpt-4o", "openai/gpt-4o-mini"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := cfg.Models["gpt-4o"]
	if len(r.Targets) != 2 || r.Targets[0].Provider != "openai" || r.Targets[0].Model != "gpt-4o" {
		t.Errorf("unexpected targets: %+v", r.Targets)
	}
	if cfg.Server.GlobalBlackout != DefaultGlobalBlackout {
		t.Errorf("default blackout = %v", cfg.Server.GlobalBlackout)
	}
}

func TestClientKeyGeneratedWhenEmpty(t *testing.T) {
	path := writeTemp(t, `
[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com"
keys = ["k"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.GeneratedClientKey == "" {
		t.Fatal("expected a generated client key")
	}
	if len(cfg.Server.ClientKeys) != 1 || cfg.Server.ClientKeys[0] != cfg.Server.GeneratedClientKey {
		t.Errorf("client_keys should contain the generated key: %+v", cfg.Server.ClientKeys)
	}
	if !strings.HasPrefix(cfg.Server.GeneratedClientKey, "sk-mai-") {
		t.Errorf("unexpected key format: %q", cfg.Server.GeneratedClientKey)
	}
}

func TestClientKeyNotGeneratedWhenProvided(t *testing.T) {
	path := writeTemp(t, `
[server]
client_keys = ["sk-mine"]

[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com"
keys = ["k"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.GeneratedClientKey != "" {
		t.Errorf("should not generate a key when one is configured")
	}
}

func TestModelReferencesExpanded(t *testing.T) {
	path := writeTemp(t, `
[[provider]]
name = "google"
kind = "gemini"
base_url = "https://g/v1beta"
keys = ["k"]

[[provider]]
name = "openrouter"
kind = "openai"
base_url = "https://o/api/v1"
keys = ["k"]

[model."free"]
targets = ["google/gemma-4-31b-it", "openrouter/openrouter/free"]

[model."think"]
targets = ["google/gemini-3.5-flash", "free"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	think := cfg.Models["think"].Targets
	want := []Target{
		{"google", "gemini-3.5-flash"},
		{"google", "gemma-4-31b-it"},
		{"openrouter", "openrouter/free"},
	}
	if len(think) != len(want) {
		t.Fatalf("think targets = %+v, want %+v", think, want)
	}
	for i := range want {
		if think[i] != want[i] {
			t.Errorf("think[%d] = %+v, want %+v", i, think[i], want[i])
		}
	}
}

func TestModelReferenceCycleRejected(t *testing.T) {
	path := writeTemp(t, `
[[provider]]
name = "google"
kind = "gemini"
base_url = "https://g/v1beta"
keys = ["k"]

[model."a"]
targets = ["google/x", "b"]

[model."b"]
targets = ["a"]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected cyclic reference error")
	}
}

func TestUnknownKindRejected(t *testing.T) {
	path := writeTemp(t, `
[[provider]]
name = "x"
kind = "bogus"
base_url = "https://x"
keys = ["k"]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestModelSelectionDefaultSequential(t *testing.T) {
	path := writeTemp(t, `
[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com"
keys = ["k"]

[model."m"]
targets = ["openai/a", "openai/b"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Models["m"].Selection; got != TargetSelectionSequential {
		t.Errorf("selection = %q, want %q", got, TargetSelectionSequential)
	}
}

func TestModelSelectionShuffleParsed(t *testing.T) {
	for _, sel := range []string{"shuffle", "random", "SHUFFLE"} {
		path := writeTemp(t, fmt.Sprintf(`
[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com"
keys = ["k"]

[model."m"]
selection = %q
targets = ["openai/a", "openai/b"]
`, sel))
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load selection=%q: %v", sel, err)
		}
		if got := cfg.Models["m"].Selection; got != TargetSelectionShuffle {
			t.Errorf("selection=%q -> %q, want %q", sel, got, TargetSelectionShuffle)
		}
	}
}

func TestModelSelectionUnknownRejected(t *testing.T) {
	path := writeTemp(t, `
[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com"
keys = ["k"]

[model."m"]
selection = "round_robin"
targets = ["openai/a"]
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown selection")
	}
}

func TestSupportsResponsesOptional(t *testing.T) {
	path := writeTemp(t, `
[[provider]]
name = "a"
kind = "openai"
base_url = "https://a.example"
keys = ["k"]

[[provider]]
name = "b"
kind = "openai"
base_url = "https://b.example"
keys = ["k"]
supports_responses = true

[[provider]]
name = "c"
kind = "openai"
base_url = "https://c.example"
keys = ["k"]
supports_responses = false

[[provider]]
name = "b"
kind = "openai"
base_url = "https://b2.example"
keys = ["k2"]
supports_responses = false
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Providers["a"].SupportsResponses != nil {
		t.Fatalf("a: want unset nil, got %v", *cfg.Providers["a"].SupportsResponses)
	}
	if cfg.Providers["b"].SupportsResponses == nil || *cfg.Providers["b"].SupportsResponses {
		t.Fatalf("b: later supports_responses=false should win, got %#v", cfg.Providers["b"].SupportsResponses)
	}
	if cfg.Providers["c"].SupportsResponses == nil || *cfg.Providers["c"].SupportsResponses {
		t.Fatalf("c: want false, got %#v", cfg.Providers["c"].SupportsResponses)
	}
}

func TestCoerceTrailingAssistantDefaultAndExplicit(t *testing.T) {
	minimal := `
[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com"
keys = ["k"]
`
	cfg, err := Load(writeTemp(t, minimal+`
[server]
client_keys = ["sk"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Server.CoerceTrailingAssistant {
		t.Fatal("omitted coerce_trailing_assistant should default true")
	}

	cfgFalse, err := Load(writeTemp(t, minimal+`
[server]
client_keys = ["sk"]
coerce_trailing_assistant = false
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfgFalse.Server.CoerceTrailingAssistant {
		t.Fatal("explicit false not respected")
	}

	cfgTrue, err := Load(writeTemp(t, minimal+`
[server]
client_keys = ["sk"]
coerce_trailing_assistant = true
`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfgTrue.Server.CoerceTrailingAssistant {
		t.Fatal("explicit true not respected")
	}
}
