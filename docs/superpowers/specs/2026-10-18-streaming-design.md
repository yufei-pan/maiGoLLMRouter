# Streaming (SSE) for Chat Completions and Responses

**Date:** 2026-10-18
**Status:** Approved by the user on 2026-10-18; next step is the implementation plan
**Goal:** Let annai (annai.guide) use maiGoLLMRouter for its chat models: stream text, reasoning and tool-call arguments to the client as they arrive, while keeping the router's key rotation, blackout and target fallback for everything that happens before the first output.

## Why

annai is a browser app (the reason CORS was added in `d1311c0`). Its two model plugins, Chat Completions and OpenAI Responses, always send `stream: true` and can only read server-sent events. The router today strips `stream` / `stream_options`, buffers, and answers with one JSON body. annai's readers find no events in it and fail with "The reply stopped before it finished", so annai cannot use the router at all.

Speed matters as much as working at all. Shiori speaks through a `speak` tool, and annai plays her words sentence by sentence by reading that tool call's arguments while they are still being written (annai `src/core/agent/loop.js`, `#streamed`, using `parsePartialJSON`). Tool-call argument fragments must reach annai as the model writes them. A tool call delivered whole at the end means silence until the model has finished.

This is not for MaiBot. Non-streamed requests, which is everything MaiBot sends, must not change.

## What annai does that this design relies on

Read from `~/work/annai` on 2026-10-18. annai is still being built in another session; re-check these if it changes.

- **Chat plugin** (`plugins-official/chat-completions/index.js`) sends `stream: true` and `stream_options: {include_usage: true}`. It reads `choices[0].delta.content`, `delta.reasoning_content` or `delta.reasoning`, `delta.tool_calls[]` (merged by `index`; `id`, `function.name`, `function.arguments` fragments), `delta.annotations`, `choices[0].logprobs`, `finish_reason`, and `usage` (including `prompt_tokens_details.cached_tokens` and `cost`). A chunk with a top-level `error` object ends the stream as an error, and a numeric `error.code` is read as an HTTP status. A stream that ends with neither `[DONE]` nor a `finish_reason` is treated as broken.
- **Responses plugin** (`plugins-official/openai-responses/index.js`) sends `stream: true`, `store: false`, `instructions`, function tools with `strict: false`, and sometimes `reasoning`, `include` and `top_logprobs`. It reads `response.output_text.delta`, `response.refusal.delta`, `response.reasoning_summary_text.delta`, `response.reasoning_text.delta`, `response.output_text.annotation.added`, `response.output_item.added` (a `function_call` with `call_id`, `name`, `arguments`), `response.function_call_arguments.delta` (matched by `output_index`), `response.output_item.done`, `response.completed` / `response.incomplete` (with `response.usage` and `incomplete_details.reason`), `response.failed` and `error`. A stream without `response.completed` or `response.incomplete` is treated as broken.
- **Errors:** a non-2xx status is classified by its number (401/403 auth, 402 credit, 404 not found, 429 rate, 503/529 overloaded, other 5xx server), and annai's own fallback rests the model for a while depending on the kind. A real HTTP status on total failure therefore matters.
- **annai's own fallback:** when a stream breaks after some text, annai continues on its next model from the text already said (`src/core/llm/fallback.js`), and sends itself a `restart` event to drop half-spoken speech.
- **annai's timer:** `firstOutputMs` (default 30 s, configurable per role) is how long annai waits for the first stream event and then between events. When it runs out, annai aborts the request.
- **Tool-call ids and extra fields:** annai keeps tool calls as `{id, name, input}` only, so extra fields on a tool call (such as the router's `thought_signature`) are dropped and never sent back. Its `safeId` leaves ids made of `[A-Za-z0-9_-]`, up to 64 characters, unchanged. The router's Gemini ids (`call_` + 24 hex) survive.

## Goals

1. `POST /v1/chat/completions` and `POST /v1/responses` with `"stream": true` answer with `text/event-stream` in the format the matching OpenAI API defines. Every piece of output (text, reasoning, tool-call arguments) is written and flushed to the client as soon as the router has it.
2. Until the first piece of output is sent, the router behaves as it does today: key rotation, blackout, prohibited-content skipping, the normal-key and fallback-key rounds, and a real HTTP status with a JSON error body when everything fails.
3. Streaming works through the OpenAI-kind providers (most of `config.toml`: OpenRouter, DeepSeek, Z.ai, Qwen, local servers, local proxies) and through the Gemini-kind provider.
4. Gemini tool calling through the Gemini-kind provider (`google`) works across turns for clients that don't send `thought_signature` back (annai), streamed or not.
5. Logs, usage counts, the web UI and the in-flight view keep working for streamed requests.

## Non-goals

- **Native Anthropic streaming.** No Anthropic provider is configured. An Anthropic target asked to stream still answers correctly, buffered (path 6 below).
- **Audio.** `/v1/audio/transcriptions`, `/v1/audio/speech`, Gemini speech and transcription, and the router dropping `input_audio` parts for Gemini are the next project, designed separately.
- **`/v1/files`.** Later, separately.
- **Streamed tool-call arguments from Gemini.** The Gemini API doesn't offer it: Google's SDK references mark `streamFunctionCallArguments` and `partialArgs` as not supported on the Gemini API. Through Gemini, each tool call arrives whole, as soon as Gemini has finished writing it.
- **Gemini signatures through OpenAI-kind providers** (OpenRouter, Google's OpenAI-style endpoint, local proxies). There, Gemini 3's signatures travel in `reasoning_details` (OpenRouter) or `extra_content` (Google). annai fixes that in its own Chat plugin (see "Changes needed in annai"), and the router keeps passing those requests through unchanged.
- **Live token display** in the web UI.
- **Streaming for embeddings.** `stream` stays ignored there.

## Anti-goals

These would make the work a failure even if streams flow:

- Any change to non-streamed requests: bodies, statuses, retries or logging.
- Holding output back after the first piece to check it. Once output starts, it flows.
- Sending HTTP 200 and a stream that then says everything failed, when nothing had been committed. Failures before the first output must be real HTTP errors.
- Silent truncation. A stream that breaks must end with an error event, never with a clean `[DONE]` or `response.completed`.
- New dependencies beyond the standard library and `github.com/BurntSushi/toml`.

## Decisions

"User" means the user decided it. "Claude, accepted" means Claude proposed it and the user accepted it in conversation on 2026-10-18. "Claude, approved with the document" means Claude's call, made while writing this document and approved by the user with it.

| # | Topic | Decision | Who |
|---|---|---|---|
| 1 | What it's for | Real streaming for annai, so text and tool calls (including `speak` arguments) reach it as soon as possible. Not for MaiBot. | User |
| 2 | Endpoints | Chat Completions and Responses, each used by its own annai plugin. | User |
| 3 | Providers | OpenAI-kind and Gemini-kind must work. Anthropic is buffered (non-goal above). | User; Claude, accepted |
| 4 | Commit point | The router commits to an attempt when the first piece of real output arrives (defined below). Before that, nothing is sent to the client, not even headers. | Claude, accepted |
| 5 | Before commit | Exactly today's routing and failure handling. | Claude, accepted |
| 6 | After commit | No retry and no switching. A later failure becomes an error event inside the stream, and annai's own fallback takes over. | Claude, accepted |
| 7 | Bad finish after commit | A `length` or `content_filter` finish passes through as the stream's own finish, with no retry. | Claude, accepted |
| 8 | Responses | OpenAI-kind targets with `/responses` stream it through unchanged. Gemini, and OpenAI-kind targets on the Chat path, have their Chat stream translated into Responses events. | Claude, accepted |
| 9 | Usage | Always ask the provider for usage; pass it to the client only if the client asked. | Claude, accepted |
| 10 | Logging | Log the rebuilt complete reply, so logs, usage counts and the UI work as today. | Claude, accepted |
| 11 | Gemini thought signatures | The router remembers each Gemini tool call's signature by the tool-call id it issued, and adds it back when a client sends that call in history without it. | Claude, accepted |
| 12 | Gemini fallback signature | On a cache miss, send Google's placeholder signature, for `gemini-*` models only. | Claude, approved with the document |
| 13 | Timeouts | For streams, a provider's `timeout` limits the wait for the first output, then the gap between events. | Claude, approved with the document |
| 14 | Error after commit | Black out the normal key, as a provider error does today, unless the failure is a content-policy block. | Claude, approved with the document |
| 15 | Kill switch | `[server] streaming = true` (default). When false, a streamed request is served buffered and sent as a stream (path 6). | Claude, approved with the document |
| 16 | Provider ignores `stream` | A 200 JSON reply to a streamed request is checked as today and, if good, sent as a stream (path 6). | Claude, approved with the document |
| 17 | Gemini thoughts | Gemini thought parts (present only when the client asked for thoughts) stream as `delta.reasoning_content`. The non-streamed path keeps dropping them. | Claude, approved with the document |
| 18 | Translated reasoning on Responses | Reasoning from a Chat stream becomes a Responses `reasoning` item with a summary only: no `content` and no `encrypted_content`, so annai never sends it back and later requests stay portable. | Claude, approved with the document |
| 19 | Giving up on a stream | Failures inside a 200 stream before the first output get a real status and an `{"error": …}` body (table under "The commit point"). A streamed request that gives up never answers 2xx. | Claude, approved with the document |
| 20 | Every target refused | Answer with a short stream ending in `content_filter`, so annai sees a refusal rather than a bad key. | Claude, approved with the document |
| 21 | Trailing-turn rewrite | `coerce_trailing_assistant` never runs on streamed requests. The user also turned it off in the live `config.toml` on 2026-10-18, since MaiBot no longer needs it. | User |
| 22 | Gemini through OpenRouter | annai's Chat plugin keeps `reasoning_details` and `extra_content` and sends them back. The router passes them through and caches signatures for the `google` provider only. | User |
| 23 | Signature from another model | A cached signature goes back only to the provider/model that wrote it. Any other model is treated as a miss and gets the placeholder. | Claude, approved with the document (raised by a side note from the user) |

## Behavior

### Which requests stream

A request streams when it is `OpChat` or `OpResponses` and the JSON body has `"stream": true` (a JSON boolean). Anything else, including a missing or false `stream` or a string value, takes today's path unchanged.

A streamed request is never rewritten by `coerce_trailing_assistant`, whatever that setting says (decision 21). annai ends every Responses request with its "now" block as a `developer` message. The rewrite would turn that message into a `user` message and change I/me/my and 我 inside it.

### The commit point

**Real output:**

- **Chat chunk:** a non-empty `delta.content`, `delta.reasoning_content`, `delta.reasoning` or `delta.refusal`, or any `delta.tool_calls` entry. Gemini parts count after translation, by the same rule.
- **Responses event:** any `*.delta` event (output text, refusal, reasoning summary or text, function-call arguments), any `response.output_item.added` whose item is not an empty `message` or `reasoning`, or any `response.output_item.done`.

**Before commit**, the router keeps the events it has read so far, such as a role-only first chunk or `response.created`. At commit it sends the headers, then those events in order, then everything that follows.

**Outcomes before commit,** using today's outcome names and rules:

| What happens | Outcome |
|---|---|
| Transport error, or a non-2xx status | `provider_error`. Today's rules apply, including no blackout for 400. |
| Chat: a chunk with a top-level `error`. Responses: `error` or `response.failed`. | `provider_error`. A numeric `error.code` of 400 means no blackout, as for an HTTP 400. |
| Policy block: chat `content_filter` finish or the `PROHIBITED_CONTENT` marker; Gemini `promptFeedback.blockReason` or a policy `finishReason`; Responses `response.incomplete` with `content_filter` | `prohibited`. Skip the combo, no blackout. |
| The stream ends (EOF, `[DONE]`, `response.completed`) with no output | `bad_output`. Next key, no blackout, the same as an empty buffered reply. |
| The first-output timeout runs out | `provider_error` with blackout, the same as a buffered timeout today |
| The inbound request is canceled | `canceled`. Stop, no blackout. |
| A 200 JSON reply instead of a stream | Check it with today's rules; if good, send it as a stream (path 6) |

A failed attempt still records a **status and JSON body**, used if it turns out to be the last one (decision 19). Downstreams such as OpenRouter answer 200 and send comments before any output, so many failures happen inside a 200 stream. Those must not reach the client as a 200:

| Failure | Status | Body |
|---|---|---|
| Non-2xx HTTP reply | that status | the downstream body, as today |
| Chat `error` chunk with a numeric `code` between 400 and 599 | that code | `{"error": <the downstream error object>}` |
| Chat `error` chunk with a string code, or a Responses `error` / `response.failed` | by code: `rate_limit_exceeded`, `slow_down`, `insufficient_quota` → 429; `server_is_overloaded` → 503; `invalid_api_key` → 401; `model_not_found` → 404; `context_length_exceeded` → 400; anything else → 502 | `{"error": <the downstream error object>}`, which keeps `code` so annai's `classify` can sharpen it |
| The stream ended with no output | 502 | `{"error":{"message":"the provider's stream ended without output","type":"upstream_error"}}` |
| The first-output timeout ran out | 504 | `{"error":{"message":"no output from the provider in time","type":"upstream_error"}}` |

When every attempt fails, the client gets the last attempt's status and body, with `Content-Type: application/json`. A streamed request that gives up never answers with a 2xx. Other cases keep today's rules: the 400 when every target was incompatible, and 502 when no attempt reached a provider.

There is one exception: **every target refused on policy grounds** (decision 20). The router then answers 200 with a short stream that ends as a refusal:
- **Chat:** a role chunk, then a chunk with `finish_reason: "content_filter"`, then `[DONE]`.
- **Responses:** `response.created`, then `response.incomplete` with reason `content_filter`.

annai records that as the model declining. Today's 403 would read as a bad key (`auth`), and annai would rest the model for 3 minutes. The buffered path keeps its 403.

### After commit

- **Headers:** `200`, `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`. Each event is written and flushed as it arrives, with no batching.
- **Pass-through paths:** downstream `data:` payloads are forwarded unchanged, apart from the usage rule below. `model` stays as the downstream wrote it, as the buffered pass-through does today. SSE comment lines (OpenRouter's `: OPENROUTER PROCESSING`) are dropped.
- **Normal end:**
  - Chat ends with `data: [DONE]`. Responses ends with `response.completed` or `response.incomplete`, with no `[DONE]` (the Responses format has none).
  - The attempt is checked with today's `outputOK`, for the log only:
    - success calls `blackout.Success`;
    - a bad finish is logged as `bad_output`, with no blackout and no retry.
- **Failure mid-stream** (connection drop, idle timeout, a downstream error event, or a stream the router can't parse):
  - **Chat:** if the downstream sent its own `error` chunk, forward it. Otherwise send `data: {"error":{"message":"…","type":"upstream_error","code":502}}`, with `code` 504 for an idle timeout. Then close without `[DONE]`.
  - **Responses:** forward the downstream's `error` or `response.failed`. Otherwise send `event: error` and `data: {"type":"error","code":"server_error","message":"…","sequence_number":N}`. Then close without `response.completed`.
  - The attempt is logged as `provider_error`, and the normal key is blacked out unless the failure is a content-policy block (decision 14).
- **Client disconnects:** stop reading, close the downstream body, and log the attempt as `canceled` with what had arrived. No blackout.

### Timeouts (decision 13)

Today `http.Client{Timeout: p.Timeout}` limits the whole request. For streamed requests:

- **Before commit:** `timeout` limits the time from sending the request to the first real output.
- **After commit:** `timeout` limits the gap between two downstream lines. Comments and keep-alives count, so they reset it. There is no limit on total duration.

Non-streamed requests keep the whole-request limit. A streamed request can't use a client with `Client.Timeout`, because that would cut off long streams. It needs a client without one, with both limits enforced by timers (for example, by cancelling a per-attempt context).

**Interplay with annai:** annai aborts after 30 s without an event (its default). The configured provider timeouts are 200–3600 s, so for annai traffic annai's limit is the one that applies before the first output. When annai aborts, the router sees a canceled context and stops without blacking anything out. As a result, a key that hangs before its first token is never blacked out by annai traffic. This is an accepted limitation.

### Usage (decision 9)

- **OpenAI-kind Chat:** the router sets `stream_options.include_usage = true` downstream. If the client didn't ask for usage, the router removes `usage` from the chunks it forwards and drops chunks that carried only usage (empty `choices`).
- **Gemini:** the last `usageMetadata` becomes a usage chunk before `[DONE]`, sent only when the client asked.
- **Responses:** usage is part of `response.completed`, so nothing changes.

### Logging and the UI (decision 10)

- `Entry.Response` holds the rebuilt complete body:
  - **Chat:** a `chat.completion` object with content, `reasoning_content` when any arrived, tool calls with their full arguments, finish reason and usage.
  - **Native Responses:** the `response` object from `response.completed` or `response.incomplete`.
  - **Translated Responses:** the translator's final object.
- `Entry.Status` is 200 for a committed stream. `Entry.Success` is true only if the stream ended normally and passed `outputOK`.
- Attempts are logged as today. Failed attempts keep the raw downstream text they received in `Attempt.Response`.
- `Entry` gains `Stream bool` (`json:"stream,omitempty"`) so the UI can show that a request streamed.
- In the in-flight view, a committed request shows `streaming` as its last outcome.

### Paths

1. **OpenAI-kind, Chat.** `POST {base}/chat/completions` with the inbound body (model pinned), keeping `stream: true` and forcing `include_usage`. Read the SSE stream and forward it. The outcome comes from the chunks gathered so far: choice 0's finish reason, whether content or tool calls arrived, and policy markers.
2. **OpenAI-kind, Responses on `/responses`** (`supports_responses = true`, or a probe). `POST {base}/responses` with `stream: true`, and forward the events. In probe mode, a non-2xx reply that `isResponsesUnsupported` recognizes still marks the provider chat-only and switches to path 3 within the same attempt, as today.
3. **OpenAI-kind, Responses on the Chat path** (`supports_responses = false`, a cached chat-only verdict, or the probe fallback). `ResponsesToChat`, then path 1 internally, then the Chat-to-Responses translator below.
4. **Gemini, Chat.** `POST {base}/models/{model}:streamGenerateContent?alt=sse` with the same body `buildGeminiChatBody` builds today. Each SSE `data:` is a `GenerateContentResponse` fragment, translated into Chat chunks:
   - A first chunk with `role: assistant`.
   - Text parts become `delta.content`. Thought parts (`thought: true`) become `delta.reasoning_content` (decision 17).
   - Each `functionCall` part becomes one `delta.tool_calls` entry carrying the whole call:
     - the next `index`, an id of `call_<random>`, `type: function`, the name, and the full arguments as a JSON string;
     - `thought_signature` when the part has one, which is also stored in the signature cache.
   - The finish reason comes from `normalizeGeminiFinish`, and becomes `tool_calls` if any tool call was sent (the same rule as today).
   - Usage is handled as described under Usage, then `[DONE]`.
   - Blocks follow the before-commit and after-commit rules above. An `error` object in the stream is a downstream error.
   - Candidate *i* becomes choice *i*, as in the buffered translation.
   - The builder should check the chunk shapes against a live call: where signatures land (a final part with empty text and a signature is possible), and whether `usageMetadata` appears on every chunk.
5. **Gemini, Responses.** Today's portability check and `ResponsesToChat`, then path 4, then the Chat-to-Responses translator.
6. **Buffered, sent as a stream.** Used for Anthropic targets, for OpenAI-kind replies that ignore `stream` (decision 16), and for everything when `streaming = false` (decision 15).
   - Run today's buffered call, with all of today's checks and retries.
   - Send the final good reply as a stream:
     - **Chat:** a role chunk, one content chunk, one chunk per tool call with full arguments, a finish chunk, a usage chunk if asked, then `[DONE]`.
     - **Responses:** the full event sequence for that response object.
   - Nothing is sent until the reply is final, so failures are still HTTP errors.

Embeddings ignore `stream`, as today.

### Chat-to-Responses translator

Paths 3 and 5 use it. It sends the standard Responses streaming sequence. Each event has `type`, an increasing `sequence_number`, and an SSE `event:` line equal to its type.

- **Start:** `response.created`, then `response.in_progress`, each carrying the response object with status `in_progress` and empty `output`.
- **Reasoning** (when reasoning deltas arrive):
  - `response.output_item.added` (a `reasoning` item with an empty `summary`)
  - `response.reasoning_summary_part.added`
  - `response.reasoning_summary_text.delta`, repeated
  - `response.reasoning_summary_text.done`
  - `response.reasoning_summary_part.done`
  - `response.output_item.done`, with `summary: [{type: "summary_text", text}]` and no `content` or `encrypted_content` (decision 18)
- **Text:**
  - `response.output_item.added` (a `message` with role assistant, status `in_progress`, empty content)
  - `response.content_part.added` (`output_text`)
  - `response.output_text.delta`, repeated
  - `response.output_text.done`
  - `response.content_part.done`
  - `response.output_item.done`
- **Each tool call:**
  - `response.output_item.added` (a `function_call` with `id`, `call_id`, `name`, empty `arguments`)
  - `response.function_call_arguments.delta`, repeated
  - `response.function_call_arguments.done`
  - `response.output_item.done`, with the full arguments and `thought_signature` when there is one, matching `ChatToResponses`
- **End:**
  - Normally, `response.completed` with the full response object, in the same shape as `ChatToResponses` output, including usage.
  - On a `length` finish, `response.incomplete` with `incomplete_details.reason = "max_output_tokens"`.
  - On a `content_filter` finish, `response.incomplete` with reason `content_filter`.
- **Ordering:** items close in order, and `output_index` counts up by one per item. Text that arrives after a tool call starts a new message item.

### Gemini thought signatures (streamed and buffered)

Gemini 3 refuses a request (HTTP 400, "Function call is missing a thought_signature") when a function call in the current turn comes back without its signature. annai drops the field, so today the turn after any Gemini tool call fails when it goes through the router.

- **Store:** whenever the router translates a Gemini `functionCall` part that carries `thoughtSignature`, on any path, it records `tool-call id → (signature, provider/model that wrote it)` in an in-memory cache. It also keeps putting `thought_signature` on the outgoing tool call, as today.
- **Restore:** when building a Gemini request, an assistant tool call without `thought_signature` gets one from the cache by its id. This happens only when the request goes to the same provider/model that wrote it (decision 23). Responses input goes through `ResponsesToChat`, which already copies `thought_signature` from a `function_call` item when present, so the cache covers that path too.
- **A different model counts as a miss.** One inbound model name can expand to several Gemini targets, through fallback targets or `shuffle`, so the next turn may land on a different model than the one that signed. Developers report that Gemini rejects a signature written by another model ("Thought signature is not valid"). Google's docs don't say so, but the cost of assuming it is small.
- **On a cache miss** (decision 12): for the first `functionCall` part of a model turn, and only when the model name starts with `gemini-` (not Gemma), send Google's placeholder `skip_thought_signature_validator`. Google warns this may lower quality, but the request is accepted instead of refused.
- **Cache limits:** bounded (around 4096 entries, oldest dropped first, each kept for up to 24 h), shared by all providers, kept across config reloads, and lost on restart (the placeholder covers that). The exact sizes are the builder's to tune.

## Constraints

- Go 1.26, standard library plus `github.com/BurntSushi/toml` only.
- **Non-streamed behavior is unchanged.** `Router.Execute` stays the buffered entry point and keeps stripping `stream` / `stream_options`; tests such as `TestExecuteStripsStreamingControlsForOpenAIProvider` and `TestCallOpenAIStripsStreamingControls` stay valid for that path. The server sends `stream: true` requests to a new streaming entry point instead.
- **No router lock for the whole request.** Today `Execute` holds `r.mu.RLock()` for the whole routing run. Once a config reload waits for the write lock, Go's `RWMutex` blocks every new reader, so a long request during a reload stalls all new requests until it ends.
  - **It has already happened:** on 2026-10-18 at 22:02, a config edit stalled the live router for about 45 s behind a slow stepfun `/v1/responses` request. With 999 s timeouts it could have been far longer.
  - **Fix it first,** as its own change, for the buffered path too: take a snapshot of the config, clients and capability cache under the lock, then release it. The streaming path must work the same way.
- **Server:** `http.Server` has no `WriteTimeout`; keep it that way. `WithCORS` passes the `ResponseWriter` through unwrapped, so flushing works. Use `http.ResponseController` or `http.Flusher`.
- **Reverse proxies:** any proxy in front of the router must not buffer `text/event-stream`. The router sends `X-Accel-Buffering: no`; the README should mention it.
- **Config:** no new required fields. `[server] streaming` is optional and defaults to true. It reloads like the rest of `[server]`, and a reload affects only new requests.

## Testing

- **Fake downstreams** (`httptest`) that send SSE for each dialect:
  - **OpenAI Chat:** a role-only first chunk, tool-call arguments split across chunks, a usage-only chunk, OpenRouter-style comment lines, an `error` chunk before output, an `error` chunk after output, and a stream that stops without `[DONE]`.
  - **OpenAI Responses:** the full event sequence, plus `error` and `response.failed`.
  - **Gemini:** text, thought, a `functionCall` with a signature, a `promptFeedback` block, a `SAFETY` finish after text, and a final empty-text part that carries a signature.
- **Router:**
  - before commit: failover across keys and targets, blackout rules as today, a real status on exhaustion;
  - after commit: no failover;
  - client cancel before and after commit.
- **Server:**
  - headers;
  - one flush per event: the client must be able to read the first event before the fake downstream sends the second;
  - error-event formats, and no `[DONE]` or `response.completed` after an error.
- **Translators:**
  - Gemini to Chat;
  - Chat to Responses: sequence numbers, output indices, item ids, and a final object that matches `ChatToResponses` for the same reply.
- **Signature cache:** annai-shaped history (tool calls without signatures) round-trips to the same model. A different Gemini target gets the placeholder instead. The placeholder appears only for `gemini-*` models on a miss.
- **Consumer check:** feed the router's streams to annai's own `read()` functions from both plugins, for example with a small Node script run against captured router output, and confirm they yield the expected events.
- **Manual check:** point annai at the router with an OpenAI-kind model and a Gemini model on the `google` provider, with voice on:
  - **OpenAI-kind:** Shiori starts speaking while `speak` is still being written.
  - **Gemini:** she speaks once the call arrives, and the next turn after a tool call isn't refused.
- **Regression:** all existing tests pass unchanged.

## Changes needed in annai

annai is built in another session (`~/work/annai`). These are not router work, but annai needs them for Gemini to work across turns through OpenAI-kind endpoints (decision 22):

- **Store:** its Chat Completions plugin should keep, from a reply:
  - OpenRouter's `reasoning_details`: it arrives in streamed `delta.reasoning_details` and belongs to the assistant message;
  - Google's `extra_content` on tool calls (`extra_content.google.thought_signature` on Google's OpenAI-style endpoint).
- **Send back:** when that model gets the history again, send them back unchanged on the assistant message and its tool calls.
- **Like Responses reasoning:** treat them as signed provider data, sent back only to the model that wrote them, just as the Responses plugin already handles encrypted reasoning.
- **Not only for the router:** without this, Gemini 3 refuses the turn after a tool call even when annai talks to OpenRouter directly.

## Left to the builder

- The internal shape of the streaming entry point, for example a router method that takes a sink with `Commit(status, header)` / `Write(event)`, or a pull iterator the server drains.
- Where the signature cache lives, and its exact sizes.
- How rebuilt log bodies are assembled.
- Whether to add a first-output latency to the log entry and the UI.
- Exact SSE framing details not fixed above. For example, OpenAI Chat streams carry no `event:` lines, and the router need not add them.

## Docs to update

- **README:**
  - streaming in Features;
  - the meaning of a provider's `timeout` for streams;
  - the `[server] streaming` switch;
  - the reverse-proxy note;
  - the Gemini thought-signature behavior.
- **`config.example.toml`:** the `streaming` switch.
- **Code comments** that say streaming is stripped because the router buffers (`provider/openai.go`) should be updated to describe the buffered path only.
