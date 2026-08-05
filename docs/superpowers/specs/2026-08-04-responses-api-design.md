# OpenAI Responses API Support

**Date:** 2026-08-04  
**Status:** Approved for implementation planning  
**Goal:** Make maiGoLLMRouter a drop-in Responses API backend for MaiBot (`client_type = openai_responses`) while keeping existing Chat Completions behavior unchanged.

## Background

MaiBot 1.1.4+ supports the OpenAI Responses API via `openai_responses` (`POST {base_url}/responses`). Features in use:

- Text and image input
- Structured output (`text.format`)
- Function tools and native tools (e.g. `web_search`)
- Reasoning output items
- Usage accounting (`input_tokens` / `output_tokens`)
- Optional streaming (MaiBot `force_stream_mode`)
- `store=false` with local `output` item replay for conversation continuity (no `previous_response_id`)

maiGoLLMRouter today exposes only `POST /v1/chat/completions` and `POST /v1/embeddings`, with OpenAI pass-through plus Anthropic/Gemini dialect translation. Streaming is stripped; responses are buffered and verified.

## Decisions (locked)

| Topic | Choice |
| --- | --- |
| Backend strategy | Pass-through when Responses is supported; otherwise translate Responses ↔ Chat Completions |
| Capability detection | Optional `supports_responses` on `[[provider]]`; when unset, probe `/responses` then fall back |
| Probe miss cache | Remember “no Responses” per provider until config reload |
| Streaming | Not in scope — strip `stream` / `stream_options`; return complete JSON only |
| Non-portable features | Strict — do not lossily translate native tools or non-portable `input` items; skip target or error |
| Approach | Explicit capability override + probe when unset (Approach 2) |

## Architecture

Inbound remains OpenAI-shaped. Add `POST /v1/responses` beside existing chat and embeddings endpoints. Auth, model routing, key rotation, blackout, and fallback are shared.

```text
inbound /v1/responses
  └─ resolve targets (existing model map)
       └─ for each provider/key attempt:
            ├─ anthropic | gemini
            │    └─ if request portable → Responses↔Chat codec → existing dialect
            │       else → skip (incompatible), no blackout
            └─ openai kind
                 ├─ supports_responses=true  → POST /responses (pass-through)
                 ├─ supports_responses=false → Chat translate if portable else skip
                 └─ unset
                      ├─ cached "no Responses" → Chat translate path
                      └─ else try POST /responses
                           ├─ success → done
                           └─ not-supported → cache "no Responses", same attempt → Chat translate
```

Inbound `POST /v1/chat/completions` is unchanged.

## Components

### Server

- Register `POST /v1/responses` mapped to new `provider.OpResponses`.
- Reuse the existing handler shell (auth, JSON body parse, inflight registry, JSONL logging).
- Endpoint string for logs: `/v1/responses`.

### Provider package

- Extend `Operation` with `OpResponses`.
- **Pass-through (openai kind):** forward body to `{base_url}/responses` with model override; strip stream fields; return raw Responses JSON to the client (do not Chat-wrap).
- **Responses↔Chat codec:**
  - Request: portable `input` items → Chat `messages`; function `tools` only; `max_output_tokens` → `max_tokens`; `text.format` → `response_format`; pass temperature and other portable extras.
  - Response: Chat `choices[0]` → synthetic Responses object with `id`, `object`, `status=completed`, `model`, `output[]` (`message` and/or `function_call` items), and usage mapped to `input_tokens` / `output_tokens` / `total_tokens`.
- **Portability gate (strict):** translation is refused when the request includes:
  - Tools with `type` other than `function` (e.g. `web_search`)
  - `input` items that Chat cannot represent (`reasoning`, `web_search_call`, other native tool items, and similarly non-portable types)
  - Portable items are role-based message shapes plus `function_call` / `function_call_output` (and user content parts that map to Chat text/image_url).
- Anthropic and Gemini attempts always go codec → existing chat translators → codec back to Responses, and only when portable.

### Capability config and cache

- New optional `[[provider]]` field: `supports_responses` (bool, unset by default).
  - `true`: always use `/responses`; never fall back to Chat on probe failure.
  - `false`: never probe; Chat translate if portable, else skip.
  - unset: probe then cache.
- In-memory map keyed by provider name: “known Chat-only”. Set when a probe reports not-supported. Cleared on config reload so a provider upgrade is discovered without process restart.

### Output verification

- **Pass-through Responses:** Match MaiBot `_parse_completed_response` success criteria. HTTP 2xx with `status` in `failed` / `incomplete` / `cancelled` → `bad_output` (or provider error when hard failure details are present). Otherwise success if `output` has any of: non-empty message text (including refusal), non-empty reasoning display text (`summary` or `reasoning_text`), or at least one `function_call`. Empty usable output → `bad_output`.
- **Translated path:** existing Chat verification (`HasContent` + good finish reasons), then wrap as Responses.
- Prohibited-content detection continues to apply to raw downstream bodies.

### Claude trailing-user coercion

Apply existing Claude trailing non-user → user coercion on the Chat-shaped body after codec when the resolved model is Claude (including openai-kind proxies). Pass-through Responses bodies are not rewritten for this rule (Responses item model differs); if Claude is reached only via Chat translation or Anthropic dialect, coercion applies there as today.

## Error handling

| Case | Behavior |
| --- | --- |
| Probe not-supported | HTTP 404 on `/responses`, or 4xx whose body clearly indicates unknown/unsupported endpoint. Cache Chat-only; same attempt continues via Chat translate if portable. |
| Other 4xx/5xx on `/responses` | Normal provider error (auth, rate limit, invalid Responses payload). Existing blackout / advance. No Chat fallback. |
| `supports_responses=true` but backend lacks Responses | Provider failure only — no silent Chat fallback. |
| Chat-only / non-openai target + non-portable request | Skip attempt with outcome `incompatible` (no blackout). |
| All targets incompatible or exhausted | Client error (400) explaining Responses-only features require a Responses-capable provider, or existing multi-attempt failure body if portable but all providers failed. |
| `store` / `previous_response_id` | Pass through if present. Router does not implement server-side conversation store. MaiBot uses `store=false` and local item replay. |

Logging records outbound URL (`/responses` vs `/chat/completions`), probe-fallback, and raw bodies using existing log fields. No new Web UI required for v1.

## Configuration

Example:

```toml
[[provider]]
name = "openai"
kind = "openai"
base_url = "https://api.openai.com/v1"
keys = ["sk-..."]
# supports_responses = true   # force /responses
# supports_responses = false  # never probe; Chat translate only

[[provider]]
name = "openrouter"
kind = "openai"
base_url = "https://openrouter.ai/api/v1"
keys = ["sk-or-..."]
# leave unset: probe /responses once, cache if unsupported
```

Document in `config.example.toml` and README. Update inbound endpoint list to include `POST /v1/responses`.

## Testing

1. **Codec unit tests:** portable round-trip; `text.format` / `max_output_tokens` / function tools mapping; portability gate rejects native tools and non-portable input items.
2. **OpenAI dialect unit tests:** OpResponses pass-through path and URL; stream stripping; probe → Chat same attempt; cache suppresses re-probe; `supports_responses` true/false/unset.
3. **Verification unit tests:** message text, function_call-only, reasoning-only (per MaiBot rules), empty output, `status=failed|incomplete`.
4. **Router/server tests:** auth + routing smoke for `/v1/responses`; Anthropic/Gemini portable mock path; incompatible skip without blackout; all-incompatible client error; reload clears capability cache; existing chat/completions tests unchanged.

## Out of scope (v1)

- SSE / Responses streaming (including synthesizing stream events from Chat)
- Server-side `previous_response_id` / `store=true` conversation state
- Synthesizing native tool output items on the Chat translation path
- New provider `kind` value (capability is orthogonal to `openai` / `anthropic` / `gemini`)

## Success criteria

- MaiBot configured with `client_type = openai_responses` and router `base_url` can complete text, image, structured-output, and function-tool turns against Responses-capable openai providers.
- Same MaiBot client works against Chat-only openai proxies and Anthropic/Gemini for portable requests via translation.
- Requests that require native tools or non-portable item replay fail clearly unless a Responses-capable target exists.
- Existing `/v1/chat/completions` clients and tests remain green.

## Implementation notes

- Prefer small focused units: portability check, request codec, response codec, capability cache, OpResponses dispatch in openai dialect, server route wiring, verification branch.
- Do not invent a separate `openai_responses` provider kind.
- Keep pass-through Responses JSON fidelity for MaiBot `ProviderState.output_items` replay when the downstream is a real Responses API.
