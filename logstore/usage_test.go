package logstore

import (
	"encoding/json"
	"testing"
)

func TestParseUsageOpenAI(t *testing.T) {
	raw := []byte(`{"usage":{"prompt_tokens":12,"completion_tokens":4}}`)
	p, c, ok := parseUsageFromBody(raw)
	if !ok || p != 12 || c != 4 {
		t.Fatalf("got ok=%v p=%d c=%d", ok, p, c)
	}
}

func TestParseUsageAnthropic(t *testing.T) {
	raw := []byte(`{"usage":{"input_tokens":20,"output_tokens":6}}`)
	p, c, ok := parseUsageFromBody(raw)
	if !ok || p != 20 || c != 6 {
		t.Fatalf("got ok=%v p=%d c=%d", ok, p, c)
	}
}

func TestParseUsageGemini(t *testing.T) {
	raw := []byte(`{"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":9}}`)
	p, c, ok := parseUsageFromBody(raw)
	if !ok || p != 30 || c != 9 {
		t.Fatalf("got ok=%v p=%d c=%d", ok, p, c)
	}
}

func TestParseUsageGeminiThoughts(t *testing.T) {
	raw := []byte(`{"usageMetadata":{"promptTokenCount":8,"totalTokenCount":69,"thoughtsTokenCount":61}}`)
	p, c, ok := parseUsageFromBody(raw)
	if !ok || p != 8 || c != 61 {
		t.Fatalf("got ok=%v p=%d c=%d", ok, p, c)
	}
}

func TestComputeUsageSuccessUsesResponseOnly(t *testing.T) {
	resp := json.RawMessage(`{"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	attempts := []json.RawMessage{
		mustRaw(t, map[string]any{
			"outcome":  "bad_output",
			"response": `{"usage":{"prompt_tokens":10,"completion_tokens":99}}`,
		}),
	}
	p, c, ok := computeUsage(true, resp, attempts)
	if !ok || p != 10 || c != 2 {
		t.Fatalf("got ok=%v p=%d c=%d", ok, p, c)
	}
}

func TestComputeUsageFailedSumsFailedAttemptOutputs(t *testing.T) {
	resp := json.RawMessage(`{"error":{"message":"exhausted"}}`)
	attempts := []json.RawMessage{
		mustRaw(t, map[string]any{
			"outcome":  "bad_output",
			"response": `{"usage":{"prompt_tokens":100,"completion_tokens":5}}`,
		}),
		mustRaw(t, map[string]any{
			"outcome":  "bad_output",
			"response": `{"usage":{"prompt_tokens":100,"completion_tokens":7}}`,
		}),
		mustRaw(t, map[string]any{
			"outcome": "skipped_blackout",
		}),
	}
	p, c, ok := computeUsage(false, resp, attempts)
	if !ok || p != 100 || c != 12 {
		t.Fatalf("got ok=%v p=%d c=%d", ok, p, c)
	}
}

func TestComputeUsageFailedIgnoresNonFailedAttempts(t *testing.T) {
	resp := json.RawMessage(`{"error":{"message":"exhausted"}}`)
	attempts := []json.RawMessage{
		mustRaw(t, map[string]any{
			"outcome":  "provider_error",
			"response": `{"usage":{"prompt_tokens":50,"completion_tokens":3}}`,
		}),
	}
	p, c, ok := computeUsage(false, resp, attempts)
	if !ok || p != 50 || c != 3 {
		t.Fatalf("got ok=%v p=%d c=%d", ok, p, c)
	}
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
