# maiGoLLMRouter

A lightweight, single-binary LLM API router (in the spirit of OpenRouter /
LiteLLM). It accepts **OpenAI-style** requests with bearer auth and routes them
to **OpenAI**, **Anthropic**, or **Gemini** providers, with multi-key selection,
ordered fallbacks, key blackout, output verification + retry, TOML config, JSONL
request logging, and an embedded web UI.

Minimal dependencies: the Go standard library plus one TOML parser
(`github.com/BurntSushi/toml`).

## Features

- **OpenAI-compatible inbound API** (`POST /v1/chat/completions`, `POST /v1/embeddings`, `POST /v1/responses`) with `Authorization: Bearer` auth validated against configured client keys.
- **Outbound dialect translation**: OpenAI (pass-through), Anthropic (`/messages`), Gemini (`:generateContent` / `:batchEmbedContents`). The version segment comes from each provider's `base_url`. Responses are translated back to OpenAI format. OpenAI-kind providers can pass Responses through to `/responses` (or probe then fall back to Chat); Anthropic/Gemini serve portable Responses via Chat translation.
- **Multiple normal keys per provider**, one chosen at random; all are tried before advancing.
- **Ordered fallback keys** per provider, tried only after every normal key/model is exhausted; never blacked out.
- **Key blackout**: a normal key that fails for a given model is skipped for that model for a configurable duration; the same key stays usable for other models.
- **Allowed-models gating** for fallback keys.
- **Output verification + retry**: chat responses must have content and a "good" finish reason; otherwise the request is retried (bounded, no blackout).
- **Streaming** (`"stream": true` on chat and Responses): text, reasoning and tool-call arguments go to the client as the provider writes them. Until the first output the router fails over between keys and targets as usual; from then on it is committed to that attempt, and a later failure arrives as an error event inside the stream. OpenAI-kind providers stream through; Gemini is translated; Anthropic replies are sent as a stream once complete.
- **Chat, embeddings, and Responses** support.
- **Audio**: `POST /v1/audio/speech` and `POST /v1/audio/transcriptions` through OpenAI-kind providers (passed through), stepfun (its own transcription route) and Gemini (translated), with the usual routing and failover. Speech comes back in the format the client asks for (`opus`, `mp3`, `aac`, `flac`, `wav`, `pcm`), converted with `ffmpeg`. Gemini chat models take `input_audio`.
- **Model mapping**: inbound model name → ordered list of `provider/model` targets.
- **Fallback providers**: a list of catch-all providers for unmapped names, tried in sequence (or shuffled).
- **Pass-through** of unknown request arguments to the downstream provider.
- **JSONL request/response logging** with masked keys, viewable in an embedded web UI.

## Releases

Pre-built binaries for Linux, macOS, and Windows (`amd64` / `arm64` where
applicable) are attached to [GitHub
Releases](https://github.com/yufei-pan/maiGoLLMRouter/releases). Verify with
`sha256sum -c SHA256SUMS.txt` after download.

To build release archives locally (requires Go 1.26+):

```bash
chmod +x scripts/release.sh
./scripts/release.sh 0.1.8
```

Artifacts land in `dist/`. Release notes since the previous tag:

```bash
./scripts/release-notes.sh 0.1.8 v0.1.7 > dist/RELEASE_NOTES.md
```

Tag `v0.1.8` and push to trigger the GitHub Actions release workflow, or
upload `dist/*` manually with `gh release create`.

## Quick start

Requires Go 1.26+.

```bash
cp config.example.toml config.toml   # then edit keys
go build -o maiGoLLMRouter .
./maiGoLLMRouter -f config.toml        # or --config config.toml (default: config.toml)
```

The server listens on `:8470` by default. The web UI is at
`http://localhost:8470/ui`. Run `./maiGoLLMRouter -h` for usage plus a reminder
of the required provider fields and where to obtain API keys, or
`./maiGoLLMRouter -V` (`--version`) to print the version.

The web UI has no sign-in and shows every logged prompt and reply, so keep
`listen` off the internet. To use the API from the internet, set `api_listen`
(see [Server](#server)): it serves the API alone, never the UI. Point an
internet-facing proxy at it.

### Running as a systemd service

After you install the binary and `config.toml` where you want them to live
(e.g. `/usr/local/bin/maiGoLLMRouter` and `/etc/maiGoLLMRouter/config.toml`), run
`--generate-systemd` from the directory where you want the unit file written.
It writes real paths, with symlinks resolved, for `ExecStart`, `-config` and
`WorkingDirectory` (the directory the config file sits in, even when that file
is a link to one elsewhere), and uses your current login `User`/`Group`:

```bash
./maiGoLLMRouter --generate-systemd -f /etc/maiGoLLMRouter/config.toml
```

This writes `./mai-go-llm-router.service`. The unit is sandboxed:

- The service can write only its log directory (`ProtectSystem=strict` with
  `ReadWritePaths=` the absolute `log_dir`; a relative one is resolved against
  `WorkingDirectory`, as the service resolves it). The generator creates that
  directory if it is missing. If it can't read the config, it says so, and the
  unit marks where to add `ReadWritePaths=` by hand.
- Home directories are hidden (`ProtectHome=yes`), or read-only when the
  binary, the config, the working or log directory, or an absolute `ffmpeg`
  path is under `/home` or `/root`.
- `RequiresMountsFor=` the binary, config and log directory, so a service on a
  separate disk waits for it.
- Memory is capped with `MemoryMax=6G` and `MemorySwapMax=6G`: twice the 3 GiB
  the router's request-body memory budget is sized to peak at, the usual cap
  sizing, so a runaway restarts the router instead of starving the host. Swap
  equals the cap so the kernel can still page the router out.
- The rest is in the unit: no new privileges and no capabilities, a private
  `/tmp` and `/dev`, only IP and Unix sockets, `UMask=0077`, and more.

Install and start it:

```bash
sudo cp mai-go-llm-router.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now mai-go-llm-router
```

Check status or follow logs with `systemctl status mai-go-llm-router` and
`journalctl -u mai-go-llm-router -f`. Reload routing config after editing
`config.toml` with `systemctl reload mai-go-llm-router` (sends SIGHUP; the
service also polls the file every 3s by default). Re-run `--generate-systemd`
after moving the binary, the config or the log directory, so the paths and
`ReadWritePaths=` stay correct.

#### Running as a dedicated user

The router needs no root: it writes only its log directory. It also runs
`ffmpeg` on client audio, so a user of its own limits what a decoder bug could
reach. `--generate-systemd` run as root says so and points here. Keep
everything the service uses (binary, config, log directory, an absolute
`ffmpeg` path) outside home directories, where that user can reach it and the
unit can hide homes.

Stop the service first, so the root service creates no file or day directory
while you change ownership: one made in between stays root-owned, the new user
can't write that day's directory, and log writes fail silently. Then create the
user, let its group read the config (it holds every key), and give it the log
directory. From the directory that holds `config.toml`, where the default
`log_dir = "./logs"` lives (use your `log_dir` if it is elsewhere):

```bash
sudo systemctl stop mai-go-llm-router
sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin mai-go-llm-router
sudo mkdir -p logs
sudo chgrp mai-go-llm-router config.toml
sudo chmod 640 config.toml
sudo chown -R mai-go-llm-router: logs
sudo find logs -type f -exec chmod 600 {} +
sudo find logs -type d -exec chmod 700 {} +
```

The router creates log files 0600 and directories 0700, but files written
before keep their modes, hence the two `find`s. If you haven't installed the
generated unit yet, do the `cp` and `enable` above (`enable` without `--now`,
so it doesn't start as root). Then set its user and group, and start the
service:

```bash
sudo sed -i 's/^User=.*/User=mai-go-llm-router/; s/^Group=.*/Group=mai-go-llm-router/' /etc/systemd/system/mai-go-llm-router.service
sudo systemctl daemon-reload
sudo systemctl start mai-go-llm-router
```

The generated unit drops all capabilities (`CapabilityBoundingSet=`), so
`listen` and `api_listen` need ports above 1023. It also holds the service to
`MemoryMax=6G` and `MemorySwapMax=6G`: leave that much free memory and swap.

### Example request

```bash
curl http://localhost:8470/v1/chat/completions \
  -H "Authorization: Bearer sk-local-change-me" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello!"}]}'
```

Responses API (same auth and model routing):

```bash
curl http://localhost:8470/v1/responses \
  -H "Authorization: Bearer sk-local-change-me" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","input":"Hello!"}'
```

When a target has to serve a Responses request through Chat translation
(Anthropic/Gemini, or an OpenAI-kind provider without a `/responses` route), the
request must be *portable*: function tools only, plain message/function-call
input items, and no `previous_response_id`. `instructions` becomes a leading
system message; `reasoning`, `include`, and `truncation` are dropped. A
non-portable request skips that target and falls through to the next one.

`input_file` parts are portable when they carry `file_data` (a data URL, or raw
base64 typed by `filename`) or `file_url`; a bare `file_id` is not, since it
names a file in the OpenAI file store. They become Chat `file` parts, which
Gemini receives as `inlineData`/`fileData` and Anthropic as `document` blocks
(PDF as base64, `text/*` as plain text, URLs as a `url` source, `filename` as
the title; image files become `image` blocks). Chat Completions requests with
`file` parts get the same mapping.

### Audio

Speech (`POST /v1/audio/speech`) takes OpenAI's JSON (`model`, `input`, `voice`,
`response_format`, `instructions`, `speed`) and answers with audio in
`response_format` (default `mp3`): `opus` (Ogg, 32 kbps mono, `audio/ogg`),
`mp3`, `aac`, `flac`, `wav` or `pcm` (24 kHz mono 16-bit). OpenAI-kind providers
are asked for `pcm` and the router converts it with `ffmpeg`; Gemini answers
with WAV, converted the same way. Safari plays Ogg Opus from 18.4 on; older
iPhones should ask for `mp3` or `aac`.

```bash
curl http://localhost:8470/v1/audio/speech \
  -H "Authorization: Bearer sk-local-change-me" -H "Content-Type: application/json" \
  -d '{"model":"shiori-voice","input":"Hello from Kyoto.","response_format":"opus"}' -o hello.ogg
```

Voices differ by provider, so a speech route can name one per target, and
the format to ask an OpenAI-kind provider for (default `pcm`):

```toml
[model."shiori-voice"]
targets = ["google/gemini-3.8-flash-lite-tts", "stepfun/stepaudio-2.5-tts"]

[model."shiori-voice".speech."google/gemini-3.8-flash-lite-tts"]
voice = "Kore"

[model."shiori-voice".speech."stepfun/stepaudio-2.5-tts"]
voice = "zixinnansheng"
```

A target's configured voice wins over the client's; on Gemini, a voice that
isn't one of its 30 prebuilt names becomes `Kore`.

Transcription (`POST /v1/audio/transcriptions`) takes OpenAI's multipart upload
(`file`, `model`, `language`, `prompt`, `response_format`, `temperature`) or
OpenRouter's JSON (`input_audio: {data, format}`), and answers `{"text": …}`
(or plain text for `response_format=text`). Gemini, which has no transcription
endpoint, is asked to transcribe the audio exactly; stepfun is reached on its
SSE route; audio stepfun can't read (e.g. WebM) is converted to Ogg Opus
first. An empty transcript (silence) is a success.

Logs record audio as its size and type, never its bytes; chat and Responses
requests carrying `input_audio` are logged the same way.

## Configuration

See `[config.example.toml](config.example.toml)` for a fully commented example.

### Server


| Key                   | Meaning                                                                                                    |
| --------------------- | ---------------------------------------------------------------------------------------------------------- |
| `listen`              | Bind address (default `:8470`). Serves everything: the API, `/healthz` and the web UI. The UI has no sign-in, so keep this address off the internet. Requires restart to change. |
| `api_listen`          | Optional second bind address serving only the API (`/v1/...`) and `/healthz`, never the web UI (`/ui` paths answer 404 there). Point an internet-facing proxy here, e.g. a TLS proxy on the same host at `127.0.0.1:8471`; never at `listen`, since a proxy on the same host makes every visitor look local. Unset (the default): no second listener. Both addresses are bound before either serves, and a failed bind stops the start, naming the key. Requires restart to change. |
| `log_dir`             | Directory for JSONL logs (default `./logs`).                                                               |
| `client_keys`         | Accepted inbound bearer tokens. **If empty, a random key is generated at startup and printed to the log.** A reload applies changes: a key removed from the list stops working, and emptying the list leaves only a generated key (the one generated at startup, else a new one printed to the log). |
| `global_blackout`     | Duration a failed normal (key, model) combination is skipped (default `60s`).                              |
| `max_retries`         | Deprecated, no-op. A key is never retried in place; a bad output moves on without blackout, while a provider error blacks out the key. Kept only so old configs still load. |
| `good_finish_reasons` | Normalized finish reasons treated as success.                                                              |
| `config_reload_interval` | Poll interval for automatic config reload (default `3s`). Set to `0` to disable polling; `SIGHUP` / `systemctl reload` still reloads. |
| `log_retention_days`  | Days to keep detail logs (default 60). `0` disables cleanup. Requires restart to change at runtime.        |
| `cors`                | Let web apps in a browser (such as annai) call the API: `/v1/...` and `/healthz`, never the web UI. Default `true`; `false` turns it off. The API asks for a client key and uses no cookies, so this gives a page nothing it couldn't do with the key. |
| `cors_origins`        | Limit `cors` to these exact origins, e.g. `["https://annai.guide"]`. Empty (the default) allows any origin. |
| `ffmpeg`              | Path to `ffmpeg`, used to convert speech to the format a client asks for (e.g. Opus). Default `ffmpeg` on `PATH`. Without it, speech is sent as the provider made it, under its true type (raw PCM wrapped as WAV). |
| `audio_timeout`       | Time limit for each speech or transcription attempt: the shorter of this and the provider's `timeout` (default `10s`), so a hung provider is failed over before a voice app gives up. |
| `streaming`           | Send a request that asks for a stream piece by piece as the provider writes it. Default `true`; `false` still answers it as a stream, but only once the reply is complete (and checked, with retries, as a non-streamed one). |


### Providers (`[[provider]]`, repeatable)


| Key               | Meaning                                                                                                                                                                                                                                                                                           |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `name`            | Provider name used in `provider/model` targets.                                                                                                                                                                                                                                                   |
| `kind`            | Outbound dialect: `openai`, `anthropic`, or `gemini`.                                                                                                                                                                                                                                             |
| `base_url`        | Provider API base URL **including the version segment** (e.g. `https://api.openai.com/v1`, `https://generativelanguage.googleapis.com/v1beta`, `https://openrouter.ai/api/v1`). The dialect appends the endpoint path (e.g. `/chat/completions`, `/messages`, `/models/{model}:generateContent`). |
| `timeout`         | Per-provider request timeout. For a streamed request it limits the wait for the first output, then the longest silence between pieces; a stream has no limit on its total length. |
| `keys`            | Normal keys (random selection, blacked out on failure).                                                                                                                                                                                                                                           |
| `fallback_keys`   | Fallback keys (tried in order, never blacked out). If omitted but `fallback_models` is set, the normal `keys` are reused for the fallback round.                                                                                                                                                  |
| `fallback_models` | Models allowed on the fallback round (empty = all).                                                                                                                                                                                                                                               |
| `audio`           | Optional, for `kind = "openai"`: `"stepfun"` when the provider speaks stepfun's audio API (speech as OpenAI's, but transcription on `/audio/asr/sse`). Default `"openai"`. |
| `supports_responses` | Optional. `true` = always `POST /responses`; `false` = never probe, Chat-translate portable Responses inbound; omit = probe `/responses` once and cache Chat-only until config reload. Applies to OpenAI-kind providers; Anthropic/Gemini always use Chat translation for portable Responses. |


If the same `name` appears multiple times, the key lists are **combined** and the
scalar fields (`kind`, `base_url`, `timeout`, `supports_responses`) from the **last** entry win.

### Model routing

```toml
[model."gpt-4o"]
targets = ["openai/gpt-4o", "google/gemini-1.5-pro"]

[model."free"]
selection = "shuffle"
targets = ["google/gemma-4-31b-it", "openrouter/openrouter/free"]

[routing]
fallback_providers = ["google", "openrouter"]
fallback_selection = "sequential"   # default; "shuffle"/"random" to randomize order
```

Optional `selection` controls how the expanded target list is ordered for each
request:

| Value        | Behavior                                            |
| ------------ | --------------------------------------------------- |
| `sequential` | Try targets in config order (default when omitted). |
| `shuffle`    | Shuffle the expanded targets before each request.   |
| `random`     | Alias for `shuffle`.                                |

A target is either a `provider/model` pair or the **name of another defined
model**, which is expanded in place (recursively, with cycle detection at load
time). For example:

```toml
[model."free"]
targets = ["google/gemma-4-31b-it", "openrouter/openrouter/free"]

[model."chat"]
targets = ["google/gemini-3.5-flash", "free"]
# chat expands to:
#   google/gemini-3.5-flash, google/gemma-4-31b-it, openrouter/(openrouter/free)
```

Resolution order for an inbound model name:

1. If it matches a `[model.*]` entry, use its ordered `targets`.
2. Otherwise, if it is `provider/model` with a **known** provider, route there directly with the remainder as the model (e.g. `openrouter/google/gemma-4` -> provider `openrouter`, model `google/gemma-4`).
3. Otherwise (unknown provider prefix, or a bare unmapped name), route to the `fallback_providers` using the **full original name** (e.g. `nvidia/nemotron-3` -> provider `fallback`, model `nvidia/nemotron-3`; `free` -> provider `fallback`, model `free`).

### Fallback providers

`fallback_providers` is a list of provider names used as the catch-all for
inbound names that don't match a `[model.*]` entry or a known `provider/` prefix.
The full original name is tried against each fallback provider as an additional
target, so a later provider can serve a name that an earlier one rejects (each
target still goes through the normal-keys then fallback-keys phases).

| Key                  | Meaning                                                                                          |
| -------------------- | ------------------------------------------------------------------------------------------------ |
| `fallback_providers` | Ordered list of provider names tried for unmapped inbound names.                                 |
| `fallback_selection` | `sequential` (default) tries them in listed order; `shuffle` (alias `random`) shuffles per request. |
| `fallback_provider`  | **Deprecated** singular form; still accepted and merged into `fallback_providers`.               |

Entries that do not name a defined provider are dropped with a warning; if none
remain, unresolved names return an error. The deprecated singular
`fallback_provider` is still accepted and appended to the list.

Examples (providers `google` + `openrouter` defined, `fallback_providers = ["openrouter"]`):


| Inbound                     | Resolves to                        |
| --------------------------- | ---------------------------------- |
| `google/gemma-4`            | `google` / `gemma-4`               |
| `openrouter/google/gemma-4` | `openrouter` / `google/gemma-4`    |
| `free`                      | `openrouter` / `free`              |
| `nvidia/nemotron-3`         | `openrouter` / `nvidia/nemotron-3` |


If only `google` is defined (so `openrouter` fallback is undefined), `google/gemma-4` still resolves to `google/gemma-4` while `openrouter/google/gemma-4`, `free`, and `nvidia/nemotron-3` all error.

## Routing algorithm

```
resolve inbound model -> ordered [provider/model] targets
  (unmapped names expand to one target per fallback provider,
   in sequential or shuffled order)

# Phase 1 — normal keys
for each target in order:
    shuffle the provider's normal keys
    for each non-blacked-out key:
        call; on transport/HTTP error -> blackout (key, model), next key
              on bad/unfinished output (empty content, bad finish reason)
                  -> next key, no blackout
              on success -> return

# Phase 2 — fallback round
for each target in order:
    skip if model not allowed by provider.fallback_models
    keys = fallback_keys, or the normal keys if fallback_keys is empty
    for each key in order:
        call (no blackout); on success -> return

# otherwise return the last upstream error
```

## Logging & web UI

Every request appends one JSON line to `logs/YYYY-MM-DD.jsonl` containing the
timestamp, masked client key, endpoint, inbound model, resolved targets, the
serving provider/model, per-attempt details (masked key, status, finish reason,
latency, outcome), and the request/response bodies. Downstream API keys are
never written to disk (only masked forms appear).

- `GET /ui` — HTML log viewer (embedded in the binary).
- `GET /ui/logs?limit=N` — recent log entries as JSON.

The web UI has no sign-in. It is served on `listen` only, never on
`api_listen`.

## Reasoning / thinking budgets

Please note that different providers — and even different models from the same
provider — likely expect **different reasoning / thinking budget parameters**
(e.g. OpenAI's `reasoning_effort`, Anthropic's `output_config.effort`, Gemini's
`thinkingConfig.thinkingBudget`). Unlike OpenRouter, maiGoLLMRouter does **not**
adapt or normalize these in general — the reasoning fields you send are passed
through to the downstream provider as-is — with one exception: for Anthropic-kind
targets, OpenAI's `reasoning_effort` becomes Anthropic's `output_config.effort`,
since Anthropic rejects `reasoning_effort` outright. `none` and `minimal` become
`low`, the least effort every current Claude model takes (Claude Opus 5.5 can't
turn thinking off); other values go on lowercased, for Anthropic to judge: the
levels differ by model (Claude Opus 5.5, Sonnet 5.5 and Haiku 5.5 take `low`,
`medium`, `high`, `xhigh` and `max`; older models take fewer, and some none). An
`output_config.effort` the request sets itself wins. Gemini targets still get
`reasoning_effort` as sent, and reject it.

Practical implications:

- If you want to specify reasoning, route to models of roughly the **same
  generation / provider** so a single reasoning parameter stays valid across the
  automatic model routing fallbacks. Mixing providers (or model generations) in a
  target list means a reasoning field valid for one target may be rejected or
  ignored by another.
- Even within the same provider, follow that **provider's own docs** to set the
  correct field name and value for the specific model you're targeting.

## Notes & limitations

- **Streaming**: once the first output has gone to the client, the router can no longer try another key, so a reply cut short (`length`, `content_filter`) or broken mid-way reaches the client as it is; a client with its own fallback (such as annai) takes over from there. When every attempt fails before any output, the client gets a normal JSON error with a real status, never a 200; when every target refused on policy grounds, it gets a short stream that ends as a refusal (`content_filter`). A reverse proxy in front of the router must not buffer `text/event-stream` (the router sends `X-Accel-Buffering: no` for nginx). Streamed requests are never rewritten by `coerce_trailing_assistant`. Gemini's API sends each tool call whole, so tool-call arguments stream piece by piece only through OpenAI-kind providers.
- Anthropic does not support embeddings; embedding requests routed there return an error and advance.
- For translated dialects, pass-through of unknown arguments is best-effort. For Gemini, known top-level request fields (`toolConfig`, `safetySettings`, `systemInstruction`, `cachedContent`, `labels`, in camelCase or snake_case) are placed at the request root and an explicit `generationConfig` is merged in; all other extras go into `generationConfig`. For Anthropic, extras are merged top-level. Both drop OpenAI-only fields that they have no counterpart for and would reject (`store`, `parallel_tool_calls`, `metadata`, `user`, `service_tier`, `max_tool_calls`, `prompt_cache_key`, `safety_identifier`, `logprobs`, `top_logprobs`, `include`, `reasoning`, `truncation`, `background`). OpenAI is pass-through (a non-streamed request has its streaming controls removed).
- **Anthropic prompt caching** is on for every request: the router adds Anthropic's automatic `cache_control: {"type": "ephemeral"}` (5-minute lifetime, renewed by each read) unless the request sends its own `cache_control`, such as `{"type": "ephemeral", "ttl": "1h"}`. A cache read costs a tenth of the input price or less, a write 1.25x; a prompt shorter than the model's minimum is simply not cached. `usage.prompt_tokens` counts the whole prompt, cached part included; `usage.prompt_tokens_details.cached_tokens` is the part read from cache and `cache_write_tokens` the part written to it (OpenRouter's name, which annai reads).
- **Gemini tool calling** is translated from the OpenAI shape: `tools` (`{type:"function", function:{...}}`) and the legacy `functions` array become Gemini `functionDeclarations` (the parameter JSON Schema is sanitized to the fields Gemini accepts, dropping e.g. `additionalProperties`/`$schema`), `tool_choice` becomes `toolConfig.functionCallingConfig`, assistant `tool_calls` and `tool` result messages become `functionCall`/`functionResponse` parts, and Gemini `functionCall` responses are converted back into OpenAI `tool_calls` (with `finish_reason: "tool_calls"`). Tools already in Gemini-native form (`{functionDeclarations:[...]}`) are passed through unchanged.
- **Gemini thought signatures**: Gemini 3 refuses a turn whose earlier tool call comes back without its thought signature. The router puts the signature on the tool call (`thought_signature`) and also remembers it by tool-call id (up to 4096, for 24 hours, until restart), so a client that drops the field still works; a remembered signature goes back only to the provider/model that wrote it. When none is known, the first call of a `gemini-*` turn gets Google's placeholder `skip_thought_signature_validator`, which Gemini accepts at some cost to reasoning. Through OpenAI-kind endpoints (OpenRouter, Google's OpenAI-style endpoint) the signatures travel in `reasoning_details` / `extra_content`, which the client must keep and send back; the router passes them through.
- **Gemini `input_audio`**: chat parts `{"type":"input_audio","input_audio":{"data","format"}}` (or a `data:` URI) reach Gemini as inline audio (`wav`, `mp3`, `ogg`, `webm`, `flac`, `aac`, `m4a`/`mp4`, `aiff`). OpenAI-kind providers get them unchanged. Anthropic targets are skipped for speech and transcription.
- **stepfun's Step Plan** includes `stepaudio-2.5-tts` and `stepaudio-2.5-asr` but no audio-input chat model, so chat with `input_audio` should go to Gemini or OpenRouter.
- `max_tokens` is required by Anthropic; if omitted it defaults to 8192.
- **Model names** are at most 256 bytes, with no control characters; any other name gets 400 `invalid_model` before it is routed or logged.
- **Request bodies** are at most 32 MB: a larger `Content-Length` gets 413 `body_too_large` before anything is read, and a body that states no length (chunked) gets the same 413 once it passes 32 MB. A body must arrive in full within 5 minutes; a stalled one gets 400 and its connection is closed.
- **JSON values**: a request body holding more than 100,000 JSON values (each object, array, string, number, bool and null counts once; object keys don't) gets 400 `too_many_values` before it is routed or logged.
- **A client that stops reading** a reply for 60 s is cut off, as if it had disconnected. A slow reader that keeps reading still gets the whole reply.
- **Under memory pressure from large bodies** (the bodies being decoded at once share a 63 MiB budget), a request may wait up to 25 s, then get 503 `busy` with `Retry-After: 5`.
- **Idle connections** close after 120 s. A proxy in front of the router should drop its idle connections sooner, or it may now and then send a request on one the router just closed and answer its client 502.
- **`/ui/logs`** serves at most 500 rows per call; a larger `limit` is served as 500.
- **Every client key can use every route**: any key in `client_keys` reaches every `/v1/...` route and model, on both listeners. There is no per-key scoping.
- **Networks that inspect HTTPS** (common on company networks) can read a client key used on them. Give such devices their own key, so removing it from `client_keys` (a reload is enough) revokes only that device.
- Known limitations and work deferred to the next version are listed in [`docs/limitations.md`](docs/limitations.md).
