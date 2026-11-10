# Lessons, in full

Why this repo's APIs, formats and agreed behaviour had to change: the story behind each line of `docs/lessons.md`, under the same date and name. New entries go last, in the same commit as the change:

```markdown
## YYYY-MM-DD <short name>
- Changed: ...
- Why we had to change: ...
- Original design was wrong because: ...
- Lesson: ...
```

## 2026-11-09 API-only listener
- Changed: `api_listen` serves only `/v1/...` and `/healthz`. The web UI (`/ui` and its JSON endpoints `/ui/logs`, `/ui/logs/entry` and `/ui/inflight`) is never registered on that listener, so those paths answer 404 there. `listen` still serves everything, and a reload can't change either address.
- Why we had to change: a same-host TLS proxy for the API would have exposed the unauthenticated UI and every logged prompt. The UI's endpoints return every logged request and reply of every client, and the live requests, to anyone who reaches them, with no key.
- Original design was wrong because: the UI shared one listener with the API, on the assumption that the whole port was only ever reached locally. A proxy on the same host delivers every request from 127.0.0.1, so no check on the client's address could tell an internet visitor from a local one.
- Lesson: register the web UI, and any route that shows logged prompts or live requests, only on `listen`'s mux, never on `api_listen`'s: a proxy or tunnel on the same host makes every visitor look local, so no address check can keep such a route private.

## 2026-11-09 client_keys reload
- Changed: a reload that finds `client_keys` empty no longer keeps the keys the previous config had. Keys that came from the file are revoked: the router keeps only the key it generated at startup (when it generated one), else generates a new key and logs it. Before, emptying the list kept every previous key, file keys included.
- Why we had to change: the API is now reachable from the internet, so removing a leaked key from `client_keys` and reloading has to revoke it. Taking the last key out of the list (or emptying it while rotating) left the old keys working after the reload, with nothing in the log to say so.
- Original design was wrong because: it read an empty list on reload as "no change", to keep the key generated at startup valid. That also made removal not total: the one case where an operator removes every key is the case where nothing was removed. The generated key is the only thing a reload may keep, and it is the router's own, not one the operator listed.
- Lesson: on a reload, a credential list the operator emptied revokes what it held; only a credential the program issued itself may carry over. What replaces the list is logged, so the operator sees what the API accepts now.

## 2026-11-10 OpenAI fields on translated dialects
- Changed: for Anthropic-kind targets, `reasoning_effort` becomes `output_config.effort` (`none` and `minimal` become `low`; an `output_config.effort` the request sets wins), and OpenAI-only fields (`store`, `parallel_tool_calls`, `metadata`, `user` and the rest of one shared list) are dropped instead of passed through; Gemini now takes the same list from the same place. Anthropic requests also get automatic prompt caching unless they set their own `cache_control`, and `usage.prompt_tokens` counts cached tokens, with `prompt_tokens_details.cached_tokens`.
- Why we had to change: coding agents pointed at the router could neither set Claude's effort nor run at all: `reasoning_effort` reached Anthropic as an unknown field (400 "Extra inputs are not permitted"), and pi sends `store: false` on every request, which turned every Claude call into the same 400. With caching on, `prompt_tokens` reported only Anthropic's uncached remainder (4 tokens for a 19,625-token prompt), so clients would misjudge the conversation's size.
- Original design was wrong because: passing unknown fields through as-is assumed the target would understand or ignore them. Anthropic and Gemini reject names they don't know, so a standard OpenAI field passed raw to a translated dialect can only fail; Gemini's dialect had already learned this, but in a list of its own the Anthropic dialect never got. Usage was translated field by field, so the fields Anthropic adds for caching were dropped silently.
- Lesson: a translator decides every standard field of the API it accepts that a target would reject: fields no translated dialect has are dropped from one shared list; fields only some dialects have are each dialect's to translate or drop; only fields that aren't the inbound API's own pass through. Translate usage as totals in the inbound API's meaning, not field by field.

## 2026-11-10 Reasoning effort shows its thinking
- Changed: a request that sets `reasoning_effort` on an Anthropic-kind target also gets `thinking: {type: adaptive, display: summarized}` unless it sets its own `thinking`, so Claude streams a summary of its reasoning.
- Why we had to change: Claude Opus 5.5 hides its thinking by default; at max effort it thought 277 s with nothing to stream (tested), and opencode gives up after 300 s waiting for a reply to start. Keep-alives don't help: opencode's SSE parser drops comment lines and its timers count only events.
- Original design was wrong because: translating the effort setting alone assumed a client that asks for more reasoning can wait in silence for it. A client asking for reasoning expects to see progress, and every client's timeouts are built for a stream that moves.
- Lesson: when a translated request can make the target work for minutes before its answer, ask the target for whatever progress it can stream as real events; silence and keep-alive comments both look dead to a client's timers.
