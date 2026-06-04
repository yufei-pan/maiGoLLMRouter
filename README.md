# maiGoLLMRouter

A lightweight, single-binary LLM API router (in the spirit of OpenRouter /
LiteLLM). It accepts **OpenAI-style** requests with bearer auth and routes them
to **OpenAI**, **Anthropic**, or **Gemini** providers, with multi-key selection,
ordered fallbacks, key blackout, output verification + retry, TOML config, JSONL
request logging, and an embedded web UI.

Minimal dependencies: the Go standard library plus one TOML parser
(`github.com/BurntSushi/toml`).

## Features

- **OpenAI-compatible inbound API** (`POST /v1/chat/completions`, `POST /v1/embeddings`) with `Authorization: Bearer` auth validated against configured client keys.
- **Outbound dialect translation**: OpenAI (pass-through), Anthropic (`/messages`), Gemini (`:generateContent` / `:batchEmbedContents`). The version segment comes from each provider's `base_url`. Responses are translated back to OpenAI format.
- **Multiple normal keys per provider**, one chosen at random; all are tried before advancing.
- **Ordered fallback keys** per provider, tried only after every normal key/model is exhausted; never blacked out.
- **Key blackout**: a normal key that fails for a given model is skipped for that model for a configurable duration; the same key stays usable for other models.
- **Allowed-models gating** for fallback keys.
- **Output verification + retry**: chat responses must have content and a "good" finish reason; otherwise the request is retried (bounded, no blackout).
- **Chat and embeddings** support.
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
./scripts/release.sh 0.1.5
```

Artifacts land in `dist/`. Release notes since the previous tag:

```bash
./scripts/release-notes.sh 0.1.5 v0.1.4 > dist/RELEASE_NOTES.md
```

Tag `v0.1.5` and push to trigger the GitHub Actions release workflow, or
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

### Running as a systemd service

After you install the binary and `config.toml` where you want them to live
(e.g. `/usr/local/bin/maiGoLLMRouter` and `/etc/maiGoLLMRouter/config.toml`), run
`--generate-systemd` from the directory where you want the unit file written.
It resolves absolute paths for `ExecStart`, `WorkingDirectory`, and your
current login `User`/`Group`:

```bash
./maiGoLLMRouter --generate-systemd -f /etc/maiGoLLMRouter/config.toml
```

This writes `./mai-go-llm-router.service`. Install and start it:

```bash
sudo cp mai-go-llm-router.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now mai-go-llm-router
```

Check status or follow logs with `systemctl status mai-go-llm-router` and
`journalctl -u mai-go-llm-router -f`. Edit `User=` / `Group=` in the unit
file if needed, or re-run `--generate-systemd` after moving the binary or
config so paths stay correct.

### Example request

```bash
curl http://localhost:8470/v1/chat/completions \
  -H "Authorization: Bearer sk-local-change-me" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"Hello!"}]}'
```

## Configuration

See `[config.example.toml](config.example.toml)` for a fully commented example.

### Server


| Key                   | Meaning                                                                                                    |
| --------------------- | ---------------------------------------------------------------------------------------------------------- |
| `listen`              | Bind address (default `:8470`).                                                                            |
| `log_dir`             | Directory for JSONL logs (default `./logs`).                                                               |
| `client_keys`         | Accepted inbound bearer tokens. **If empty, a random key is generated at startup and printed to the log.** |
| `global_blackout`     | Duration a failed normal (key, model) combination is skipped (default `60s`).                              |
| `max_retries`         | Retries on the same key when output didn't finish normally (default `2`).                                  |
| `good_finish_reasons` | Normalized finish reasons treated as success.                                                              |


### Providers (`[[provider]]`, repeatable)


| Key               | Meaning                                                                                                                                                                                                                                                                                           |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `name`            | Provider name used in `provider/model` targets.                                                                                                                                                                                                                                                   |
| `kind`            | Outbound dialect: `openai`, `anthropic`, or `gemini`.                                                                                                                                                                                                                                             |
| `base_url`        | Provider API base URL **including the version segment** (e.g. `https://api.openai.com/v1`, `https://generativelanguage.googleapis.com/v1beta`, `https://openrouter.ai/api/v1`). The dialect appends the endpoint path (e.g. `/chat/completions`, `/messages`, `/models/{model}:generateContent`). |
| `timeout`         | Per-provider request timeout.                                                                                                                                                                                                                                                                     |
| `keys`            | Normal keys (random selection, blacked out on failure).                                                                                                                                                                                                                                           |
| `fallback_keys`   | Fallback keys (tried in order, never blacked out). If omitted but `fallback_models` is set, the normal `keys` are reused for the fallback round.                                                                                                                                                  |
| `fallback_models` | Models allowed on the fallback round (empty = all).                                                                                                                                                                                                                                               |


If the same `name` appears multiple times, the key lists are **combined** and the
scalar fields (`kind`, `base_url`, `timeout`) from the **last** entry win.

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
              on bad/unfinished output -> retry up to max_retries (no blackout), then next key
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

## Reasoning / thinking budgets

Please note that different providers — and even different models from the same
provider — likely expect **different reasoning / thinking budget parameters**
(e.g. OpenAI's `reasoning_effort`, Anthropic's `thinking.budget_tokens`, Gemini's
`thinkingConfig.thinkingBudget`). Unlike OpenRouter, maiGoLLMRouter does **not**
adapt or normalize any of these to maintain broad compatibility — whatever
reasoning fields you send are passed through to the downstream provider as-is.

Practical implications:

- If you want to specify reasoning, route to models of roughly the **same
  generation / provider** so a single reasoning parameter stays valid across the
  automatic model routing fallbacks. Mixing providers (or model generations) in a
  target list means a reasoning field valid for one target may be rejected or
  ignored by another.
- Even within the same provider, follow that **provider's own docs** to set the
  correct field name and value for the specific model you're targeting.

## Notes & limitations

- Streaming (SSE) is not supported; `stream` and `stream_options` are removed before forwarding so output can be verified before retrying.
- Anthropic does not support embeddings; embedding requests routed there return an error and advance.
- For translated dialects, pass-through of unknown arguments is best-effort. For Gemini, known top-level request fields (`toolConfig`, `safetySettings`, `systemInstruction`, `cachedContent`, `labels`, in camelCase or snake_case) are placed at the request root and an explicit `generationConfig` is merged in; all other extras go into `generationConfig`. For Anthropic, extras are merged top-level. OpenAI is pass-through except for unsupported streaming controls.
- **Gemini tool calling** is translated from the OpenAI shape: `tools` (`{type:"function", function:{...}}`) and the legacy `functions` array become Gemini `functionDeclarations` (the parameter JSON Schema is sanitized to the fields Gemini accepts, dropping e.g. `additionalProperties`/`$schema`), `tool_choice` becomes `toolConfig.functionCallingConfig`, assistant `tool_calls` and `tool` result messages become `functionCall`/`functionResponse` parts, and Gemini `functionCall` responses are converted back into OpenAI `tool_calls` (with `finish_reason: "tool_calls"`). Tools already in Gemini-native form (`{functionDeclarations:[...]}`) are passed through unchanged.
- `max_tokens` is required by Anthropic; if omitted it defaults to 8192.
