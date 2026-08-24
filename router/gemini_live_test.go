//go:build live

package router

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maiGoLLMRouter/config"
	"maiGoLLMRouter/provider"
)

func TestLiveGeminiResponsesViaRouter(t *testing.T) {
	path := os.Getenv("MAI_CONFIG")
	if path == "" {
		path = filepath.Join("..", "config.toml")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Providers["google"] == nil || cfg.Providers["google"].Kind != "gemini" {
		t.Fatal("config.toml has no google/gemini provider")
	}
	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = "gemini-3-flash-preview"
	}

	r := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	res := r.Execute(ctx, provider.OpResponses, "google/"+model, map[string]any{
		"model": "ignored",
		"input": "Reply with exactly the word PONG and nothing else.",
	})
	if !res.Success {
		t.Fatalf("router Execute failed: status=%d attempts=%+v body=%s", res.Status, res.Attempts, res.Body)
	}
	if res.Provider != "google" {
		t.Fatalf("provider=%q, want google", res.Provider)
	}
	var parsed map[string]any
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["object"] != "response" || parsed["status"] != "completed" {
		t.Fatalf("body=%s", res.Body)
	}
	if !strings.Contains(strings.ToUpper(string(res.Body)), "PONG") {
		t.Fatalf("expected PONG: %s", res.Body)
	}
}
