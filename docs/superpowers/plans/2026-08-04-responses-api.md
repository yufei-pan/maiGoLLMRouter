# Responses API Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add inbound `POST /v1/responses` so MaiBot `openai_responses` can use maiGoLLMRouter, with optional `supports_responses`, probe+cache, and strict Chat Completions translation fallback.

**Architecture:** New `OpResponses` operation. OpenAI-kind providers pass through to `/responses` (or probe then fall back). Non-OpenAI providers and Chat-only OpenAI use a Responses↔Chat codec when the request is portable; non-portable requests skip the target without blackout. Capability cache lives on the Router and clears on reload.

**Tech Stack:** Go 1.26+, standard library `net/http` + `encoding/json`, existing `github.com/BurntSushi/toml`, existing router/provider/server packages.

**Spec:** `docs/superpowers/specs/2026-08-04-responses-api-design.md`

## Global Constraints

- Do not break existing `/v1/chat/completions` or `/v1/embeddings` behavior or tests.
- No SSE/streaming — strip `stream` and `stream_options` on all outbound paths.
- Strict portability: never lossily translate native tools or non-portable `input` items.
- No new provider `kind`; use optional `supports_responses` on `[[provider]]`.
- Keep pass-through Responses JSON fidelity for MaiBot `output` item replay.
- TDD: write failing tests before implementation in each task; commit after each task.

## File Structure

| File | Responsibility |
| --- | --- |
| `config/config.go` | Parse optional `supports_responses *bool`; merge on same-name providers |
| `config/config_test.go` | Config parse/merge tests |
| `provider/provider.go` | `OpResponses`, `ResponsesMode`, `Request`/`Response` fields, `Call` dispatch |
| `provider/responses_codec.go` | Portability gate, Responses→Chat, Chat→Responses |
| `provider/responses_codec_test.go` | Codec + portability tests |
| `provider/responses_outcome.go` | Parse Responses status/output into `FinishReason`/`HasContent`; detect unsupported endpoint |
| `provider/responses_outcome_test.go` | Outcome + unsupported detection tests |
| `provider/openai.go` | OpResponses pass-through, probe, Chat fallback |
| `provider/openai_responses_test.go` | OpenAI dialect Responses tests |
| `provider/anthropic.go` / `gemini.go` | Unchanged chat translators; invoked after codec via `Call` |
| `router/capability.go` | In-memory Chat-only cache; clear on reload |
| `router/router.go` | Wire mode/cache, incompatible outcome, Responses `outputOK`, all-incompatible 400 |
| `router/router_test.go` | Integration tests for Responses routing |
| `server/server.go` | Register `/v1/responses` |
| `main.go` | Startup log mentions `/v1/responses` |
| `config.example.toml` / `README.md` | Document new field and endpoint |

---

### Task 1: Config `supports_responses`

**Files:**
- Modify: `config/config.go`
- Modify: `config/config_test.go`
- Modify: `config.example.toml` (comment only; fold docs into this task)

**Interfaces:**
- Consumes: existing `rawProvider` / `Provider` merge loop
- Produces: `Provider.SupportsResponses *bool` (`nil` = unset, non-nil true/false)

- [ ] **Step 1: Write the failing test**

Add to `config/config_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./config/ -run TestSupportsResponsesOptional -v`

Expected: FAIL (unknown field or `SupportsResponses` missing)

- [ ] **Step 3: Write minimal implementation**

In `config/config.go`:

1. Add to `Provider`:

```go
// SupportsResponses controls Responses API routing for kind=openai.
// nil = unset (probe then cache); non-nil true forces /responses;
// non-nil false forces Chat translation (never probe).
SupportsResponses *bool
```

2. Add to `rawProvider`:

```go
SupportsResponses *bool `toml:"supports_responses"`
```

3. In the provider merge loop, after scalar overwrites for kind/base_url/timeout:

```go
if rp.SupportsResponses != nil {
	v := *rp.SupportsResponses
	p.SupportsResponses = &v
}
```

4. In `config.example.toml`, under the provider field comments, add:

```toml
#  - supports_responses: optional. true = always POST /responses;
#    false = never probe, Chat-translate portable Responses inbound;
#    omit = probe /responses once, cache Chat-only until config reload.
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./config/ -run TestSupportsResponsesOptional -v`

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add config/config.go config/config_test.go config.example.toml
git commit -m "$(cat <<'EOF'
Add optional supports_responses provider config.

EOF
)"
```

---

### Task 2: Portability gate and Responses→Chat request codec

**Files:**
- Create: `provider/responses_codec.go`
- Create: `provider/responses_codec_test.go`
- Modify: `provider/provider.go` (add `OpResponses` constant only if needed for later tasks; prefer defining operation constants in Task 4 — this task stays codec-only)

**Interfaces:**
- Consumes: inbound Responses body `map[string]any`
- Produces:
  - `func PortableForChat(body map[string]any) error`
  - `func ResponsesToChat(body map[string]any, model string) (map[string]any, error)`

Portable rules (strict):
- `tools`: every entry must be a map with `type == "function"` (missing type treated as non-portable).
- `input`: if string, portable. If array, each item must be either:
  - role message: `role` in `system|user|assistant|developer` (developer → system), content string or array of parts with types `input_text`/`output_text`/`text` or `input_image`/`image_url`
  - `type == "function_call"` with call_id/name/arguments
  - `type == "function_call_output"` with call_id/output
- Reject items with `type` in `reasoning`, `web_search_call`, or any other non-listed type.
- Reject if `tools` contains non-function types even when `input` is portable.

- [ ] **Step 1: Write the failing tests**

Create `provider/responses_codec_test.go`:

```go
package provider

import (
	"encoding/json"
	"testing"
)

func TestPortableForChatAcceptsBasicInput(t *testing.T) {
	body := map[string]any{
		"model": "m",
		"input": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "hi"},
				map[string]any{"type": "input_image", "image_url": "https://example.com/a.png"},
			}},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "fn", "arguments": `{"a":1}`},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"},
		},
		"tools": []any{
			map[string]any{"type": "function", "name": "fn", "parameters": map[string]any{"type": "object"}},
		},
	}
	if err := PortableForChat(body); err != nil {
		t.Fatalf("portable: %v", err)
	}
}

func TestPortableForChatRejectsNativeTools(t *testing.T) {
	body := map[string]any{
		"input": "hi",
		"tools": []any{map[string]any{"type": "web_search"}},
	}
	if err := PortableForChat(body); err == nil {
		t.Fatal("expected non-portable")
	}
}

func TestPortableForChatRejectsReasoningItem(t *testing.T) {
	body := map[string]any{
		"input": []any{map[string]any{"type": "reasoning", "summary": []any{}}},
	}
	if err := PortableForChat(body); err == nil {
		t.Fatal("expected non-portable")
	}
}

func TestResponsesToChatMapsFields(t *testing.T) {
	body := map[string]any{
		"model":             "inbound",
		"max_output_tokens": float64(100),
		"temperature":       0.2,
		"text":              map[string]any{"format": map[string]any{"type": "json_object"}},
		"input": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		"tools": []any{
			map[string]any{
				"type": "function", "name": "fn", "description": "d",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
		"stream": true,
	}
	out, err := ResponsesToChat(body, "real-model")
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	if out["model"] != "real-model" {
		t.Fatalf("model=%v", out["model"])
	}
	if out["max_tokens"] != float64(100) {
		t.Fatalf("max_tokens=%v", out["max_tokens"])
	}
	rf, _ := out["response_format"].(map[string]any)
	if rf["type"] != "json_object" {
		t.Fatalf("response_format=%v", out["response_format"])
	}
	if _, ok := out["stream"]; ok {
		t.Fatal("stream must not be present")
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%v", out["messages"])
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", out["tools"])
	}
	raw, _ := json.Marshal(out["tools"])
	if !strings.Contains(string(raw), `"name":"fn"`) {
		t.Fatalf("chat tools shape unexpected: %s", raw)
	}
}
```

Ensure the test file imports `"strings"` alongside `"encoding/json"` and `"testing"`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./provider/ -run 'TestPortableForChat|TestResponsesToChat' -v`

Expected: FAIL (undefined PortableForChat / ResponsesToChat)

- [ ] **Step 3: Write minimal implementation**

Create `provider/responses_codec.go` implementing:

```go
package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

func PortableForChat(body map[string]any) error { /* strict checks */ }

func ResponsesToChat(body map[string]any, model string) (map[string]any, error) {
	if err := PortableForChat(body); err != nil {
		return nil, err
	}
	// Build messages from input; map tools to chat function tools;
	// max_output_tokens -> max_tokens; text.format -> response_format;
	// copy temperature and other extras except consumed keys;
	// never include stream / stream_options / input / text / max_output_tokens / previous_response_id.
	out := map[string]any{"model": model}
	// ...
	return out, nil
}
```

Mapping details:
- String `input` → one user message.
- Role messages: map content parts `input_text`/`output_text`/`text` → text; `input_image` with `image_url` string → `{"type":"image_url","image_url":{"url":...}}` (if image_url already object, pass through).
- `function_call` → assistant message with `tool_calls` entry (`id`=call_id, `type`=function, `function.name`, `function.arguments`).
- `function_call_output` → role `tool` message with `tool_call_id` and string content.
- Responses function tool `{type,name,description,parameters}` → Chat `{type:"function", function:{name,description,parameters}}`.
- Consumed keys for extras passthrough: `model`, `input`, `tools`, `text`, `max_output_tokens`, `max_tokens`, `stream`, `stream_options`, `previous_response_id`, `store` (drop store on Chat path).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./provider/ -run 'TestPortableForChat|TestResponsesToChat' -v`

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add provider/responses_codec.go provider/responses_codec_test.go
git commit -m "$(cat <<'EOF'
Add Responses-to-Chat portability gate and request codec.

EOF
)"
```

---

### Task 3: Chat→Responses response codec and outcome helpers

**Files:**
- Modify: `provider/responses_codec.go`
- Create: `provider/responses_outcome.go`
- Modify: `provider/responses_codec_test.go`
- Create: `provider/responses_outcome_test.go`

**Interfaces:**
- Consumes: Chat Completions JSON bytes; Responses JSON bytes
- Produces:
  - `func ChatToResponses(chatRaw []byte, model string) ([]byte, error)`
  - `func responsesOutcome(raw []byte) (finishReason string, hasContent bool)`
  - `func isResponsesUnsupported(status int, raw []byte) bool`

`ChatToResponses` synthetic shape:

```json
{
  "id": "resp_mai_<hex8>",
  "object": "response",
  "status": "completed",
  "model": "<model>",
  "output": [ /* message and/or function_call items */ ],
  "usage": {
    "input_tokens": <prompt_tokens>,
    "output_tokens": <completion_tokens>,
    "total_tokens": <total_tokens>
  }
}
```

- Message item: `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"..."}]}`
- Each Chat tool_call → `{"type":"function_call","call_id":"...","name":"...","arguments":"..."}`
- If Chat content empty but tool_calls present, omit message or include empty message — prefer omit empty message, only emit function_call items.
- Map usage from Chat `usage.prompt_tokens` / `completion_tokens` / `total_tokens`.

`responsesOutcome` (MaiBot-aligned):
- If `status` in `failed|incomplete|cancelled` → `("", false)`
- Scan `output`: message `output_text`/`refusal`, reasoning summary/`reasoning_text`, `function_call` → hasContent true
- finishReason: `tool_calls` if any function_call; else `stop` when hasContent; else `""`

`isResponsesUnsupported`:
- `status == 404` → true
- `status` in 400–499 and body message/type clearly indicates missing route (case-insensitive contains `/responses` and (`not found` or `unknown` or `no route` or `not supported` or `404`)) → true
- else false

- [ ] **Step 1: Write the failing tests**

```go
func TestChatToResponsesTextAndTools(t *testing.T) {
	chat := []byte(`{
	  "id":"chatcmpl-1",
	  "model":"m",
	  "choices":[{
	    "finish_reason":"tool_calls",
	    "message":{
	      "role":"assistant",
	      "content":"hello",
	      "tool_calls":[{
	        "id":"c1","type":"function",
	        "function":{"name":"fn","arguments":"{}"}
	      }]
	    }
	  }],
	  "usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}
	}`)
	out, err := ChatToResponses(chat, "real-model")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["object"] != "response" || parsed["status"] != "completed" {
		t.Fatalf("envelope=%v", parsed)
	}
	if parsed["model"] != "real-model" {
		t.Fatalf("model=%v", parsed["model"])
	}
	output, _ := parsed["output"].([]any)
	if len(output) < 2 {
		t.Fatalf("output=%v", parsed["output"])
	}
	usage, _ := parsed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(4) {
		t.Fatalf("usage=%v", usage)
	}
}

func TestResponsesOutcome(t *testing.T) {
	cases := []struct {
		name string
		body string
		fr   string
		hc   bool
	}{
		{"text", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`, "stop", true},
		{"tools", `{"status":"completed","output":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}]}`, "tool_calls", true},
		{"reasoning", `{"status":"completed","output":[{"type":"reasoning","summary":[{"text":"think"}]}]}`, "stop", true},
		{"failed", `{"status":"failed","output":[]}`, "", false},
		{"empty", `{"status":"completed","output":[]}`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fr, hc := responsesOutcome([]byte(c.body))
			if fr != c.fr || hc != c.hc {
				t.Fatalf("got (%q,%v) want (%q,%v)", fr, hc, c.fr, c.hc)
			}
		})
	}
}

func TestIsResponsesUnsupported(t *testing.T) {
	if !isResponsesUnsupported(404, []byte(`{}`)) {
		t.Fatal("404")
	}
	if !isResponsesUnsupported(400, []byte(`{"error":{"message":"Unknown path /responses"}}`)) {
		t.Fatal("explicit unknown path")
	}
	if isResponsesUnsupported(401, []byte(`{"error":{"message":"invalid api key"}}`)) {
		t.Fatal("auth must not count as unsupported")
	}
	if isResponsesUnsupported(400, []byte(`{"error":{"message":"missing required parameter: input"}}`)) {
		t.Fatal("valid Responses validation error must not count as unsupported")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./provider/ -run 'TestChatToResponses|TestResponsesOutcome|TestIsResponsesUnsupported' -v`

Expected: FAIL (undefined symbols)

- [ ] **Step 3: Write minimal implementation**

Implement `ChatToResponses` in `responses_codec.go` and the two helpers in `responses_outcome.go`. Generate id with `crypto/rand` + hex like other IDs in the repo (`server/util.go` pattern), prefixed `resp_mai_`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./provider/ -run 'TestChatToResponses|TestResponsesOutcome|TestIsResponsesUnsupported' -v`

Expected: PASS

Also run: `go test ./provider/ -count=1` to ensure no regressions.

- [ ] **Step 5: Commit**

```bash
git add provider/responses_codec.go provider/responses_codec_test.go provider/responses_outcome.go provider/responses_outcome_test.go
git commit -m "$(cat <<'EOF'
Add Chat-to-Responses codec and Responses outcome helpers.

EOF
)"
```

---

### Task 4: `OpResponses` in Call + OpenAI pass-through/probe/fallback

**Files:**
- Modify: `provider/provider.go`
- Modify: `provider/openai.go`
- Create: `provider/openai_responses_test.go`

**Interfaces:**
- Consumes: codec + outcome helpers from Tasks 2–3; `Request.ResponsesMode`
- Produces: working `Call(..., OpResponses)` for `kind=openai`

Add to `provider/provider.go`:

```go
const (
	OpChat Operation = iota
	OpEmbed
	OpResponses
)

type ResponsesMode int

const (
	ResponsesModeProbe ResponsesMode = iota // unset / default
	ResponsesModeForce
	ResponsesModeChatOnly
)

type Request struct {
	Op            Operation
	Model         string
	Body          map[string]any
	ResponsesMode ResponsesMode // meaningful for OpResponses + openai
}

type Response struct {
	HTTPStatus     int
	OpenAIBody     []byte // client-facing body (Responses JSON for OpResponses)
	FinishReason   string
	HasContent     bool
	Prohibited     bool
	OutboundURL    string
	OutboundBody   []byte
	RawResponse    []byte
	LearnChatOnly  bool // probe discovered /responses unsupported
	Incompatible   bool // cannot serve this OpResponses body on this dialect
}
```

Update `Call`:
- Keep Claude coercion for `OpChat` only (unchanged).
- For `OpResponses` + `kind == "openai"`: `callOpenAI(...)` handles path selection.
- For `OpResponses` + other kinds: handled in Task 5 (for now return error `unsupported` OR leave a stub — Task 5 must land before router tests use anthropic). In this task, non-openai OpResponses may return `Incompatible: true` temporarily if PortableForChat fails, else encode via chat dialect — prefer implementing the shared wrapper in Task 5; this task only completes openai.

`callOpenAI` for `OpResponses`:

```text
strip stream fields from a copy of body; set model
switch mode:
  Force: POST /responses; on OK set outcome via responsesOutcome; OpenAIBody=raw
         on not-supported: treat as provider error (NO chat fallback, LearnChatOnly=false)
  ChatOnly: PortableForChat; if err → Incompatible=true, HTTPStatus=0, nil error
            else ResponsesToChat → POST /chat/completions → ChatToResponses → set OpenAIBody
            apply withClaudeTrailingUserCoercion on the Chat Request before POST
  Probe: POST /responses
         if OK → same as Force success
         if isResponsesUnsupported → LearnChatOnly=true, then same as ChatOnly path (same attempt)
         else → normal provider error with raw body
```

Always set `OutboundURL`/`OutboundBody`/`RawResponse` to the **last** downstream call (Chat if fallback ran).

- [ ] **Step 1: Write the failing tests**

Create `provider/openai_responses_test.go`:

```go
func TestOpenAIResponsesPassThrough(t *testing.T) {
	var path string
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"id":"r1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", Request{
		Op: OpResponses, Model: "m", ResponsesMode: ResponsesModeForce,
		Body: map[string]any{"model": "in", "input": "hi", "stream": true},
	})
	if err != nil || !resp.OK() || !resp.HasContent {
		t.Fatalf("err=%v resp=%+v", err, resp)
	}
	if path != "/responses" {
		t.Fatalf("path=%s", path)
	}
	if _, ok := outbound["stream"]; ok {
		t.Fatal("stream should be stripped")
	}
}

func TestOpenAIResponsesProbeFallsBackToChat(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/responses" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"message":"not found"}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", Request{
		Op: OpResponses, Model: "m", ResponsesMode: ResponsesModeProbe,
		Body: map[string]any{"model": "in", "input": "hi"},
	})
	if err != nil || !resp.OK() || !resp.HasContent || !resp.LearnChatOnly {
		t.Fatalf("err=%v resp=%+v body=%s", err, resp, resp.OpenAIBody)
	}
	if len(paths) != 2 || paths[0] != "/responses" || paths[1] != "/chat/completions" {
		t.Fatalf("paths=%v", paths)
	}
	var parsed map[string]any
	_ = json.Unmarshal(resp.OpenAIBody, &parsed)
	if parsed["object"] != "response" {
		t.Fatalf("client body not Responses-shaped: %s", resp.OpenAIBody)
	}
}

func TestOpenAIResponsesForceNoChatFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"error":"nope"}`)
	}))
	defer srv.Close()
	resp, err := Call(context.Background(), srv.Client(), "openai", srv.URL, "k", Request{
		Op: OpResponses, Model: "m", ResponsesMode: ResponsesModeForce,
		Body: map[string]any{"input": "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK() || resp.LearnChatOnly {
		t.Fatalf("force mode must not fall back: %+v", resp)
	}
}

func TestOpenAIResponsesChatOnlyNonPortable(t *testing.T) {
	resp, err := Call(context.Background(), http.DefaultClient, "openai", "http://127.0.0.1:0", "k", Request{
		Op: OpResponses, Model: "m", ResponsesMode: ResponsesModeChatOnly,
		Body: map[string]any{"input": "hi", "tools": []any{map[string]any{"type": "web_search"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Incompatible {
		t.Fatalf("want incompatible, got %+v", resp)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./provider/ -run 'TestOpenAIResponses' -v`

Expected: FAIL

- [ ] **Step 3: Write minimal implementation**

Implement fields + `callOpenAI` OpResponses branches as specified. Refactor shared “copy body, strip stream, set model, POST” helpers if useful without large drive-by cleanup.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./provider/ -count=1`

Expected: PASS (all provider tests)

- [ ] **Step 5: Commit**

```bash
git add provider/provider.go provider/openai.go provider/openai_responses_test.go
git commit -m "$(cat <<'EOF'
Add OpResponses OpenAI pass-through with probe and Chat fallback.

EOF
)"
```

---

### Task 5: Anthropic/Gemini OpResponses via codec

**Files:**
- Modify: `provider/provider.go` (`Call` switch)
- Create: `provider/responses_dialect_test.go`

**Interfaces:**
- Consumes: `PortableForChat`, `ResponsesToChat`, `ChatToResponses`; existing `callAnthropic` / `callGemini`
- Produces: `Call` with `OpResponses` for `anthropic` and `gemini`

In `Call`, before/inside switch:

```go
if req.Op == OpResponses && kind != "openai" {
	return callResponsesViaChat(ctx, client, kind, baseURL, apiKey, req)
}
```

```go
func callResponsesViaChat(ctx context.Context, client *http.Client, kind, baseURL, apiKey string, req Request) (*Response, error) {
	if err := PortableForChat(req.Body); err != nil {
		return &Response{Incompatible: true}, nil
	}
	chatBody, err := ResponsesToChat(req.Body, req.Model)
	if err != nil {
		return &Response{Incompatible: true}, nil
	}
	chatReq := Request{Op: OpChat, Model: req.Model, Body: chatBody}
	chatReq = withClaudeTrailingUserCoercion(chatReq)
	var resp *Response
	switch kind {
	case "anthropic":
		resp, err = callAnthropic(ctx, client, baseURL, apiKey, chatReq)
	case "gemini":
		resp, err = callGemini(ctx, client, baseURL, apiKey, chatReq)
	default:
		return nil, fmt.Errorf("unknown provider kind %q", kind)
	}
	if err != nil || resp == nil || !resp.OK() {
		return resp, err
	}
	wrapped, werr := ChatToResponses(resp.OpenAIBody, req.Model)
	if werr != nil {
		resp.HasContent = false
		return resp, nil
	}
	resp.OpenAIBody = wrapped
	resp.FinishReason, resp.HasContent = responsesOutcome(wrapped)
	return resp, nil
}
```

Keep prohibited-content check in `Call` after dialect returns.

- [ ] **Step 1: Write the failing tests**

Create `provider/responses_dialect_test.go`:

```go
func TestResponsesViaAnthropicPortable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		fmt.Fprint(w, `{
		  "id":"msg_1","type":"message","role":"assistant","model":"claude",
		  "content":[{"type":"text","text":"hi"}],
		  "stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}
		}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "anthropic", srv.URL, "k", Request{
		Op: OpResponses, Model: "claude-3-5-sonnet",
		Body: map[string]any{"model": "in", "input": "hello"},
	})
	if err != nil || !resp.OK() || !resp.HasContent {
		t.Fatalf("err=%v resp=%+v body=%s", err, resp, resp.OpenAIBody)
	}
	var parsed map[string]any
	if err := json.Unmarshal(resp.OpenAIBody, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["object"] != "response" || parsed["status"] != "completed" {
		t.Fatalf("body=%s", resp.OpenAIBody)
	}
}

func TestResponsesViaAnthropicNonPortable(t *testing.T) {
	resp, err := Call(context.Background(), http.DefaultClient, "anthropic", "http://127.0.0.1:0", "k", Request{
		Op: OpResponses, Model: "claude",
		Body: map[string]any{
			"input": "hi",
			"tools": []any{map[string]any{"type": "web_search"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Incompatible {
		t.Fatalf("want incompatible, got %+v", resp)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./provider/ -run 'TestResponsesViaAnthropic' -v`

Expected: FAIL

- [ ] **Step 3: Implement `callResponsesViaChat` and wire `Call`**

- [ ] **Step 4: Run all provider tests**

Run: `go test ./provider/ -count=1`

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add provider/provider.go provider/responses_dialect_test.go
git commit -m "$(cat <<'EOF'
Route OpResponses through Anthropic/Gemini via Chat codec.

EOF
)"
```

---

### Task 6: Capability cache + router wiring

**Files:**
- Create: `router/capability.go`
- Modify: `router/router.go`
- Modify: `router/router_test.go`

**Interfaces:**
- Consumes: `config.Provider.SupportsResponses`, `provider.ResponsesMode*`, `Response.LearnChatOnly`, `Response.Incompatible`
- Produces: end-to-end Responses routing with cache clear on `Reload`

```go
// router/capability.go
type responsesCapability struct {
	mu       sync.Mutex
	chatOnly map[string]bool
}

func newResponsesCapability() *responsesCapability {
	return &responsesCapability{chatOnly: make(map[string]bool)}
}

func (c *responsesCapability) IsChatOnly(provider string) bool { /* lock */ }
func (c *responsesCapability) MarkChatOnly(provider string) { /* lock */ }
func (c *responsesCapability) Clear() { /* reset map */ }
```

Add field on `Router`: `respCaps *responsesCapability` — init in `New`, `Clear()` in `Reload`.

New outcome: `outcomeIncompatible`.

In `callOnce`, build mode:

```go
mode := provider.ResponsesModeProbe
if op == provider.OpResponses {
	if p.SupportsResponses != nil {
		if *p.SupportsResponses {
			mode = provider.ResponsesModeForce
		} else {
			mode = provider.ResponsesModeChatOnly
		}
	} else if r.respCaps.IsChatOnly(p.Name) {
		mode = provider.ResponsesModeChatOnly
	}
}
resp, err := provider.Call(..., provider.Request{Op: op, Model: model, Body: body, ResponsesMode: mode})
if resp != nil && resp.LearnChatOnly {
	r.respCaps.MarkChatOnly(p.Name)
}
if resp != nil && resp.Incompatible {
	att.Outcome = "incompatible"
	att.Error = "responses request not portable to this provider"
	return resp, att, outcomeIncompatible
}
```

In `tryKey`, `outcomeIncompatible`: return `done=false, skip=false` (advance to next key/target) **without blackout**.

In `Execute`, after exhaustion:
- If there was at least one attempt and every non-`skipped_blackout` attempt is `incompatible` (and there was ≥1 incompatible) → `Status=400`, body explaining Responses-only features require a Responses-capable provider.
- Else keep existing exhausted behavior.

`outputOK`:

```go
func (r *Router) outputOK(op provider.Operation, resp *provider.Response) bool {
	if op == provider.OpEmbed {
		return resp.HasContent
	}
	if op == provider.OpResponses {
		// FinishReason already normalized by responsesOutcome / Chat path.
		if !resp.HasContent {
			return false
		}
		if resp.FinishReason == "" {
			return false
		}
		return r.cfg.Server.GoodFinishReasons[strings.ToLower(resp.FinishReason)]
	}
	// existing chat logic
}
```

- [ ] **Step 1: Write failing router tests**

Add cases:
1. Probe 404 then Chat success; second request to same router hits Chat only (one path `/chat/completions`) — proves cache.
2. `supports_responses=false` + native tool body → all incompatible → status 400.
3. Reload clears cache (after MarkChatOnly, Reload, next request probes `/responses` again).
4. Existing `TestExecuteSuccessNormalKey` still passes (OpChat).

- [ ] **Step 2: Run tests to verify new ones fail**

Run: `go test ./router/ -run 'TestResponses' -v`

Expected: FAIL

- [ ] **Step 3: Implement capability cache + router changes**

- [ ] **Step 4: Run full router + provider tests**

Run: `go test ./router/ ./provider/ ./config/ -count=1`

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add router/capability.go router/router.go router/router_test.go
git commit -m "$(cat <<'EOF'
Wire Responses capability cache and incompatible routing outcomes.

EOF
)"
```

---

### Task 7: Server route, startup log, README

**Files:**
- Modify: `server/server.go`
- Modify: `main.go`
- Modify: `README.md`
- Optional smoke: small server test if one exists; otherwise router-level coverage is enough — add a focused server test registering mux and POSTing `/v1/responses` with auth if easy.

**Interfaces:**
- Consumes: `provider.OpResponses`, existing `handle`
- Produces: public `POST /v1/responses`

- [ ] **Step 1: Write failing server smoke test** (add `server/server_test.go` if missing)

```go
func TestResponsesRouteRegistered(t *testing.T) {
	// Build minimal cfg + router + server, Register on mux,
	// POST /v1/responses with bearer + {"model":"m","input":"hi"}
	// against httptest backend; expect 200 Responses JSON.
}
```

Follow existing test helpers from `router_test.go` for config loading if server tests are thin — constructing `server.New` + `router.New` is fine.

- [ ] **Step 2: Run test to verify fail/missing route**

Run: `go test ./server/ -run TestResponsesRouteRegistered -v`

- [ ] **Step 3: Implement**

In `server/server.go` `Register`:

```go
mux.HandleFunc("/v1/responses", s.handle(provider.OpResponses))
```

In `handle`, set endpoint:

```go
endpoint := "/v1/chat/completions"
switch op {
case provider.OpEmbed:
	endpoint = "/v1/embeddings"
case provider.OpResponses:
	endpoint = "/v1/responses"
}
```

Update `main.go` startup line to include `POST /v1/responses`.

Update README features + example curl for `/v1/responses` + document `supports_responses`.

- [ ] **Step 4: Run full test suite**

Run: `go test ./... -count=1`

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add server/server.go server/server_test.go main.go README.md config.example.toml
git commit -m "$(cat <<'EOF'
Expose POST /v1/responses and document Responses API support.

EOF
)"
```

---

## Self-Review (plan vs spec)

| Spec requirement | Task |
| --- | --- |
| Inbound `POST /v1/responses` | Task 7 |
| OpenAI pass-through `/responses` | Task 4 |
| Optional `supports_responses` | Task 1 |
| Probe when unset + same-attempt Chat fallback | Task 4 |
| Cache Chat-only until reload | Task 6 |
| Force mode no Chat fallback | Task 4 |
| Strict portability / incompatible skip | Tasks 2, 4–6 |
| Anthropic/Gemini via codec | Task 5 |
| No streaming | Tasks 4, 2 (strip) |
| Output verification MaiBot-aligned | Tasks 3, 6 |
| All-incompatible → 400 | Task 6 |
| Chat Completions unchanged | Tasks 4–7 (regression runs) |
| Docs / example config | Tasks 1, 7 |
| Out of scope streaming/store/native synth | Not planned |

No intentional placeholders remain. Task 5 includes a concrete Anthropic `/messages` httptest stub.
