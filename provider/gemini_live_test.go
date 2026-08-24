//go:build live

package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maiGoLLMRouter/config"
)

func liveGeminiCreds(t *testing.T) (baseURL string, keys []string, model string) {
	t.Helper()
	path := os.Getenv("MAI_CONFIG")
	if path == "" {
		path = filepath.Join("..", "config.toml")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p := cfg.Providers["google"]
	if p == nil || p.Kind != "gemini" {
		t.Fatal("config.toml has no google/gemini provider")
	}
	if len(p.Keys) == 0 {
		t.Fatal("google provider has no keys")
	}
	model = os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = "gemini-3-flash-preview"
	}
	return p.BaseURL, append([]string(nil), p.Keys...), model
}

func liveGeminiCall(t *testing.T, ctx context.Context, baseURL string, keys []string, req Request) *Response {
	t.Helper()
	var last *Response
	for i, key := range keys {
		resp, err := Call(ctx, http.DefaultClient, "gemini", baseURL, key, req)
		if err != nil {
			t.Logf("key %d transport: %v", i, err)
			continue
		}
		last = resp
		if resp.OK() {
			return resp
		}
		if resp.HTTPStatus == 429 || resp.HTTPStatus == 503 {
			t.Logf("key %d http=%d, trying next key", i, resp.HTTPStatus)
			continue
		}
		return resp
	}
	if last == nil {
		t.Fatal("all gemini keys failed at transport")
	}
	return last
}

func TestLiveGeminiResponsesText(t *testing.T) {
	baseURL, keys, model := liveGeminiCreds(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	resp := liveGeminiCall(t, ctx, baseURL, keys, Request{
		Op: OpResponses, Model: model,
		Body: map[string]any{
			"input":        "Reply with exactly the word PONG and nothing else.",
			"instructions": "You are a terse test bot.",
		},
	})
	if !resp.OK() {
		t.Fatalf("http=%d body=%s", resp.HTTPStatus, resp.RawResponse)
	}
	if !resp.HasContent || resp.FinishReason != "stop" {
		t.Fatalf("finish=%q hasContent=%v body=%s", resp.FinishReason, resp.HasContent, resp.OpenAIBody)
	}
	var parsed map[string]any
	if err := json.Unmarshal(resp.OpenAIBody, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["object"] != "response" || parsed["status"] != "completed" {
		t.Fatalf("envelope=%s", resp.OpenAIBody)
	}
	if !strings.Contains(strings.ToUpper(string(resp.OpenAIBody)), "PONG") {
		t.Fatalf("expected PONG in output: %s", resp.OpenAIBody)
	}
}

func TestLiveGeminiResponsesJSONSchema(t *testing.T) {
	baseURL, keys, model := liveGeminiCreds(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	resp := liveGeminiCall(t, ctx, baseURL, keys, Request{
		Op: OpResponses, Model: model,
		Body: map[string]any{
			"input": "The capital of France.",
			"text": map[string]any{"format": map[string]any{
				"type": "json_schema", "name": "Cap",
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
					},
					"required":             []any{"city"},
					"additionalProperties": false,
				},
			}},
		},
	})
	if !resp.OK() {
		t.Fatalf("http=%d body=%s", resp.HTTPStatus, resp.RawResponse)
	}
	raw := string(resp.OpenAIBody)
	if !strings.Contains(strings.ToLower(raw), "paris") {
		t.Fatalf("expected paris in json output: %s", raw)
	}
}

func TestLiveGeminiResponsesFunctionCall(t *testing.T) {
	baseURL, keys, model := liveGeminiCreds(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tools := []any{map[string]any{
		"type": "function", "name": "get_temp",
		"description": "Get a temperature in celsius",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string"},
			},
			"required": []any{"city"},
		},
	}}
	first := liveGeminiCall(t, ctx, baseURL, keys, Request{
		Op: OpResponses, Model: model,
		Body: map[string]any{
			"input":       "What is the temperature in Paris? Use the get_temp tool.",
			"tools":       tools,
			"tool_choice": "required",
		},
	})
	if !first.OK() {
		t.Fatalf("first http=%d body=%s", first.HTTPStatus, first.RawResponse)
	}
	var parsed map[string]any
	if err := json.Unmarshal(first.OpenAIBody, &parsed); err != nil {
		t.Fatal(err)
	}
	var fc map[string]any
	for _, item := range parsed["output"].([]any) {
		m, _ := item.(map[string]any)
		if m["type"] == "function_call" {
			fc = m
			break
		}
	}
	if fc == nil {
		t.Fatalf("no function_call in %s", first.OpenAIBody)
	}
	callID, _ := fc["call_id"].(string)

	second := liveGeminiCall(t, ctx, baseURL, keys, Request{
		Op: OpResponses, Model: model,
		Body: map[string]any{
			"input": []any{
				map[string]any{"role": "user", "content": "What is the temperature in Paris? Use the get_temp tool."},
				fc,
				map[string]any{"type": "function_call_output", "call_id": callID, "output": `{"celsius":21}`},
			},
			"tools": tools,
		},
	})
	if !second.OK() {
		t.Fatalf("second http=%d raw=%s openai=%s", second.HTTPStatus, second.RawResponse, second.OpenAIBody)
	}
	if !strings.Contains(string(second.OpenAIBody), "21") {
		t.Fatalf("expected tool result in final answer: %s", second.OpenAIBody)
	}
}

func TestLiveGeminiResponsesOpenAIExtrasDoNot400(t *testing.T) {
	baseURL, keys, model := liveGeminiCreds(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	resp := liveGeminiCall(t, ctx, baseURL, keys, Request{
		Op: OpResponses, Model: model,
		Body: map[string]any{
			"input":               "Say OK.",
			"parallel_tool_calls": true,
			"metadata":            map[string]any{"test": "1"},
		},
	})
	if !resp.OK() {
		t.Fatalf("openai extras caused gemini error http=%d body=%s", resp.HTTPStatus, resp.RawResponse)
	}
}
