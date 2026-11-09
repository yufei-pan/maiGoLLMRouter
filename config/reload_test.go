package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReloadPreservesClientKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	initial := `
[server]
client_keys = []

[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com/v1"
keys = ["sk-test"]

[model."gpt"]
targets = ["openai/gpt-4o"]
`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(first.Server.ClientKeys) != 1 {
		t.Fatalf("want generated client key, got %v", first.Server.ClientKeys)
	}

	updated := `
[server]
client_keys = []

[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com/v1"
keys = ["sk-test", "sk-test-2"]

[model."gpt"]
targets = ["openai/gpt-4o"]
`
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := Reload(path, first)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if second.Server.ClientKeys[0] != first.Server.ClientKeys[0] {
		t.Fatalf("client key changed on reload: %q -> %q", first.Server.ClientKeys[0], second.Server.ClientKeys[0])
	}
	if len(second.Providers["openai"].Keys) != 2 {
		t.Fatalf("want 2 provider keys after reload, got %d", len(second.Providers["openai"].Keys))
	}
}

// keysConfig is a minimal config whose [server] client_keys is keys, a TOML
// array such as `[]` or `["sk-file"]`.
func keysConfig(keys string) string {
	return `
[server]
client_keys = ` + keys + `

[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com/v1"
keys = ["sk-test"]
`
}

// rewrite replaces the config at path with content.
func rewrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReloadEmptyKeysDropsFileKeys(t *testing.T) {
	path := writeTemp(t, keysConfig(`["sk-file"]`))
	first, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rewrite(t, path, keysConfig(`[]`))
	next, err := Reload(path, first)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	keys := next.Server.ClientKeys
	if len(keys) != 1 {
		t.Fatalf("want one client key, got %d", len(keys))
	}
	if keys[0] == "sk-file" {
		t.Fatal("the file's old key survived emptying client_keys")
	}
	if next.Server.GeneratedClientKey != keys[0] {
		t.Errorf("GeneratedClientKey should be the new generated key")
	}
}

func TestReloadKeepsGeneratedKeyTwice(t *testing.T) {
	path := writeTemp(t, keysConfig(`[]`))
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	gen := cfg.Server.GeneratedClientKey
	if gen == "" {
		t.Fatal("expected a generated client key")
	}
	for i := 1; i <= 2; i++ {
		cfg, err = Reload(path, cfg)
		if err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
		if keys := cfg.Server.ClientKeys; len(keys) != 1 || keys[0] != gen {
			t.Errorf("reload %d: client keys should be just the generated key (got %d key(s))", i, len(keys))
		}
		if cfg.Server.GeneratedClientKey != gen {
			t.Errorf("reload %d: GeneratedClientKey should stay the generated key", i)
		}
	}
}

func TestReloadFileKeysReplaceGenerated(t *testing.T) {
	path := writeTemp(t, keysConfig(`[]`))
	first, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rewrite(t, path, keysConfig(`["sk-new"]`))
	next, err := Reload(path, first)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if keys := next.Server.ClientKeys; len(keys) != 1 || keys[0] != "sk-new" {
		t.Errorf("client keys = %v, want [sk-new]", keys)
	}
	if next.Server.GeneratedClientKey != "" {
		t.Errorf("GeneratedClientKey should be empty when the file lists keys")
	}
}
