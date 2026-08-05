# Coerce trailing assistant turns (all models)

Date: 2026-08-05

## Problem

Some providers (Anthropic Claude, and Google Gemini) reject requests whose last conversation turn is an assistant message. The router already rewrites that turn to `user` for Claude model names only, inside `provider.Call`. Google models need the same treatment, and applying it only by model-name heuristics is brittle.

After flipping the turn to `user`, first-person pronouns in that turn become confusing (the model reads them as the user’s voice). They should be rewritten to second person.

## Goals

- Apply trailing-assistant → user coercion for **all** models, not Claude-only.
- Gate the feature with a server config toggle, **on by default**.
- Run the rewrite once at **ingest** (after JSON parse), not at outbound Call sites.
- When a turn is coerced, also rewrite pronouns in that turn’s text.
- Keep detail request logs as the original client bytes (`raw`).

## Non-goals

- Changing tool/function pairing rules.
- Rewriting pronouns in earlier turns or in turns that were already `user`.
- Re-marshaling coerced bodies into detail logs.
- Per-model or per-provider overrides (one global toggle only).

## Behavior

When `server.coerce_trailing_assistant` is enabled (default):

1. After inbound JSON unmarshal, for Chat (`messages`) or Responses (`input`):
2. Inspect the last array item.
3. If it has a `role` and that role is not `user`, `tool`, or `function`, set `role` to `user`.
4. **Only if** that role flip happened, rewrite text in that same turn:
   - English, word-boundary, case-insensitive:
     - `I` / `me` → `you`
     - `my` → `your`
     - `mine` → `yours`
   - Chinese, character replace: `我` → `你` (so `我的` becomes `你的`).
5. Typed Responses items without a role are left alone.
6. Embeddings and empty message/input arrays are no-ops.

Content shapes rewritten:

- string `content`
- multimodal content arrays: text-bearing parts (`text`, `input_text`, `output_text` string fields)

Image / non-text parts are untouched.

## Config

Under `[server]`:

```toml
# Rewrite trailing assistant (etc.) turns to user and flip pronouns in that
# turn. Default true when omitted.
coerce_trailing_assistant = true
```

| TOML value | Runtime |
|---|---|
| omitted | `true` |
| `true` | enabled |
| `false` | disabled |

Reload: live via existing `Server.Reload` (same as other server auth/routing knobs that do not require restart).

Document in `config.example.toml` and the embedded example in `main.go`.

## Architecture

```
client JSON → unmarshal → [coerce if enabled] → router.Execute(mutated body)
                ↓
         log keeps original `raw`
```

| Component | Change |
|---|---|
| `config.Server` | `CoerceTrailingAssistant bool`; parse `coerce_trailing_assistant`; default true when omitted (use `*bool` in raw TOML or equivalent). |
| `provider` | Export a pure helper (e.g. `CoerceTrailingAssistantTurn(body, field)`) that copies on write, flips role, and rewrites pronouns. Remove `withClaudeTrailingUserCoercion` / Claude gating from `Call`, `callResponsesViaChat`, and `openAIResponsesViaChat`. |
| `server.handle` | After unmarshal, if enabled and op is Chat/Responses, call the helper on `messages` / `input` before `Execute`. |
| Tests | See below. |

`isClaudeModelName` may remain only if still used elsewhere; otherwise delete it and its tests.

### Why ingest is enough for Responses→Chat

`ResponsesToChat` copies role-based input items into Chat `messages` with the same role. Coercing `input` at ingest means translated Chat bodies already end on `user`, so no second pass after translation is required.

## Pronoun rewrite details

- Apply only to the coerced last turn.
- Prefer longer English matches first where needed (`mine` before `me` is irrelevant with word boundaries; `my` is distinct from `mine`).
- Replacement output is lowercase `you` / `your` / `yours` regardless of input case (acceptable; keeps implementation simple). If a later change wants case-preserving output, treat it as a follow-up.
- Chinese: unconditional rune/string replace of `我` → `你` within the rewritten text fields (no word-boundary concept).

## Testing

**Helper unit tests**

- Trailing assistant → user + English/Chinese pronoun cases.
- Already-user / trailing tool / typed item → unchanged.
- Multimodal: text parts changed, image parts unchanged.
- Shared slice element maps are not mutated in place (copy-on-write).

**Config tests**

- Omitted → true; explicit true/false respected.

**Server / Call tests**

- Enabled: non-Claude models get coercion at ingest (body passed to router).
- Disabled: trailing assistant preserved for all models.
- `provider.Call` no longer performs the rewrite (repurpose/remove Claude-only Call coercion tests).

## Rollout

1. Implement helper + config + ingest hook.
2. Remove outbound Claude-only call sites.
3. Update example config / embedded docs.
4. Run package tests.
