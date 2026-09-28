package provider

import "testing"

func TestGeminiChatUnwrapsGoogleExtraBodyThinkingConfig(t *testing.T) {
	body := buildGeminiChatBody(Request{
		Model: "gemma-4-31b-it",
		Body: map[string]any{
			"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
			"temperature": 0.0,
			"max_tokens":  float64(16),
			"extra_body": map[string]any{
				"google": map[string]any{
					"thinking_config": map[string]any{
						"thinking_level": "minimal",
					},
				},
			},
		},
	})
	gen, _ := body["generationConfig"].(map[string]any)
	if _, leaked := gen["extra_body"]; leaked {
		t.Fatalf("extra_body leaked into generationConfig: %v", gen)
	}
	if _, leaked := body["extra_body"]; leaked {
		t.Fatalf("extra_body leaked onto Gemini body: %v", body)
	}
	tc, _ := gen["thinkingConfig"].(map[string]any)
	if asString(tc, "thinking_level") != "minimal" {
		t.Fatalf("thinkingConfig=%v", gen["thinkingConfig"])
	}
}

func TestGeminiChatUnwrapsDoubleNestedExtraBody(t *testing.T) {
	body := buildGeminiChatBody(Request{
		Model: "gemma-4-31b-it",
		Body: map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"extra_body": map[string]any{
				"extra_body": map[string]any{
					"google": map[string]any{
						"thinking_config": map[string]any{
							"thinking_level": "high",
						},
					},
				},
			},
		},
	})
	gen, _ := body["generationConfig"].(map[string]any)
	if _, leaked := gen["extra_body"]; leaked {
		t.Fatalf("extra_body leaked into generationConfig: %v", gen)
	}
	tc, _ := gen["thinkingConfig"].(map[string]any)
	if asString(tc, "thinking_level") != "high" {
		t.Fatalf("thinkingConfig=%v", gen["thinkingConfig"])
	}
}
