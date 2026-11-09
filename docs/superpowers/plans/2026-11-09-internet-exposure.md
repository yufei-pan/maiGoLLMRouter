# Internet Exposure (API-only Listener and Hardening) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an `api_listen` listener that serves only `/v1/...` and `/healthz`, for an internet-facing TLS proxy on the same host to point at, and fix what the security review found: unbounded connections, memory and model-name storage, the `/ui/logs` crash, the Gemini path escape, the reload key bug, log modes, journal forging, and an unsandboxed unit.

**Architecture:**
- **server:**
  - One connection wrapper (`withConnLimits`) goes outside CORS on both servers. It sets the body deadline, closes connections whose body was left unread, enforces write progress, and refuses an oversized declared body.
  - A weighted memory budget of our own admits each body after it is read and before it is decoded.
  - Each handler decodes the body once. The preview, the token estimate and the audio scrub come from that decoded value.
- **main:** builds two muxes. The full mux gets `Register` and `webui.Register`; the API mux gets only `Register`. It binds both listeners before serving either.
- **logstore, router, provider, config, systemd.go:** take the smaller fixes.

**Tech Stack:** Go 1.26 standard library plus the existing `github.com/BurntSushi/toml` and `github.com/klauspost/compress`. Tests use `net/http/httptest` and raw `net` connections. The end-to-end check uses `systemd-run`.

**Spec:** `docs/superpowers/specs/2026-11-09-internet-exposure-design.md`. "D*n*" means the spec's decision *n*.

## Global Constraints

- No new Go dependency: no `golang.org/x/sync` or anything else. `go.mod` keeps `go 1.26.3`; this host builds with Go 1.27.3.
- **Unchanged:**
  - `/v1/...` behavior for a valid request (auth, routing, streaming, logs) apart from the limits below.
  - Every existing test passes unchanged.
  - `listen` keeps serving everything.
- **Public repository:** no private host names, LAN addresses or local service names in code, tests or docs.
- **Error shape for new refusals:** `{"error":{"message":…,"type":…,"code":…}}`, written by `writeCodedError`. The message states the limit. The values are exactly:
  - **413** for a declared `Content-Length` over 32 MiB: type `invalid_request_error`, code `body_too_large`, message `request body is larger than 32 MB`.
  - **503** when the memory budget doesn't admit within 25 s: type `server_error`, code `busy`, header `Retry-After: 5`, message `the router is busy with other large requests; retry shortly`.
  - **400** for a bad model name: type `invalid_request_error`, code `invalid_model`, message `model must be at most 256 bytes with no control characters`.
- **Exact values.** Each goes in a comment beside its constant, with its reason:
  - **`maxBody = 32 << 20`:** today's body cap, now one constant for every reader.
  - **Body deadline 5 min:** 32 MB at 1 Mbit/s takes about 4.5 min.
  - **Write piece 64 KiB, write deadline 60 s:** 64 KiB at 1 Mbit/s takes 0.5 s, so only a client that has stopped reading misses 60 s.
  - **`IdleTimeout` 120 s:** the UI polls every 3 s and annai sends turn after turn.
  - **`ReadHeaderTimeout` 10 s:** unchanged.
  - **`limit` cap 500:** five of the UI's 100-entry pages.
  - **Model names at most 256 bytes:** the longest in two months of logs is 57.
  - **Budget wait 25 s:** annai gives up after 30 s without a first event.
  - **Memory budget:** the measured number from Task 6. Never below 32 MiB.
- **Journal lines** put client- or provider-supplied text through `%q`, never `%s`.
- **APIs:** before Task 1, read `/mnt/optane/work/agent-rules/LESSONS.md` in full (RULES.md, "APIs"). The repo has no `docs/lessons.md` yet; Task 9 creates it.
- **Secrets and the live service:**
  - Never read `config.toml` values or anything under `logs/`.
  - Never print a key: no `cat` of the config copy, no `curl -v`, no `set -x`.
  - Never touch the live service on `:8470` or its unit. Deploying is out of scope.
- **`$SCRATCH`** is the executing session's scratchpad directory. Temporary binaries, configs and logs go there, never in the repo.
- **Commands:** every task ends with `go test ./... -count=1` and `go vet ./...`, plus `-race` where the task says so. Run `go test` with the default parallelism; nothing here needs it cut.

## Review Focus

1. **A keep-alive connection that carried a POST whose body was read** serves its next request with a live context. Setting the read deadline to now after such a request kills `net/http`'s background read, which cancels the connection's base context and with it every later request on that connection. This was reproduced on Go 1.27.3: server.go `backgroundRead` → `handleReadErrorLocked` → `conn.cancelCtx`. Test: Task 8, `TestKeepAliveAfterReadBodyKeepsContext`.
2. **An SSE stream that is silent for longer than the write deadline** (a reasoning model thinking) is not cut off: a deadline only bounds a write in progress. Test: Task 8, `TestStreamPauseLongerThanWriteDeadline`.
3. **A response whose handler works on after its last write** (`s.log` compressing a large entry) still reaches the client. `net/http` flushes after the handler returns, so the wrapper renews the write deadline then. Test: Task 8, `TestFinalFlushAfterSlowHandlerTail`.
4. **A client that sends `Expect: 100-continue`** (curl does for bodies over 1 MB) can upload a transcription, and is refused promptly without a key. Test: Task 8, `TestExpectContinueUpload`.
5. **Small requests go ahead while a big one waits for the budget.** The budget is not FIFO: a request that fits is admitted at once. Test: Task 7, `TestBudgetSmallPassesWaitingLarge`.

---

### Task 1: Configuration: `api_listen` and the reload key fix

**Files:**
- Modify: `config/config.go` (`Server`, `rawServer`, `build`), `config/reload.go`, `config.example.toml`
- Test: `config/config_test.go`, `config/reload_test.go`

**Interfaces:**
- Produces:

```go
// Server gains:
APIListen string // [server] api_listen; "" = no second listener
// rawServer gains: APIListen string `toml:"api_listen"`
```

**`Reload(path, prev)`:**
- **Carrying the generated key:** when the file's `client_keys` is empty and `prev.Server.GeneratedClientKey != ""`, it uses `[prev.GeneratedClientKey]` as the keys and keeps `GeneratedClientKey` set to that key.
- **When the keys came from the file:** if `prev.GeneratedClientKey == ""` and the file is now empty, `build` generates a new key and `GeneratedClientKey` is that new key. The file's old keys are gone.
- **When the file lists keys:** they replace everything, and `GeneratedClientKey == ""`.

- [ ] **Step 1: Write the failing tests:**
  - **`TestAPIListenLoads`:** `api_listen = "127.0.0.1:8471"` gives `Server.APIListen == "127.0.0.1:8471"`. Absent, it gives `""`.
  - **`TestReloadEmptyKeysDropsFileKeys`:** load with `client_keys = ["sk-file"]`, then reload a file with `client_keys = []`. Then `ClientKeys` has one key, it isn't `"sk-file"`, and `GeneratedClientKey == ClientKeys[0]`.
  - **`TestReloadKeepsGeneratedKeyTwice`:** load with `[]` (generated key G), then reload twice. After each reload, `ClientKeys == [G]` and `GeneratedClientKey == G`.
  - **`TestReloadFileKeysReplaceGenerated`:** load with `[]`, then reload with `["sk-new"]`. Then `ClientKeys == ["sk-new"]` and `GeneratedClientKey == ""`.
- [ ] **Step 2: Run them and watch them fail.** Run: `go test ./config -run 'TestAPIListen|TestReload' -count=1`. Expected: FAIL on the three new reload tests and `APIListen`. The existing `TestReloadPreservesClientKeys` passes throughout.
- [ ] **Step 3: Implement.** Add the spec's commented `api_listen` block (Behavior, `api_listen`) to `config.example.toml` under `listen`.
- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -count=1` and `go vet ./...`. Expected: PASS.
- [ ] **Step 5: Commit:**

```bash
git add config/ config.example.toml && git commit -m "Config: api_listen; a reload keeps only the generated client key."
```

---

### Task 2: Log store: file modes and the `limit` cap

**Files:**
- Modify: `logstore/logstore.go`
- Test: `logstore/logstore_test.go`, new `webui/webui_test.go`

**Interfaces:**
- Produces: `const maxBeforeLimit = 500` in `logstore`. `Store.Before` serves any `limit` above it as 500. The cap lives where the allocation happens, so every caller is covered.

**Modes:**
- `os.MkdirAll(…, 0o700)` in `New` and `Write`.
- `0o600` for entry files, `ensureHeader`, `appendIndexLine`, and `pruneIndex`'s temp file, which becomes the index.
- Existing files are not chmod-ed.

- [ ] **Step 1: Write the failing tests:**
  - **`TestBeforeCapsLimit`:** write 600 entries one second apart, then call `Before(<an hour after the last>, 100000000000)`. It returns 500 entries, newest first, without panicking.
  - **`TestLogFileModes`:**
    - `New(dir+"/logs")` gives the dir `0700` and `index.tsv` `0600`.
    - After one `Write`, the month and day dirs are `0700` and the entry file is `0600`.
    - After a `Cleanup` that drops an old row, `index.tsv` is still `0600`.
  - **`TestExistingLogModesUntouched`:** an `index.tsv` created `0644` before `New` is still `0644` after `New` and a `Write`.
  - **`webui/webui_test.go`, `TestLogsLimitCapped`:** the mux from `webui.Register` with a store holding 600 entries. `GET /ui/logs?before=<future>&limit=100000000000` answers 200 with 500 `entries`.
- [ ] **Step 2: Run them and watch them fail.** Run: `go test ./logstore ./webui -count=1`. Expected: FAIL. The cap test may die with an out-of-memory error, which is the bug.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -count=1` and `go vet ./...`.
- [ ] **Step 5: Commit:**

```bash
git add logstore/ webui/ && git commit -m "Log store: 0600 files, 0700 dirs; cap /ui/logs limit at 500."
```

---

### Task 3: Log store: previews, estimates and scrubbing from a decoded body

**Files:**
- Modify: `logstore/preview.go`, `logstore/scrub.go`, `logstore/logstore.go` (`Entry`, `indexEntryFor`)
- Test: `logstore/preview_test.go`, `logstore/scrub_test.go`, `logstore/logstore_test.go`

**Interfaces:**
- Produces:

```go
func RequestPreviewFromBody(body map[string]any, maxLen int) string
func EstimateInTokensFromBody(body map[string]any) int
// ScrubAudioBody returns the JSON to log and true when body holds input_audio
// (each clip as "<N bytes audio/<format>>", HTML not escaped); else nil, false.
// body is never modified: the maps and slices on the way to a clip are copied.
func ScrubAudioBody(body map[string]any) ([]byte, bool)
// Entry gains:
RequestPreview *string `json:"-"` // set by callers that decoded the body; nil derives it from Request
```

**One implementation each:**
- The walkers move to `map[string]any` and `[]any`.
- `RequestContentPreview(raw)`, `EstimateInTokens(raw)` and `ScrubAudio(raw)` stay, for their tests and for `Entry`s without a preview. Each decodes `raw` once and calls the body form:
  - `ScrubAudio` decodes with `UseNumber`, as today.
  - `EstimateInTokens` keeps `(len+3)/4` for invalid JSON.
- **`indexEntryFor`** uses `*e.RequestPreview` when it is non-nil, so it never decodes again. A legitimately empty preview is `ptr("")`, not nil.

- [ ] **Step 1: Write the failing tests:**
  - **`TestRequestPreviewFromBody`:** every valid-JSON row of `TestRequestContentPreview`'s table, decoded with `json.Unmarshal` into `map[string]any`, gives the same `want`. So does the Unicode-truncate case.
  - **`TestEstimateInTokensFromBody`:** every valid-JSON row of `TestEstimateInTokens`'s table falls in the same `[min,max]`. For each row, it also equals `EstimateInTokens` of the raw text.
  - **`TestFromBodyDoesNotAllocatePerMessage`:** a body of 100 000 `map[string]any{}` messages followed by `{"role":"user","content":"hi"}`. `testing.AllocsPerRun(5, …)` is at most 2 for each of `RequestPreviewFromBody` and `EstimateInTokensFromBody`.
  - **`TestScrubAudioBodyCopies`:**
    - a decoded chat body with one clip (`"QUJD"`, `format:"wav"`) gives JSON whose clip is `"<3 bytes audio/wav>"`, and `true`;
    - afterwards the input body's clip is still `"QUJD"`;
    - a body without `input_audio` gives `nil, false`.
  - **`TestWriteUsesGivenPreview`:**
    - an `Entry` with `Request` `{"messages":[{"role":"user","content":"from raw"}]}` and `RequestPreview: ptr("given")` gets index preview `"given"`;
    - with `ptr("")` it gets `""`;
    - the existing nil case still derives `"hi"`, as the existing test checks.
- [ ] **Step 2: Run them and watch them fail.** Run: `go test ./logstore -count=1`. Expected: FAIL (undefined functions and field).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -count=1` and `go vet ./...`.
- [ ] **Step 5: Commit:**

```bash
git add logstore/ && git commit -m "Log store: preview, token estimate and audio scrub from a decoded body."
```

---

### Task 4: Router and provider: blackout sweep, quoted warnings, Gemini model paths

**Files:**
- Modify: `router/blackout.go`, `router/router.go` (the bad-output warning), `provider/provider.go` (`Call`, `CallStream`), `provider/gemini.go`, `server/audio.go` (the speech warning)
- Test: `router/blackout_test.go`, `router/router_test.go`, new `provider/gemini_path_test.go`

**Interfaces:**
- Produces:
  - `func validGeminiModel(model string) bool`. After trimming `models/`, it is false when any `/`-separated segment is `..`, or the name contains `?`, `#` or `%`.
  - **`Call` and `CallStream`:** when `kind == "gemini"` and the model isn't valid, they return at once with nothing sent. `Call` returns `&Response{HTTPStatus: 404, RawResponse: b, OpenAIBody: b}, nil`, and `CallStream` returns `nil, <that>, nil`. Here `b` is `{"error":{"message":"model not found: <%q of name>","type":"invalid_request_error","code":"model_not_found"}}`. The router judges it like any 404 from an unknown model.
  - **`Blackout.Fail`:** after setting its entry, it deletes every entry whose time has passed, under the same lock.
  - **Warnings:** the router's line becomes `provider=%s model=%q key=%s finish_reason=%q%s`. `server/audio.go`'s line becomes `WARNING: speech not converted to %s: %q`.

- [ ] **Step 1: Write the failing tests:**
  - **`TestFailRemovesExpired`** (package `router`):
    - `b := NewBlackout(time.Nanosecond)`, then `b.Fail("k","old")`;
    - `b.SetDuration(time.Hour)`, then `b.Fail("k","new")`;
    - afterwards `len(b.until) == 1` and `b.Blocked("k","new")`.
  - **`TestGeminiModelPathRefused`:** an `httptest` server counts hits.
    - For each of `../x`, `a/../b`, `..`, `models/../files`, `x?alt=1`, `x#y` and `x%2e%2e`: `Call(…, "gemini", …)` gives `HTTPStatus == 404` with `"model_not_found"` in `OpenAIBody`, and so does `CallStream` (nil stream). The hit count stays 0.
    - `gemini-3.5-flash`, `models/gemini-3.5-flash` and `a..b` are sent (hits > 0).
  - **`TestGeminiPathModelSendsNothing`** (router): a direct request for `google/../x`, with `google` a gemini-kind provider, gets a failed `Result` with status 404 and 0 backend hits.
  - **`TestBadOutputWarningQuotes`** (router): capture `log.SetOutput`. The target model is `a\nb` and the fake provider answers finish reason `weird\nx`. The output contains `model="a\nb"` and `finish_reason="weird\nx"`, and has exactly one newline.
- [ ] **Step 2: Run them and watch them fail.** Run: `go test ./router ./provider -run 'TestFailRemovesExpired|TestGeminiModelPath|TestGeminiPathModel|TestBadOutputWarningQuotes' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -race -count=1` and `go vet ./...`.
- [ ] **Step 5: Commit:**

```bash
git add router/ provider/ server/audio.go && git commit -m "Sweep expired blackouts; quote client text in warnings; refuse Gemini model paths that escape."
```

---

### Task 5: Server: model names and the coded error shape

**Files:**
- Create: `server/limits.go`, `server/limits_test.go`
- Modify: `server/server.go` (`handle`), `server/audio.go` (`handleSpeech`, `handleTranscription`)

**Interfaces:**
- Produces:

```go
func writeCodedError(w http.ResponseWriter, status int, msg, typ, code string) // writeError's shape plus "code"
const maxModelName = 256 // bytes; the longest name in two months of logs was 57
func modelRefusal(model string) string // "" when routable, else the Global Constraints message; control = unicode.IsControl (C0, DEL, C1)
```

- **Placement:** each handler checks right after its existing "missing model" check, before `inflight.Register` / `beginAudio`, routing and `s.log`.
- **Transcription:** both shapes are covered, because the check runs on the `model` that `readTranscription` returns.

- [ ] **Step 1: Write the failing tests** (through `srv.Register` on a mux, with a real `logstore.New(t.TempDir())` and a fake provider that counts hits):
  - **`TestModelNameRefused`:**
    - **Names:** `strings.Repeat("m", 257)`, `"a\nb"`, `"a\x7fb"` and `"a\u0085b"`.
    - **Endpoints:** `/v1/chat/completions`, `/v1/embeddings`, `/v1/responses`, `/v1/audio/speech`, and `/v1/audio/transcriptions` as both JSON and multipart.
    - **Expected for each pair:** 400, `error.type == "invalid_request_error"`, `error.code == "invalid_model"`, and a message containing `256`. Afterwards `logs.Recent()` is empty and the provider has 0 hits.
  - **`TestModelNameAccepted`:** `strings.Repeat("m", 256)` and `"模型/名"` aren't refused with `invalid_model`. They reach the router, whose own unknown-model answer comes back.
- [ ] **Step 2: Run them and watch them fail.** Run: `go test ./server -run 'TestModelName' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -count=1` and `go vet ./...`.
- [ ] **Step 5: Commit:**

```bash
git add server/ && git commit -m "Refuse model names over 256 bytes or with control characters, with a coded error."
```

---

### Task 6: Server: decode once, and measure the worst body

**Files:**
- Modify: `server/server.go` (`handle`, `log`), `server/audio.go` (`handleSpeech`, `handleTranscription`)
- Create: `server/peak_test.go`
- Test: `server/server_test.go`

**Interfaces:**
- Consumes: Task 3's `RequestPreviewFromBody`, `EstimateInTokensFromBody`, `ScrubAudioBody` and `Entry.RequestPreview`.
- Produces:

```go
// log gains the preview and takes the request exactly as it is logged:
func (s *Server) log(id, endpoint, clientKey, model, preview string, latency int64, reqLog []byte, res *router.Result, stream bool)
func requestLog(raw []byte, body map[string]any) []byte // ScrubAudioBody's JSON when it changed something, else raw
```

**Behavior:**
- **`handle`:**
  - It keeps `orig := body` before coercion, and computes the preview and token estimate from `orig`.
  - It logs `requestLog(raw, orig)`, so the log is still the request as sent: raw bytes, and a re-encoding only when audio was scrubbed.
- **`handleSpeech`:** the index preview is `RequestPreviewFromBody(body, DefaultRequestPreviewLen)`, as the index shows today. The in-flight `preview(input)` is unchanged. It logs `requestLog(raw, body)`.
- **`handleTranscription`:** it logs its own `reqLog` with preview `""`, as today's index shows.
- **No second decode:** nothing in a request's path decodes the raw body again. `s.log` no longer calls `ScrubAudio`.

**Measurement harness** (`server/peak_test.go`), `TestMeasureWorstBodyPeak`:
- **Gating:** it skips unless `ROUTER_MEASURE_PEAK=<endpoint>-<kind>`, with endpoint `chat` or `responses` and kind `openai`, `anthropic` or `gemini`. Each case runs in its own process, because `VmHWM` only rises.
- **Body:** `{"model":"m","<messages|input>":[{},{},…,{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}]},{"role":"assistant","content":"x"}]}`, filled with `{}` to just under `32<<20` bytes (about 11 million items). It reaches the decode, the audio scrub and the coercion copy.
- **Config:**
  - `coerce_trailing_assistant = true`;
  - route `m` → `<kind>/real`, a fake provider of that kind;
  - `supports_responses = true` for openai, so Responses goes direct;
  - a real `logstore` in `t.TempDir()` and an `inflight` registry.
- **Fake provider:** it drains the request with `io.Copy(io.Discard, r.Body)` and answers a minimal valid reply for its dialect. Existing tests show each shape.
- **Measuring:**
  - after building the body and starting both servers, run `runtime.GC()` and read `VmHWM` from `/proc/self/status` as the baseline;
  - send the body with `http.Post` through `httptest.NewServer` (a real connection);
  - read `VmHWM` again;
  - print `PEAK case=<case> body=<bytes> delta_mib=<after-before>`.
  - The status may be anything; the request only has to run its full path.

- [ ] **Step 1: Write the harness first and record the "before" number** on the current code:

```bash
grep 'full avg60' /proc/pressure/memory    # run the six cases in parallel only if full avg60 <= 5; else one at a time
for c in chat-openai chat-anthropic chat-gemini responses-openai responses-anthropic responses-gemini; do
  systemd-run --scope --collect -q -p MemoryMax=8G -p MemorySwapMax=8G -- \
    env ROUTER_MEASURE_PEAK=$c go test ./server -run '^TestMeasureWorstBodyPeak$' -count=1 -v 2>&1 | grep -E 'PEAK|FAIL|^ok' &
done; wait
```

  - **Expected:** the review's blow-up, about 2 GiB or more for the worst case.
  - **Reference:** on this host, one `json.Unmarshal` of such a body into `map[string]any` alone measured 1.06 GiB `VmHWM`.
  - **Exit 137 or 143:** a case that hits its cap exits this way. Raise the cap to 16G and rerun it, and say so in the hand-back.
- [ ] **Step 2: Write the failing tests:**
  - **`TestChatPreviewFromDecodedBody`:** a chat request `{"model":"m","messages":[{"role":"user","content":"hi"}]}` whose fake provider blocks until the test has read `inflight.Snapshot()`. The snapshot shows `RequestPreview == "hi"` and `InTokens == logstore.EstimateInTokens(raw)`. After the request, the index preview is `"hi"`.
  - **`TestSpeechIndexPreviewUnchanged`:** speech with input `"hello world, this is long"` gives index preview `"hello world, thi"`.
  - **`TestAudioChatLogScrubbedFromBody`:** chat with an `input_audio` clip. The logged request has `"<3 bytes audio/wav>"` and no `QUJD`, and the index preview is that request's text.
  - Run `go test ./server -run 'TestChatPreviewFromDecodedBody|TestSpeechIndexPreviewUnchanged|TestAudioChatLogScrubbedFromBody' -count=1`. Expected: PASS on the current code. These tests pin behavior the refactor must keep; Step 1's number is the failing evidence. The existing `TestChatLogHasNoAudio` stays green throughout.
- [ ] **Step 3: Implement** the refactor.
- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -count=1` and `go vet ./...`.
- [ ] **Step 5: Measure "after"** with the Step 1 loop.
  - **Expected:** the worst `delta_mib` is close to one decode (about 1.1–1.5 GiB).
  - **If it is over 2 GiB,** something still decodes twice. Find it with `go test -memprofile` on that case before going on.
  - Write all twelve numbers into the hand-back, and the worst case's into Task 7's comment.
- [ ] **Step 6: Commit:**

```bash
git add server/ && git commit -m "Decode each request body once; add the worst-body peak measurement."
```

---

### Task 7: Server: the memory budget

**Files:**
- Create: `server/budget.go`, `server/budget_test.go`
- Modify: `server/server.go` (`Server`, `New`, `handle`), `server/audio.go` (`handleSpeech`, transcription split)
- Test: `server/limits_test.go`

**Interfaces:**
- Consumes: Task 5's `writeCodedError`; Task 6's worst `delta_mib` (P).
- Produces:

```go
type budget struct{ /* mu; size, used int64; waiters list of *waiter{n int64; ready chan struct{}} in arrival order */ }
func newBudget(size int64) *budget
// acquire takes n bytes (n > size is taken as size), waiting at most wait or until ctx ends.
// On false nothing is held. release must be called exactly once, at request end.
func (b *budget) acquire(ctx context.Context, n int64, wait time.Duration) (release func(), ok bool)

// memoryBudget bounds the bodies being decoded at once, in raw bytes.
// Measured <date> with TestMeasureWorstBodyPeak: the worst 32 MiB body (<case>) raised
// peak RSS by P MiB, P/32 per body byte. A full budget of such bodies must peak under
// 3 GiB, the memory this host usually has free: 3072 MiB * 32 / P = X MiB, rounded down
// to N MiB. Never below 32 MiB, so one maximum-size body always fits.
const memoryBudget = N << 20
const budgetWait = 25 * time.Second // annai gives up after 30 s without a first event (D9)

// Server gains: budget *budget; budgetWait time.Duration (set in New; tests replace them)
// admit waits for n bytes of the budget. If the wait runs out it answers 503 busy
// (Retry-After: 5) and returns ok=false; if the client went away it writes nothing.
func (s *Server) admit(w http.ResponseWriter, r *http.Request, n int64) (release func(), ok bool)
func readTranscriptionBody(w http.ResponseWriter, r *http.Request) (raw []byte, size int64, msg string)
func parseTranscription(r *http.Request, raw []byte) (model string, tr provider.TranscriptionRequest, msg string)
```

**The budget algorithm.** Neither the signature nor the tests determine it:
- **Admission:** `acquire` succeeds at once whenever `used+n <= size`, even while bigger requests wait. A request that fits never waits behind one that doesn't (Review Focus 5). Otherwise the waiter is appended to the list.
- **Release:** `used -= n`. Then walk the waiters in arrival order and grant every one that now fits: add its `n`, remove it, close `ready`. Don't stop at the first that doesn't fit.
- **Wait timeout or `ctx` done:** take the lock.
  - If `ready` was already closed (granted in the race), give the grant back through the release path and return false.
  - Otherwise remove the waiter.
- **Why the weight is the raw length:** it is the measure the arithmetic used, and ordinary bodies (KB of text, MB of base64 media) cost close to their own size to decode. So charging raw bytes over-counts them, never under-counts.

**Where each reader acquires.** Each acquires after its body is read in full and before decoding. Each handler does `release, ok := s.admit(w, r, n); if !ok { return }; defer release()`.
- **`handle` and `handleSpeech`:** right after `io.ReadAll`, with `n = len(raw)`.
- **Transcription:** `readTranscription` splits in two.
  - `readTranscriptionBody` reads everything. For multipart it runs `ParseMultipartForm` over a byte-counting wrapper on the `MaxBytesReader`, and `size` is the bytes counted. For JSON it is `io.ReadAll`, and `size = len(raw)`.
  - The handler admits `size`, then calls `parseTranscription`. It gets `raw == nil` for multipart and reads the form already parsed.

- [ ] **Step 1: Write the failing tests** (`budget_test.go`, run with `-race`):
  - **`TestBudgetWaitsThenRuns`:** size 10; A takes 6; B asks for 6 and waits; A releases; B gets in within 1 s.
  - **`TestBudgetTimeout`:** size 10, all held. `acquire(1, 50ms)` is false after at least 50 ms. Once the holder releases, `acquire(10, 0)` succeeds at once.
  - **`TestBudgetLargerThanSize`:** size 10; `acquire(20)` succeeds when empty, and then `acquire(1, 10ms)` is false.
  - **`TestBudgetSmallPassesWaitingLarge`** (Review Focus 5): size 10, 6 held. A waiter for 10 is queued, and `acquire(1, 0)` still succeeds at once.
  - **`TestBudgetCanceledWaiterFreesQueue`:** size 10, 6 held. A waits for 10, and B waits for 4 behind it. A's ctx is cancelled; then the 6 are released. B gets in, and `used` ends at 4.
  - **`TestBudgetFitsOrdinaryLoad`:** `newBudget(memoryBudget)` admits 16 concurrent 2 MiB acquisitions with `wait = 0`, and `memoryBudget >= 32<<20`.
  - **`limits_test.go`:**
    - **`TestBusyAfterWait`:** `s.budget = newBudget(1024)`, fully held, and `s.budgetWait = 100*time.Millisecond`. Chat, speech and multipart transcription each answer 503 with `code == "busy"`, `type == "server_error"` and `Retry-After == "5"`, after at least 100 ms.
    - **`TestBudgetWaitThenServe`:** the same, with the holder released after 50 ms and `budgetWait = 2s`. The chat answers 200 from the fake provider.
    - **`TestStalledUploadHoldsNoBudget`:**
      - `s.budget = newBudget(1024)`, behind a real `httptest` server.
      - A raw-connection POST with the key and `Content-Length: 1000` sends 10 bytes and stalls.
      - A second chat with a 900-byte body answers 200 at once (within 1 s).
    - **`TestBudgetReleasedAtRequestEnd`:** after a chat and a speech finish, `s.budget.acquire(ctx, 1024, 0)` succeeds.
- [ ] **Step 2: Run them and watch them fail.** Run: `go test ./server -race -run 'TestBudget|TestBusy|TestStalledUpload' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.** Set `N` from Task 6's worst `delta_mib`, with the arithmetic in the comment.
- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -race -count=1` and `go vet ./...`.
- [ ] **Step 5: Commit:**

```bash
git add server/ && git commit -m "Admit request bodies to decoding through a measured memory budget; 503 busy after 25 s."
```

---

### Task 8: Server: the connection wrapper and the HTTP servers

**Files:**
- Create: `server/conn.go`, `server/conn_test.go`
- Modify: `server/server.go` and `server/audio.go` (both use `maxBody`; `maxAudioRequest` and the literal `32<<20` go)

**Interfaces:**
- Consumes: `writeCodedError` (Task 5).
- Produces:

```go
const maxBody = 32 << 20  // today's cap for every body reader
const writePiece = 64 << 10
type ConnLimits struct {
	Body  time.Duration // reading a request body must finish within this
	Write time.Duration // each writePiece of a response must be taken within this
	Idle  time.Duration // http.Server.IdleTimeout
}
var DefaultConnLimits = ConnLimits{Body: 5 * time.Minute, Write: 60 * time.Second, Idle: 120 * time.Second}
// NewHTTPServer: Handler = withConnLimits(h, lim), ReadHeaderTimeout 10 s, IdleTimeout lim.Idle. No ReadTimeout or WriteTimeout (they would cancel long streams).
func NewHTTPServer(h http.Handler, lim ConnLimits) *http.Server
func withConnLimits(next http.Handler, lim ConnLimits) http.Handler
type progressWriter struct{ http.ResponseWriter; rc *http.ResponseController; stall time.Duration } // Write, FlushError, Flush, Unwrap
type eofBody struct{ io.ReadCloser; eof bool } // eof set when Read returns io.EOF
```

**The wrapper.** The order matters and was checked against Go 1.27.3's `net/http/server.go`:

```go
rc := http.NewResponseController(w)
pw := &progressWriter{ResponseWriter: w, rc: rc, stall: lim.Write}
var body *eofBody
if r.ContentLength != 0 { // chunked is -1. No body: the background read is already running, and a deadline would cancel its context.
	_ = rc.SetReadDeadline(time.Now().Add(lim.Body)) // net/http clears it when the body reaches EOF (startBackgroundRead)
	body = &eofBody{ReadCloser: r.Body}
	r.Body = body
}
if r.ContentLength > maxBody {
	writeCodedError(pw, 413, …, "invalid_request_error", "body_too_large") // before auth, on every path
} else {
	next.ServeHTTP(pw, r)
}
if body != nil && !body.eof {
	// Unread body (a 401, the mux's 404, a preflight, a truncated read): finishRequest would
	// drain up to 256 KB and wait for bytes that may never come. Only when unread: after EOF,
	// the background read is running, and failing it cancels the connection's context, so
	// every later request on a keep-alive connection would start cancelled.
	_ = rc.SetReadDeadline(time.Now())
}
_ = rc.SetWriteDeadline(time.Now().Add(lim.Write)) // net/http flushes after we return
```

**`progressWriter`:**
- **`Write`** loops over at most `writePiece` bytes at a time. Before each piece it calls `rc.SetWriteDeadline(now + stall)`, and it returns at the first error with the count so far. An empty write goes straight through.
- **`FlushError`** sets the same deadline, then calls `rc.Flush()`. `ResponseController` prefers `FlushError`, so `sseSink`'s flushes get a deadline.
- **`Flush`** calls `FlushError` and drops the error.
- **`Unwrap`** returns the inner writer, so `ResponseController`'s other methods reach the real connection.
- **A missed deadline** fails the write. `net/http` then records the error and cancels the request context, as for a client disconnect (server.go `checkConnErrorWriter`).

**Test helpers** (in `conn_test.go`):
- `startServer(t, h, lim) string` serves `NewHTTPServer(h, lim)` on a `127.0.0.1:0` listener whose accepted conns get `SetWriteBuffer(16<<10)`, and returns the address.
- `fullHandler(t, lim)` is `withConnLimits(srv.WithCORS(mux), lim)` around `srv.Register` and a fake provider.
- Raw requests go over `net.Dial` with `http.ReadResponse`.

- [ ] **Step 1: Write the failing tests:**
  - **`TestUnreadBodyClosesConn`:**
    - **Cases:** no auth on `/v1/chat/completions`, a bad key, `POST /healthz`, `POST /nope`, and a CORS preflight (`OPTIONS /v1/chat/completions` with `Origin` and `Access-Control-Request-Method`).
    - **Request:** headers with `Transfer-Encoding: chunked`, and no body bytes ever sent.
    - **Expected:** the reply arrives within 1 s, and the next read on the connection returns `io.EOF` within 1 s.
  - **`TestKeepAliveAfterReadBodyKeepsContext`** (Review Focus 1):
    - On one connection, send three requests with a key: a chat with `Content-Length`, a chunked chat, then `GET /v1/models`.
    - The fake provider sleeps 50 ms before answering, so a cancelled context would show.
    - **Expected:** all three answer 200 on the same connection.
  - **`TestBodyDeadline`** (`lim.Body = 200ms`):
    - **Stalled chat:** declares 100 bytes, sends 10, then stalls. It gets a 400 within 1 s and the connection closes.
    - **Stalled multipart transcription:** stalls mid-file. It gets a 400 within 1 s.
  - **`TestStreamOutlivesBodyDeadline`** (`lim.Body = 100ms`): a streamed chat whose provider sends SSE events over 500 ms. The client gets every event and `[DONE]`.
  - **`TestStreamPauseLongerThanWriteDeadline`** (Review Focus 2; `lim.Write = 100ms`): the provider sends one event, waits 400 ms, then sends another. The client gets both.
  - **`TestWriteStallCutsOff`** (`lim.Write = 200ms`). In both cases the client reads only the status line and then stops.
    - **Plain:** a handler writes 4 MiB in one `Write`. The `Write` returns an error within 2 s.
    - **Stream:** a handler does `newSSESink(w)`, `Commit`, then 1 KiB frames in a loop. A `Write` returns an error within 2 s, and `r.Context().Err() != nil`.
  - **`TestSlowReaderGetsAll`** (`lim.Write = 200ms`): a 1 MiB response, with the client reading 16 KiB every 20 ms. It gets all bytes, intact.
  - **`TestFinalFlushAfterSlowHandlerTail`** (Review Focus 3; `lim.Write = 100ms`): a handler writes `"ok"`, sleeps 300 ms, and returns. The client reads `"ok"`.
  - **`TestDeclaredSizeTooLarge`:**
    - `Content-Length: 33554433` on `/v1/chat/completions`, with no auth and no body bytes sent, gets 413 within 1 s with `code == "body_too_large"`, `type`, and a `message` containing `32 MB`.
    - `Content-Length: 33554432`, also unauthenticated, gets 401.
  - **`TestIdleTimeout`** (`lim.Idle = 200ms`): after `GET /healthz` with keep-alive, the connection reads `io.EOF` within 1 s.
  - **`TestExpectContinueUpload`** (Review Focus 4): an `http.Client` request with `Expect: 100-continue`.
    - A 2 MiB multipart transcription with a key gets 200 from the fake provider.
    - Without a key it gets 401 within 1 s.
  - **`TestProgressWriterControllers`:** through the wrapper, `http.NewResponseController(w)`'s `Flush`, `SetReadDeadline` and `SetWriteDeadline` all return nil.
- [ ] **Step 2: Run them and watch them fail.** Run: `go test ./server -race -run 'TestUnreadBody|TestKeepAlive|TestBodyDeadline|TestStream(Outlives|Pause)|TestWriteStall|TestSlowReader|TestFinalFlush|TestDeclaredSize|TestIdleTimeout|TestExpectContinue|TestProgressWriter' -count=1`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and watch them pass.** Then run `go test ./server -race -count=3` (these are timing tests: three clean runs) and `go test ./... -race -count=1`, and `go vet ./...`.
- [ ] **Step 5: Commit:**

```bash
git add server/ && git commit -m "Connection limits: body deadline, unread bodies close, write progress, 413, idle timeout."
```

---

### Task 9: Two listeners in `main`, reload, startup log, lessons

**Files:**
- Modify: `main.go`, `reload.go`
- Create: `main_test.go`, `docs/lessons.md`, `docs/lessons-full.md`

**Interfaces:**
- Consumes: `server.NewHTTPServer`, `server.DefaultConnLimits` (Task 8); `Config.Server.APIListen` (Task 1).
- Produces:

```go
func buildHandlers(srv *server.Server, logs *logstore.Store, active *inflight.Registry) (full, api http.Handler) // each srv.WithCORS(mux); only full's mux gets webui.Register
func listenAll(cfg *config.Config) (full, api net.Listener, err error) // api nil when APIListen == ""; an error names the address; on error, close what was opened
func logGeneratedKey(key string) // today's two startup lines, shared with reload
```

- **Serving:**
  - `main` binds both listeners before serving either. A bind failure is `log.Fatalf`; that also covers `api_listen` equal to `listen`.
  - Each listener is served in a goroutine, `NewHTTPServer(handler, server.DefaultConnLimits).Serve(ln)`, sending its error on `errc` (capacity 2). `log.Fatalf("server: %v", <-errc)` is the one shutdown path. There is no graceful shutdown, as today.
- **`logStartup`:** when `APIListen` is set, it adds `API-only listener: http://<api_listen>/v1 (no web UI)`, with `localhost` for a leading `:`, as for `listen`.
- **`tryReload`:**
  - A changed `api_listen` is logged as `config reload: api_listen %q -> %q ignored (restart required)` and reset, like `listen`.
  - When `next.Server.GeneratedClientKey != ""` and it differs from `prev`'s, it calls `logGeneratedKey`.
- **Lessons** (RULES.md "APIs": the same commit as the change). `docs/lessons-full.md` gets `## 2026-11-09 API-only listener`:
  - **Changed:** `api_listen` serves only `/v1/...` and `/healthz`.
  - **Why we had to change:** a same-host TLS proxy for the API would have exposed the unauthenticated UI and every logged prompt.
  - **Original design was wrong because:** the UI shared one listener with the API, on the assumption that the whole port was only ever reached locally.
  - **Lesson:** the Task 13 line, worded for this repo.

  `docs/lessons.md` gets its one line, `- **2026-11-09 API-only listener**: <lesson>`, with a two-line header saying what the file is (as LESSONS.md describes).

- [ ] **Step 1: Write the failing tests** (`main_test.go`, `package main`; configs written to `t.TempDir()`):
  - **`TestAPIListenerServesOnlyAPI`:**
    - **Setup:** `buildHandlers` with a real store holding one written entry, behind two `httptest.NewServer`s.
    - **Both servers:** `/healthz` answers 200, and so does `GET /v1/models` with the key.
    - **`/ui`, `/ui/logs`, `/ui/logs/entry?path=<the entry>`, `/ui/inflight` and `/ui/x`:** 404 on `api`, 200 on `full`.
  - **`TestListenAllSameAddressFails`:**
    - `listen` and `api_listen` both `127.0.0.1:<a free port>` gives an error containing that address, and nothing stays bound (the port can be listened on again).
    - `api_listen = ""` gives `api == nil`.
  - **`TestReloadIgnoresAPIListen`:**
    - A `configReloader` over a file with `api_listen = "127.0.0.1:1"`; the file is rewritten with `"127.0.0.1:2"`, then `tryReload` runs.
    - `r.cfg.Server.APIListen` is still `"127.0.0.1:1"`.
    - The captured log contains `api_listen "127.0.0.1:1" -> "127.0.0.1:2" ignored`.
  - **`TestReloadLogsNewGeneratedKey`:** a file with `client_keys = ["sk-file"]` is rewritten with `[]`, then `tryReload` runs. The captured log contains `r.cfg.Server.GeneratedClientKey`, and `r.cfg.Server.ClientKeys` lacks `sk-file`.
- [ ] **Step 2: Run them and watch them fail.** Run: `go test . -run 'TestAPIListener|TestListenAll|TestReload' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement**, and write the two lessons files.
- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -race -count=1` and `go vet ./...`. Then build and smoke-test outside systemd:
  - Run `go build -o $SCRATCH/router . && $SCRATCH/router -f <a test config in $SCRATCH with listen ":18470", api_listen "127.0.0.1:18471", log_dir $SCRATCH/logs>`.
  - The startup log names both addresses. `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18471/ui` prints `404`.
  - Stop it.
- [ ] **Step 5: Commit:**

```bash
git add main.go reload.go main_test.go docs/lessons.md docs/lessons-full.md && git commit -m "Serve the API alone on api_listen; reload ignores it and logs a newly generated key."
```

---

### Task 10: `--generate-systemd`: a sandboxed unit

**Files:**
- Modify: `systemd.go`, `main.go` (prints the notes)
- Test: `systemd_test.go`

**Interfaces:**
- Produces:

```go
// systemdUnitParams gains:
ExecPath    string   // absolute binary path
LogDir      string   // absolute; "" when the config could not be read
FFmpeg      string   // the config's ffmpeg value
ConfigError string   // why the config could not be read; "" when it was
func underHome(path string) bool                 // path is /home, /root, or below either ("/homework" is not)
func protectHome(p systemdUnitParams) string     // "yes", or "read-only" if any of ExecPath, WorkingDirectory, ConfigPath, LogDir, an absolute FFmpeg is underHome
func systemdNotes(p systemdUnitParams) []string  // lines printed after the install instructions
```

- **`resolveSystemdParams`:**
  - It reads the config with `config.Load`, the same parser the service uses.
  - A relative `log_dir` is joined to `WorkingDirectory` (the config's dir, as today) and cleaned.
  - On a load error, it sets `ConfigError` and leaves `LogDir` empty.
- **`writeSystemdUnit`:** it runs `os.MkdirAll(LogDir, 0o700)` when `LogDir != ""`; failing that is an error. The unit file itself stays `0o644`.
- **`renderSystemdUnit`:**
  - **`[Unit]`:** `RequiresMountsFor=` the quoted ExecPath, ConfigPath and LogDir, the ones that are set, space-separated.
  - **`[Service]`:** after `RestartSec`, the spec's fifteen lines in its order, with `ReadWritePaths=<LogDir>` and then `ProtectHome=<protectHome(p)>`.
  - **With `LogDir == ""`:** in place of `ReadWritePaths`, the comment `# ReadWritePaths=<log_dir>: add the absolute log directory; the config could not be read when this unit was generated`.
- **`systemdNotes`:**
  - When `User == "root"`: `Running as root. A dedicated user is safer: see "Running as a dedicated user" in the README.`
  - When `ConfigError != ""`: `Could not read the config (<err>); add ReadWritePaths=<log_dir> to the unit by hand.`

- [ ] **Step 1: Write the failing tests:**
  - **`TestRenderSandboxDirectives`:**
    - **Params:** ExecPath `/opt/router/maiGoLLMRouter`, ConfigPath `/etc/router/config.toml`, LogDir `/var/lib/router/logs`.
    - **Expected lines:** the unit contains each of the fifteen directive lines exactly, including `CapabilityBoundingSet=` as a whole line and `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`, plus `ReadWritePaths=/var/lib/router/logs`, `ProtectHome=yes`, and `RequiresMountsFor=/opt/router/maiGoLLMRouter /etc/router/config.toml /var/lib/router/logs`.
    - The existing `TestRenderSystemdUnit` still passes.
  - **`TestProtectHomeChoice`** (table):
    - all paths under `/opt` or `/etc` give `yes`;
    - a binary `/home/u/bin/r` gives `read-only`, which is the 203/EXEC case;
    - WorkingDirectory `/root/x` gives `read-only`;
    - LogDir `/home/u/logs` gives `read-only`;
    - FFmpeg `/home/u/ffmpeg` gives `read-only`;
    - FFmpeg `ffmpeg` (bare) gives `yes`;
    - a binary `/homework/r` gives `yes`.
  - **`TestGenerateResolvesLogDir`** (`t.Chdir(t.TempDir())`):
    - a config without `log_dir` gives `LogDir == <configdir>/logs`, and after `writeSystemdUnit` that dir exists with mode `0700`;
    - `log_dir = "<tmp>/abs"` is kept.
  - **`TestGenerateUnreadableConfig`:** invalid TOML gives a unit with no `ReadWritePaths=` line but the comment, and `systemdNotes` has a line containing `Could not read the config`.
  - **`TestRootNote`:** `systemdNotes` with `User:"root"` has the dedicated-user line; with `User:"router"` it doesn't.
- [ ] **Step 2: Run them and watch them fail.** Run: `go test . -run 'TestRender|TestProtectHome|TestGenerate|TestRootNote' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -count=1` and `go vet ./...`. Then run `systemd-analyze verify` on a unit generated into `$SCRATCH`. Expected: no errors about the sandbox directives. A warning that the binary path doesn't exist is fine.
- [ ] **Step 5: Commit:**

```bash
git add systemd.go systemd_test.go main.go && git commit -m "--generate-systemd writes a sandboxed unit with mounts, log dir and ProtectHome."
```

---

### Task 11: Documentation

**Files:**
- Modify: `README.md`
- Create: `docs/limitations.md`

**README.** Everything in the spec's "Docs to update" list:
- **Server table and "Running as a systemd service":**
  - `api_listen`, and that the UI is never served there; point an internet-facing proxy at it.
  - The sandboxed unit.
  - **New subsection "Running as a dedicated user":** create a system user. The config is readable by its group (`chgrp <user> config.toml; chmod 640 config.toml`), and the log directory is owned by the user (`chown -R <user>: logs; find logs -type f -exec chmod 600 {} +; find logs -type d -exec chmod 700 {} +`). Then re-run `--generate-systemd` as that user, or edit `User=`/`Group=`.
- **"Notes & limitations",** one line each:
  - model names at most 256 bytes with no control characters (400 `invalid_model`);
  - bodies at most 32 MB (413 `body_too_large`), and a body must arrive within 5 minutes;
  - a client that stops reading a reply for 60 s is cut off;
  - under memory pressure from large bodies, a request may wait up to 25 s, then get 503 `busy` with `Retry-After: 5`;
  - `/ui/logs` serves at most 500 rows per call;
  - any client key can use every route (D5);
  - a company network that inspects HTTPS can read a client key used on it, so give such devices their own key.

**`docs/limitations.md`:**
- **Header:** two lines saying what the file is and that reviewers read it first.
- **Entries:** one `### <name>` each, with the fields kind, what the operator sees, why, revisit when and, for the known limitation, approved.
  - **`### Every client key reaches every route`:** known limitation. Approved 2026-11-09: the user picked "Not needed" (D5).
  - **`### Per-key disk and rate quotas`:** deferred (D6), to the next version. A leaked key can fill the log disk and spend provider credit at any rate the memory budget allows.
  - **`### DNS rebinding against the UI`:** deferred (D7). A browser on the LAN or tailnet visiting a hostile page could be used to read the UI. The fix is a `Host` check on UI routes, in the next version.

- [ ] **Step 1: Write both files.** Then check them: `grep -nE '[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' README.md docs/*.md` shows only the loopback examples.
- [ ] **Step 2: Commit:**

```bash
git add README.md docs/limitations.md && git commit -m "Docs: api_listen, the new limits, a dedicated user, and limitations."
```

---

### Task 12: End-to-end check in a sandbox, without the live service

**Files:** none in the repo. Everything lives in `E2E=$SCRATCH/e2e` (`mkdir -m 700`). It is deleted at the end, because it holds a copy of the keys.

- [ ] **Step 1: Build and copy:**
  - `go build -o $E2E/maiGoLLMRouter .`, then `install -m 600 config.toml $E2E/config.toml`. Never `cat` it.
  - `grep -c '^\[server\]' $E2E/config.toml` prints `1`.
  - Delete any `listen`, `api_listen` or `log_dir` line: `sed -i -E '/^\s*(listen|api_listen|log_dir)\s*=/d'`.
  - Insert after `[server]`: `sed -i "/^\[server\]/a listen = \":18470\"\napi_listen = \"127.0.0.1:18471\"\nlog_dir = \"$E2E/logs\""`.
  - **Check:** `grep -nE '^\s*(listen|api_listen|log_dir)\s*=' $E2E/config.toml` shows exactly those three lines.
- [ ] **Step 2: Generate the unit:** `cd $E2E && ./maiGoLLMRouter --generate-systemd -f $E2E/config.toml`.
  - **Expected:** `$E2E/logs` exists, and the unit has `ReadWritePaths=$E2E/logs` and `ProtectHome=yes`. It holds only paths, so printing it is fine.
- [ ] **Step 3: Start a second router with the unit's sandbox properties:**

```bash
mapfile -t P < <(sed -n '/^\[Service\]/,/^\[Install\]/p' $E2E/mai-go-llm-router.service | grep -E '^(NoNewPrivileges|ProtectSystem|ProtectHome|ReadWritePaths|PrivateTmp|PrivateDevices|ProtectKernelTunables|ProtectKernelModules|ProtectControlGroups|RestrictNamespaces|LockPersonality|RestrictSUIDSGID|CapabilityBoundingSet|RestrictAddressFamilies|SystemCallArchitectures|UMask)=')
args=(); for l in "${P[@]}"; do args+=(-p "$l"); done
# BindPaths only because $SCRATCH sits under /tmp, which PrivateTmp hides; MemoryMax caps a runaway.
systemd-run --unit=router-e2e --collect -p WorkingDirectory=$E2E -p BindPaths=$E2E \
  -p MemoryMax=8G -p MemorySwapMax=8G "${args[@]}" -- $E2E/maiGoLLMRouter -config $E2E/config.toml
timeout 15 sh -c 'until curl -sf http://127.0.0.1:18470/healthz >/dev/null; do sleep 0.2; done'
```

  - **Expected:** `systemctl show router-e2e -p ProtectSystem -p ProtectHome -p PrivateTmp -p CapabilityBoundingSet -p RestrictAddressFamilies` matches the unit. `journalctl -u router-e2e` shows the startup lines naming both addresses.
- [ ] **Step 4: Through `api_listen` (`127.0.0.1:18471`).** The key goes into a variable without being printed: `KEY=$(python3 -I -c 'import sys,tomllib; print(tomllib.load(open(sys.argv[1],"rb"))["server"]["client_keys"][0])' $E2E/config.toml)`.
  - **Models:** pick them from `GET /v1/models` (route names aren't secret): a chat route, a Responses route, a TTS target, and a stepfun-kind ASR target.
  - **Streamed chat:** `"stream":true`. Expected: `data:` lines ending in `data: [DONE]`.
  - **Responses:** Expected: `"status":"completed"`.
  - **Speech:** `response_format: "opus"`, saved to `$E2E/s.ogg`. Expected: `Content-Type: audio/ogg`, `ffprobe -v error -show_entries stream=codec_name -of csv=p=0` prints `opus`, and `journalctl -u router-e2e` has no `speech not converted` line. That proves `ffmpeg` ran inside the sandbox.
  - **Transcription:** convert locally with `ffmpeg -loglevel error -i $E2E/s.ogg $E2E/s.flac`, then upload `s.flac` as multipart to the ASR target. stepfun gets flac converted to opus by the router's `ffmpeg`. Expected: `text` contains the spoken words. If no stepfun-kind ASR target is configured, transcribe with any ASR target, and note that speech alone proved `ffmpeg`.
  - **What this proves:** DNS, provider calls and `ffmpeg` all work under `RestrictAddressFamilies` and `PrivateDevices`.
- [ ] **Step 5: The UI split and file modes:**
  - `/ui`, `/ui/logs` and `/ui/inflight` print `404` on `:18471` and `200` on `:18470`.
  - `find $E2E/logs -type f ! -perm 600` and `find $E2E/logs -type d ! -perm 700` print nothing.
- [ ] **Step 6: Clean up:**
  - `systemctl stop router-e2e`, `unset KEY`, then `rm -rf $E2E`.
  - **Check:** `ss -ltn | grep -E ':1847[01] '` prints nothing, and the live `:8470` service was never touched.
  - Run `go test ./... -race -count=1` once more.
  - Deploying, the proxy entry, the dedicated user, and the proxy idle-timeout check from the spec are the main session's, with the user (memory `live-router-deploy`).

---

### Task 13: The cross-repo lesson (agent-rules repo, committed separately)

**Files:**
- Modify: `/mnt/optane/work/agent-rules/LESSONS.md`. This is a different git repository, committed there on its own commit, never in the router repo.

- [ ] **Step 1: Add this line** at the end of the `## Safety and authority` list, before `## People and automation`:

```markdown
- Serve an admin or data-viewing UI on its own listener from the first version, never on the API's: a proxy or tunnel on the same host makes every visitor look local, so no address check can keep it private. [maiGoLLMRouter]
```

- [ ] **Step 2: Check the cap:** `wc -c LESSONS.md` is under 20480.
- [ ] **Step 3: Commit in that repo:**

```bash
git -C /mnt/optane/work/agent-rules add LESSONS.md && git -C /mnt/optane/work/agent-rules commit -m "LESSONS: serve an admin UI on its own listener; a same-host proxy makes every visitor look local (maiGoLLMRouter: api_listen)"
```
