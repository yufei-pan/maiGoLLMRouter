package logstore

import (
	"encoding/json"
	"testing"
)

func writeEntries(t *testing.T, s *Store, n int) []Entry {
	t.Helper()
	entries := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		e := Entry{
			Time:         "2026-06-03T00:00:0" + string(rune('0'+i)) + "Z",
			ID:           "req-" + string(rune('a'+i)),
			ClientKey:    "sk-...mask",
			Endpoint:     "/v1/chat/completions",
			InboundModel: "gpt-4o",
			Provider:     "openai",
			Model:        "gpt-4o",
			Success:      true,
			Status:       200,
			LatencyMS:    int64(100 + i),
			Attempts:     []map[string]any{{"provider": "openai"}, {"provider": "google"}},
			Request:      json.RawMessage(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
			Response: json.RawMessage(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`),
		}
		if err := s.Write(e); err != nil {
			t.Fatalf("write: %v", err)
		}
		entries = append(entries, e)
	}
	return entries
}

func TestTailSummariesOmitsBodies(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	writeEntries(t, s, 3)

	sums, err := s.TailSummaries(10)
	if err != nil {
		t.Fatalf("tail summaries: %v", err)
	}
	if len(sums) != 3 {
		t.Fatalf("want 3 summaries, got %d", len(sums))
	}

	// Newest first.
	if sums[0].ID != "req-c" {
		t.Fatalf("want newest req-c first, got %q", sums[0].ID)
	}
	for _, sm := range sums {
		if sm.AttemptsCount != 2 {
			t.Errorf("want attempts_count 2, got %d", sm.AttemptsCount)
		}
		if sm.PromptTokens != 11 || sm.CompletionTokens != 7 {
			t.Errorf("want prompt_tokens=11 completion_tokens=7, got %d/%d", sm.PromptTokens, sm.CompletionTokens)
		}
	}

	// The marshaled summary must not carry request/response/attempt bodies.
	b, err := json.Marshal(sums[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, banned := range []string{"\"request\"", "\"response\"", "\"attempts\":", "hello"} {
		if contains(b, banned) {
			t.Errorf("summary JSON unexpectedly contains %q: %s", banned, b)
		}
	}
	if !contains(b, "\"attempts_count\"") {
		t.Errorf("summary JSON missing attempts_count: %s", b)
	}
}

func TestGetReturnsFullEntry(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	writeEntries(t, s, 3)

	raw, err := s.Get("req-b")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if raw == nil {
		t.Fatal("want entry, got nil")
	}
	var got Entry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "req-b" {
		t.Fatalf("want req-b, got %q", got.ID)
	}
	if len(got.Request) == 0 || len(got.Response) == 0 {
		t.Fatal("full entry should retain request and response bodies")
	}
}

func TestTailSummariesFailedRequestSumsAttemptOutputs(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	e := Entry{
		Time:         "2026-06-03T12:00:00Z",
		ID:           "req-fail",
		Endpoint:     "/v1/chat/completions",
		InboundModel: "gpt-4o",
		Success:      false,
		Status:       502,
		LatencyMS:    250,
		Response:     json.RawMessage(`{"error":{"message":"exhausted","type":"upstream_error"}}`),
		Attempts: []map[string]any{
			{
				"provider": "openai", "model": "gpt-4o", "outcome": "bad_output",
				"response": `{"usage":{"prompt_tokens":100,"completion_tokens":5}}`,
			},
			{
				"provider": "openai", "model": "gpt-4o", "outcome": "bad_output",
				"response": `{"usage":{"prompt_tokens":100,"completion_tokens":7}}`,
			},
		},
	}
	if err := s.Write(e); err != nil {
		t.Fatalf("write: %v", err)
	}
	sums, err := s.TailSummaries(10)
	if err != nil {
		t.Fatalf("tail summaries: %v", err)
	}
	if len(sums) != 1 {
		t.Fatalf("want 1 summary, got %d", len(sums))
	}
	if sums[0].PromptTokens != 100 || sums[0].CompletionTokens != 12 {
		t.Fatalf("want prompt=100 completion=12, got %d/%d", sums[0].PromptTokens, sums[0].CompletionTokens)
	}
}

func TestGetMissingReturnsNil(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	writeEntries(t, s, 1)

	raw, err := s.Get("req-does-not-exist")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if raw != nil {
		t.Fatalf("want nil for missing id, got %s", raw)
	}
}

func contains(b []byte, sub string) bool {
	return indexOf(string(b), sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
