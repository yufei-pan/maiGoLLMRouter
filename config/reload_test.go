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
