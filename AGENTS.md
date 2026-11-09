# AGENTS.md

## Cursor Cloud specific instructions

### Product

Single Go binary **maiGoLLMRouter**: OpenAI-compatible API on `:8470`, JSONL logging, embedded UI at `/ui`. No Docker, database, or separate frontend.

### Dependencies

- **Go 1.26+** (see `go.mod`). Module deps are fetched automatically by `go test` / `go build`.
- **Python 3** (stdlib only): optional, for `scripts/mock-openai-provider.py` when exercising the router without real LLM API keys.

### Standard commands

| Task | Command |
|------|---------|
| Tests | `go test ./...` |
| Static check | `go vet ./...` |
| Build | `go build -o maiGoLLMRouter .` |
| Run (real providers) | `cp config.example.toml config.toml` (edit keys), then `./maiGoLLMRouter -f config.toml` |

There is no project Makefile or golangci-lint config; CI only builds release artifacts on version tags.

### Local E2E without upstream API keys

1. Start mock OpenAI upstream: `python3 scripts/mock-openai-provider.py` (listens on `127.0.0.1:9999`).
2. Start router: `./maiGoLLMRouter -f config.dev.toml` (uses mock provider and `client_keys = ["sk-local-dev-key"]`).
3. Smoke checks:
   - `curl http://localhost:8470/healthz` → `ok`
   - Chat: `curl http://localhost:8470/v1/chat/completions -H "Authorization: Bearer sk-local-dev-key" -H "Content-Type: application/json" -d '{"model":"demo","messages":[{"role":"user","content":"Hello!"}]}'`
   - UI: `http://localhost:8470/ui` (logs API: `GET /ui/logs?limit=N` with same bearer token)

Use **tmux** for long-running `mock-openai-provider` and `maiGoLLMRouter` processes in cloud VMs.

### Gotchas

- `config.toml` is gitignored; use `config.example.toml` or committed `config.dev.toml` for local demos.
- If `client_keys` is empty in config, a random inbound key is generated at startup and printed to stderr — check logs when testing auth.
- Streaming is not supported; chat completions are non-streaming only.
- Real LLM E2E requires valid upstream keys in `config.toml` and outbound network access.
