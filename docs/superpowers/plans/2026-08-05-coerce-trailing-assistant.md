# Coerce Trailing Assistant (Ingest) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** At request ingest, optionally rewrite a trailing non-user/tool/function turn to `user` for all models, flip first-person pronouns in that turn, and remove the Claude-only outbound coercion path.

**Architecture:** A pure `provider.CoerceTrailingAssistantTurn(body, field)` helper mutates a copy of the last turn in `messages` or `input`. `server.handle` calls it once after JSON unmarshal when `config.Server.CoerceTrailingAssistant` is true (default). `provider.Call` no longer performs this rewrite. Detail logs keep the original request bytes.

**Tech Stack:** Go, BurntSushi/toml, net/http, existing `provider` / `config` / `server` packages.

**Spec:** `docs/superpowers/specs/2026-08-05-coerce-trailing-assistant-design.md`

## Global Constraints

- Toggle TOML key: `coerce_trailing_assistant` under `[server]`; omitted ⇒ enabled (`true`).
- Apply for all models (no Claude name gating).
- Pronoun rewrite only when the role was actually flipped.
- English word-boundary, case-insensitive: `I`/`me`→`you`, `my`→`your`, `mine`→`yours` (replacement text lowercase).
- Chinese: replace `我`→`你` (character replace).
- Do not rewrite trailing `user` / `tool` / `function` roles or typed Responses items without a role.
- Do not re-marshal coerced bodies into detail logs (keep original `raw`).
- Copy-on-write: never mutate the shared last-message map in place.

---

## File Structure

| File | Responsibility |
|---|---|
| `provider/coerce_trailing.go` | Exported ingest helper + pronoun rewrite (new) |
| `provider/coerce_trailing_test.go` | Unit tests for the helper (new) |
| `provider/provider.go` | Remove Claude coercion helpers and Call-site invocations |
| `provider/openai.go` | Remove Claude coercion after Responses→Chat |
| `provider/openai_claude_test.go` | Delete Claude-name / Call-coercion tests (or gut file) |
| `provider/openai_responses_test.go` | Drop assertion that Call coerces Claude trailing assistant |
| `config/config.go` | `CoerceTrailingAssistant` field + `*bool` TOML parse |
| `config/config_test.go` | Default / explicit true / false tests |
| `server/server.go` | Call helper at ingest for Chat/Responses |
| `server/server_test.go` | Enabled/disabled ingest behavior via outbound capture |
| `config.example.toml` | Document the knob |
| `main.go` | Mention in embedded config help if server knobs are listed |

---

### Task 1: Pure coerce helper + unit tests

**Files:**
- Create: `provider/coerce_trailing.go`
- Create: `provider/coerce_trailing_test.go`
- Modify: none yet (Claude removal is Task 3)

**Interfaces:**
- Consumes: existing `asString` in `provider/provider.go`
- Produces:
  - `func CoerceTrailingAssistantTurn(body map[string]any, field string) map[string]any` — returns `body` unchanged when no-op; otherwise a shallow-copied body with a copied items slice and copied last map. `field` is `"messages"` or `"input"`.

- [ ] **Step 1: Write the failing tests**

Create `provider/coerce_trailing_test.go`:

```go
package provider

import (
	"reflect"
	"testing"
)

func TestCoerceTrailingAssistantTurnFlipsRoleAndPronouns(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "I think my mine 我 我的"},
		},
	}
	out := CoerceTrailingAssistantTurn(body, "messages")
	msgs := out["messages"].([]any)
	last := msgs[1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("role=%v", last["role"])
	}
	if last["content"] != "you think your yours 你 你的" {
		t.Fatalf("content=%q", last["content"])
	}
	// Original not mutated.
	orig := body["messages"].([]any)[1].(map[string]any)
	if orig["role"] != "assistant" || orig["content"] != "I think my mine 我 我的" {
		t.Fatalf("inbound mutated: %#v", orig)
	}
}

func TestCoerceTrailingAssistantTurnLeavesUserToolTyped(t *testing.T) {
	cases := []struct {
		name  string
		field string
		items []any
	}{
		{"user", "messages", []any{map[string]any{"role": "user", "content": "I"}}},
		{"tool", "messages", []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "I"},
		}},
		{"typed", "input", []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "f", "arguments": "{}"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]any{c.field: c.items}
			out := CoerceTrailingAssistantTurn(body, c.field)
			if !reflect.DeepEqual(out[c.field], c.items) {
				t.Fatalf("changed: %#v", out[c.field])
			}
			// Pronouns must not be rewritten when role was not flipped.
			last := c.items[len(c.items)-1].(map[string]any)
			if s, ok := last["content"].(string); ok && s != "I" && c.name != "typed" {
				// tool/user keep "I"
			}
			if c.name != "typed" {
				if last["content"] != "I" {
					t.Fatalf("pronoun rewritten without role flip: %#v", last)
				}
			}
		})
	}
}

func TestCoerceTrailingAssistantTurnMultimodal(t *testing.T) {
	body := map[string]any{
		"input": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "I see 我"},
				map[string]any{"type": "input_image", "image_url": "http://x"},
			}},
		},
	}
	out := CoerceTrailingAssistantTurn(body, "input")
	last := out["input"].([]any)[1].(map[string]any)
	parts := last["content"].([]any)
	textPart := parts[0].(map[string]any)
	imgPart := parts[1].(map[string]any)
	if last["role"] != "user" || textPart["text"] != "you see 你" {
		t.Fatalf("text part=%#v role=%v", textPart, last["role"])
	}
	if imgPart["image_url"] != "http://x" {
		t.Fatalf("image mutated: %#v", imgPart)
	}
}

func TestCoerceTrailingAssistantTurnEmptyNoop(t *testing.T) {
	body := map[string]any{"messages": []any{}}
	if CoerceTrailingAssistantTurn(body, "messages")["messages"] == nil {
		t.Fatal("unexpected nil")
	}
	out := CoerceTrailingAssistantTurn(map[string]any{}, "messages")
	if len(out) != 0 {
		t.Fatalf("empty body changed: %#v", out)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./provider -run TestCoerceTrailingAssistantTurn -count=1`

Expected: FAIL with `undefined: CoerceTrailingAssistantTurn`

- [ ] **Step 3: Implement the helper**

Create `provider/coerce_trailing.go`:

```go
package provider

import (
	"regexp"
	"strings"
)

// englishPronounRE matches whole-word I/me/my/mine (case-insensitive).
// Longer tokens (mine) are listed before shorter ones that share a prefix (my)
// is unnecessary with word boundaries; order still keeps replacements clear.
var englishPronounRE = regexp.MustCompile(`(?i)\b(mine|my|me|i)\b`)

// CoerceTrailingAssistantTurn forces the last role-bearing item in body[field]
// to role "user" when it is not already user/tool/function, and rewrites
// first-person pronouns in that turn's text. field is "messages" or "input".
// Returns body unchanged when there is nothing to do. Never mutates the
// shared last-item map in place.
func CoerceTrailingAssistantTurn(body map[string]any, field string) map[string]any {
	if body == nil {
		return body
	}
	items, ok := body[field].([]any)
	if !ok || len(items) == 0 {
		return body
	}
	last, ok := items[len(items)-1].(map[string]any)
	if !ok {
		return body
	}
	role := asString(last, "role")
	if role == "" || role == "user" || role == "tool" || role == "function" {
		return body
	}

	copiedLast := make(map[string]any, len(last)+1)
	for k, v := range last {
		copiedLast[k] = v
	}
	copiedLast["role"] = "user"
	if c, exists := copiedLast["content"]; exists {
		copiedLast["content"] = rewritePronounContent(c)
	}

	copiedItems := append([]any(nil), items...)
	copiedItems[len(copiedItems)-1] = copiedLast

	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	out[field] = copiedItems
	return out
}

func rewritePronounContent(content any) any {
	switch c := content.(type) {
	case string:
		return rewritePronounText(c)
	case []any:
		out := make([]any, len(c))
		for i, part := range c {
			m, ok := part.(map[string]any)
			if !ok {
				out[i] = part
				continue
			}
			copied := make(map[string]any, len(m))
			for k, v := range m {
				copied[k] = v
			}
			for _, key := range []string{"text", "input_text", "output_text"} {
				if s, ok := copied[key].(string); ok {
					copied[key] = rewritePronounText(s)
				}
			}
			out[i] = copied
		}
		return out
	default:
		return content
	}
}

func rewritePronounText(s string) string {
	s = englishPronounRE.ReplaceAllStringFunc(s, func(m string) string {
		switch strings.ToLower(m) {
		case "i", "me":
			return "you"
		case "my":
			return "your"
		case "mine":
			return "yours"
		default:
			return m
		}
	})
	return strings.ReplaceAll(s, "我", "你")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./provider -run TestCoerceTrailingAssistantTurn -count=1`

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add provider/coerce_trailing.go provider/coerce_trailing_test.go
git commit -m "$(cat <<'EOF'
Add ingest helper to coerce trailing assistant turns.

EOF
)"
```

---

### Task 2: Config toggle (default on)

**Files:**
- Modify: `config/config.go`
- Modify: `config/config_test.go`
- Modify: `config.example.toml`

**Interfaces:**
- Consumes: none from Task 1
- Produces: `config.Server.CoerceTrailingAssistant bool` (true when TOML key omitted)

- [ ] **Step 1: Write the failing config tests**

Append to `config/config_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./config -run TestCoerceTrailingAssistantDefaultAndExplicit -count=1`

Expected: FAIL (field missing / always false)

- [ ] **Step 3: Implement config field**

In `config/config.go`:

1. Add to `Server`:

```go
	// CoerceTrailingAssistant rewrites a trailing non-user/tool/function turn
	// to role "user" (and flips pronouns in that turn) at request ingest.
	// Default true when the TOML key is omitted.
	CoerceTrailingAssistant bool
```

2. Add to `rawServer`:

```go
	CoerceTrailingAssistant *bool `toml:"coerce_trailing_assistant"`
```

3. In `build`, after other server defaults (near `ConfigReloadInterval` handling):

```go
	srv.CoerceTrailingAssistant = true
	if raw.Server.CoerceTrailingAssistant != nil {
		srv.CoerceTrailingAssistant = *raw.Server.CoerceTrailingAssistant
	}
```

4. In `config.example.toml`, under `[server]` after `good_finish_reasons` (or near other boolean knobs), add:

```toml
# When the last chat/responses turn is assistant (etc.), rewrite it to user and
# flip first-person pronouns in that turn. Applies to all models. Default true.
# coerce_trailing_assistant = true
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./config -run TestCoerceTrailingAssistantDefaultAndExplicit -count=1`

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add config/config.go config/config_test.go config.example.toml
git commit -m "$(cat <<'EOF'
Add coerce_trailing_assistant server config (default on).

EOF
)"
```

---

### Task 3: Ingest hook + remove outbound Claude coercion

**Files:**
- Modify: `server/server.go`
- Modify: `server/server_test.go`
- Modify: `provider/provider.go` (remove Claude helpers + Call invocations)
- Modify: `provider/openai.go` (remove post-translate coercion)
- Modify: `provider/openai_claude_test.go` (delete or replace with a smoke that Call does not coerce)
- Modify: `provider/openai_responses_test.go` (remove Claude coercion assertion in chat-only path)

**Interfaces:**
- Consumes: `provider.CoerceTrailingAssistantTurn`, `config.Server.CoerceTrailingAssistant`
- Produces: Chat/Responses handlers mutate parsed `body` before `router.Execute` when enabled

- [ ] **Step 1: Write failing server ingest tests**

Append to `server/server_test.go`:

```go
func TestIngestCoercesTrailingAssistantWhenEnabled(t *testing.T) {
	var outbound map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer backend.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[server]
client_keys = ["sk-test"]
# coerce_trailing_assistant omitted => true

[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["sk-upstream"]

[model."m"]
targets = ["openai/gpt-4o"]
`, backend.URL))

	rt := router.New(cfg)
	srv := New(cfg, rt, nil, nil)
	mux := http.NewServeMux()
	srv.Register(mux)

	reqBody := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"I 我"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.Bytes())
	}
	last := outbound["messages"].([]any)[1].(map[string]any)
	if last["role"] != "user" || last["content"] != "you 你" {
		t.Fatalf("outbound last=%#v", last)
	}
}

func TestIngestSkipsCoercionWhenDisabled(t *testing.T) {
	var outbound map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer backend.Close()

	cfg := loadCfg(t, fmt.Sprintf(`
[server]
client_keys = ["sk-test"]
coerce_trailing_assistant = false

[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["sk-upstream"]

[model."m"]
targets = ["openai/gpt-4o"]
`, backend.URL))

	rt := router.New(cfg)
	srv := New(cfg, rt, nil, nil)
	mux := http.NewServeMux()
	srv.Register(mux)

	reqBody := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"I"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	last := outbound["messages"].([]any)[1].(map[string]any)
	if last["role"] != "assistant" || last["content"] != "I" {
		t.Fatalf("outbound last=%#v", last)
	}
}
```

Also rewrite `provider/openai_claude_test.go` expectations that Call coerces: replace Claude Call-coercion tests with one that proves Call leaves trailing assistant alone for a Claude-named model (ingest owns the feature). Keep or delete `TestIsClaudeModelName` only if `isClaudeModelName` still exists after Step 3 — it should be deleted, so remove that test file’s Claude-name tests entirely.

Replace the file content of `provider/openai_claude_test.go` with:

```go
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCallDoesNotCoerceTrailingAssistant(t *testing.T) {
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	req := Request{
		Op:    OpChat,
		Model: "claude-3-5-sonnet",
		Body: map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "content": "hi"},
				map[string]any{"role": "assistant", "content": "prefill"},
			},
		},
	}
	if _, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "key", req); err != nil {
		t.Fatalf("Call error: %v", err)
	}
	last := outbound["messages"].([]any)[1].(map[string]any)
	if last["role"] != "assistant" {
		t.Fatalf("Call still coerced trailing role to %v", last["role"])
	}
}
```

In `provider/openai_responses_test.go`, in `TestOpenAIResponsesChatOnlyPortable` (or whatever test currently asserts `last["role"] != "user"` / `"claude coercion not applied"`), change the assertion so trailing assistant stays `assistant` when Call is invoked without prior ingest coerce — remove the block:

```go
	if last["role"] != "user" {
		t.Fatalf("claude coercion not applied to the chat request: %v", last)
	}
```

Replace with:

```go
	if last["role"] != "assistant" {
		t.Fatalf("Call should not coerce trailing assistant anymore: %v", last)
	}
```

- [ ] **Step 2: Run the new server tests — expect fail (no ingest hook yet)**

Run: `go test ./server -run 'TestIngestCoerces|TestIngestSkips' -count=1`

Expected: `TestIngestCoercesTrailingAssistantWhenEnabled` FAIL (role still assistant). `TestIngestSkips` may PASS already.

- [ ] **Step 3: Wire ingest + remove outbound Claude path**

In `server/server.go`, after successful JSON unmarshal and model check, before inflight registration / `Execute`:

```go
		s.mu.RLock()
		coerce := s.cfg.Server.CoerceTrailingAssistant
		s.mu.RUnlock()
		if coerce {
			switch op {
			case provider.OpChat:
				body = provider.CoerceTrailingAssistantTurn(body, "messages")
			case provider.OpResponses:
				body = provider.CoerceTrailingAssistantTurn(body, "input")
			}
		}
```

In `provider/provider.go`:

1. Delete `req = withClaudeTrailingUserCoercion(req)` from `Call`.
2. Delete `chatReq = withClaudeTrailingUserCoercion(chatReq)` from `callResponsesViaChat`.
3. Delete `isClaudeModelName`, `resolvedModelLeaf` (if unused), `withClaudeTrailingUserCoercion`, and `coerceTrailingRole`.

In `provider/openai.go` `openAIResponsesViaChat`, change:

```go
	chatReq := withClaudeTrailingUserCoercion(Request{Op: OpChat, Model: req.Model, Body: chatBody})
```

to:

```go
	chatReq := Request{Op: OpChat, Model: req.Model, Body: chatBody}
```

- [ ] **Step 4: Run focused tests**

Run:

```bash
go test ./provider ./config ./server -count=1
```

Expected: PASS

If any remaining test still expects Call-time Claude coercion, update it to match the new ownership (ingest-only).

- [ ] **Step 5: Commit**

```bash
git add server/server.go server/server_test.go \
  provider/provider.go provider/openai.go \
  provider/openai_claude_test.go provider/openai_responses_test.go
git commit -m "$(cat <<'EOF'
Apply trailing-assistant coercion at ingest for all models.

EOF
)"
```

---

### Task 4: Full verification

**Files:**
- Modify only if tests fail

- [ ] **Step 1: Run the full test suite**

Run: `go test ./... -count=1`

Expected: all packages PASS (ignore `MaiBot-src` if it is not part of the Go module).

If `./...` picks up unwanted trees, run:

```bash
go test ./config ./provider ./router ./server ./logstore ./inflight -count=1
```

Expected: PASS

- [ ] **Step 2: Commit only if Step 1 required fixes**

```bash
git add -u
git commit -m "$(cat <<'EOF'
Fix leftover coercion tests after ingest move.

EOF
)"
```

Skip this commit if nothing changed.

---

## Self-Review (plan vs spec)

| Spec requirement | Task |
|---|---|
| All-models coerce | Task 3 ingest (no model gate) |
| Toggle, default on | Task 2 |
| Ingest, not Call | Task 3 |
| Pronoun rewrite on flip only | Task 1 |
| Keep original `raw` logs | Task 3 (handler still logs `raw`) |
| Skip tool/function/typed | Task 1 |
| Multimodal text parts | Task 1 |
| Remove Claude Call path | Task 3 |
| Example config docs | Task 2 |
| Config + helper + server tests | Tasks 1–3 |

No placeholders left. Interface name `CoerceTrailingAssistantTurn(body, field string) map[string]any` is consistent across tasks.
