# Internet exposure: an API-only listener and hardening

**Date:** 2026-11-09
**Status:** Spec-checked twice on 2026-11-09 against the user's words; next step is the implementation plan
**Goal:** Let clients on the internet use the router's API through a TLS-terminating proxy on the same host, while the web UI stays reachable only on the tailnet and LAN, and fix what a security review found that anyone, or anyone holding a leaked client key, could do.

## Why

The user wants to use the router's API routing from outside, including from devices on a company network where the tailnet the router is reachable on today isn't available. The web UI is checked from the user's phone over the tailnet, so it doesn't need to be on the internet (decision 2).

Before opening the API, the user asked for a security review. The review (2026-11-09: an Opus review that ran tests against a scratch copy, a second-opinion review, the main session's own checks, and a design check that ran tests against Go 1.27.3's `net/http`) found:

- **The web UI and its JSON endpoints have no auth.** `/ui/logs`, `/ui/logs/entry` and `/ui/inflight` give anyone who can reach them every logged prompt and reply (all clients, kept 60 days) and the live requests. A same-host proxy (frp's `https2http` plugin here) delivers every request from 127.0.0.1, so no check on the client's address can tell internet visitors from local ones. Only a separate listener can keep the UI off the internet.
- **One request with no key crashes the process.** `GET /ui/logs?before=x&limit=100000000000` makes `Store.Before` call `make([]IndexEntry, 0, limit)`, which is a fatal out-of-memory error that no `recover` catches. Reproduced. After this change the UI is off the internet, but anyone on the LAN or tailnet can still send it.
- **Connections without a key are held forever.** `http.Server` sets only `ReadHeaderTimeout`:
  - An idle keep-alive connection never closes.
  - A chunked `POST` with no auth whose body never comes holds its connection after the 401 too. Even with `Connection: close`, `finishRequest` closes the request body, which drains up to 256 KB from the connection (`net/http` server.go:1733–1736, transfer.go:1019–1031).

  Both reproduced.
- **With a leaked client key:**
  - A 32 MB body peaks at up to 2.5 GB of heap, because it is decoded three times. Nothing bounds how many such requests run at once.
  - Model names have no length limit. They are kept in the UI cache, in `index.tsv`, and in the blackout map, where an entry for a name never asked about again is never removed.
  - A stream to a client that stops reading never times out.
  - A Gemini model name goes into the URL path unescaped, so `..` reaches other Gemini API paths with the router's key.
- **Hardening:**
  - The service runs as root while `ffmpeg` decodes client audio. Its arguments are well locked down, so only a decoder bug in `ffmpeg` itself would matter.
  - Log files are 0644 and log directories 0755.
  - Emptying `client_keys` on reload silently keeps the old keys (`config.Reload`), so revoking every key does nothing until a restart.
  - A model name containing a newline can forge lines in the journal.

Checked and fine:
- The router never fetches URLs a client sends; providers do.
- Log paths can't escape the log directory.
- About 10M fuzzed inputs caused no panic.
- The UI escapes everything it shows.
- Provider keys are never shown whole.
- Auth is checked before the body is read.
- The configured client keys are random and long.

## Goals

1. A new listener serves the API and `/healthz` only, never the web UI, for an internet-facing proxy to point at. The existing listener stays as it is.
2. No request without a key can crash the router or hold its resources without bound.
3. With a leaked client key, memory, model-name storage and stalled streams stay bounded.
4. The service can run as an unprivileged, sandboxed user, and `--generate-systemd` writes a sandboxed unit.

## Non-goals

- Any sign-in for the web UI (decision 2).
- Per-key route scoping: every client key keeps reaching every route (decision 5).
- Per-key rate or disk quotas (decision 6, deferred).
- TLS inside the router: the proxy terminates TLS, as it does for the user's other services.
- Protecting the UI on the LAN and tailnet against DNS rebinding (decision 7, deferred).

## Anti-goals

- Any change to `/v1/...` behavior for a valid request: auth, routing, streaming and logs stay as they are, apart from the limits below.
- A new Go dependency.
- Deciding what a request may reach from its client address or from a header an outsider could send.

## Decisions

"User" means the user decided it in conversation on 2026-11-09. Picking an offered option counts, and is noted as "picked". "Claude" means Claude's call while writing this document, for the user to check.

| # | Topic | Decision | Who |
|---|---|---|---|
| 1 | Scope | An API-only listener, the review's fixes listed under Behavior, and a sandboxed systemd unit. | User asked for the review. The fix list is Claude's. |
| 2 | Web UI | Not on the internet: no TOTP sign-in. It stays on the existing listener for the tailnet and LAN, unchanged. | User (picked "UI off the internet", after saying "I might not really use the ui anyway. I can check it on my phone. mainly just want to use the api routing feature") |
| 3 | How the UI stays off | A second listener, `api_listen`, serving only `/v1/...` and `/healthz`. `listen` keeps serving everything. | Claude |
| 4 | Deploy idle check | Unchanged: it reads `/ui/inflight` on the existing listener from the host. | Claude |
| 5 | Per-key route scoping | Not built. Recorded as a known limitation. | User (picked "Not needed") |
| 6 | Per-key disk and rate quota | Deferred to the next version. | Claude |
| 7 | DNS rebinding against the UI | Deferred: a browser on the LAN or tailnet visiting a hostile page could be used to read the UI. Fix with a `Host` check on UI routes. | Claude |
| 8 | Provider error bodies passed to clients | Unchanged. Not an issue: a leaked key can already spend the credit an error might reveal. | Claude |
| 9 | Waiting for the memory budget | Bounded at 25 s, then 503 `busy`, so annai (which gives up after 30 s by default) sees the 503 and fails over. | Claude |
| 10 | Connection-level limits | One wrapper around each server's whole handler sets the body deadline, closes connections whose body was left unread, and enforces write progress, so no route can miss them. | Claude |

## Behavior

### `api_listen`

```toml
[server]
listen = ":8470"                 # everything: API, /healthz and the web UI
# A second address serving only the API (/v1/...) and /healthz, never the web
# UI. Point an internet-facing proxy here. Unset: no second listener.
# api_listen = "127.0.0.1:8471"
```

- **Handler.** The `api_listen` server's handler is the CORS wrapper around a mux holding only the routes `server.Register` adds. UI routes are never registered on that mux. Any other path answers 404 there.
- **Settings.** Both servers get the same timeouts, wrapper and limits (below).
- **Reload.** Like `listen`, a reload can't change `api_listen`. A changed value is logged and ignored until a restart.
- **Startup and errors.** The startup log names both addresses. Either listener failing to bind is fatal at startup, as `listen` is today; that also covers `api_listen` naming the same address as `listen`.

### Error shape

New refusals keep the existing shape from `writeError`, `{"error": {"message": …, "type": …}}`, and add a `code` field. A client acts on the status and the `code`, never on the words.

### Limits that need no key

- **`/ui/logs?before=…&limit=N`:** `limit` is capped at 500, five of the UI's 100-entry pages. A larger value is served as 500.
- **Idle connections:**
  - `http.Server.IdleTimeout` is 120 s on both servers.
  - The UI polls every 3 s and annai sends turn after turn, so a connection in use is never idle that long. A connection someone opens and leaves goes away.
  - Not `http.Server.ReadTimeout`, which cancels long streams.
- **The connection wrapper (decision 10).** One handler wraps everything else on both servers.
  - **Body deadline.**
    - **What it does.** On a request with a body (`ContentLength != 0`; chunked is −1), reading that body must finish within 5 minutes. A 32 MB upload at 1 Mbit/s takes about 4.5 minutes.
    - **How it is set.** `http.NewResponseController(w).SetReadDeadline(now + 5 min)` before calling the handler. `net/http` clears it itself once the body reaches EOF (server.go `startBackgroundRead`, checked on Go 1.27.3 by a test that streamed past the deadline), so nothing clears it by hand.
    - **What it skips.** A request without a body gets no deadline: its background read is already running, and a deadline would cancel its context.
    - **Constraint on handlers.** They must keep reading bodies to EOF (`io.ReadAll`). A `json.Decoder` that stops at the end of the value never starts the background read, so a client that goes away would go unnoticed.
  - **Unread bodies.**
    - **What it does.** When the handler returns from a request whose body did not reach EOF, the wrapper sets the read deadline to now. A body wrapper records whether `Read` returned EOF.
    - **Only then.** When the body was read to EOF, `net/http`'s background read is running. A deadline it didn't set itself makes that read fail, and `handleReadErrorLocked` cancels the connection's base context, which every later request on that keep-alive connection inherits. Checked in Go 1.27.3's server.go `backgroundRead`, and reproduced by the plan's author.
    - **Why.** A refusal sent without reading the body (a 401, the mux's 404, `/healthz` hit with a `POST`, a CORS preflight) would otherwise hold its connection. `net/http` drains up to 256 KB of the unread body before writing the reply and when finishing the request, and waits on a body that never comes. Checked on Go 1.27.3: `Connection: close` alone did not help, and the deadline did. Keep-alive still works for requests that read their body.
  - **Write progress.**
    - **What it does.** Every response write must make progress. The wrapper hands the connection at most 64 KB at a time and sets a 60 s write deadline before each piece.
    - **Why 60 s.** A client that is reading takes each piece in well under a second. A write blocked for a minute means it has stopped reading. A slow client still gets a large log entry or audio file, because each piece has its own deadline.
    - **Requirements on the wrapped writer.** It keeps `Flush` working and has `Unwrap()`, so `ResponseController` reaches the real connection.
    - **Behavior.** A missed deadline ends the response the way a client disconnect does today. `net/http` clears write deadlines between keep-alive requests.
- **Declared size.** A `Content-Length` over the 32 MB cap answers 413, `code` `body_too_large`, before anything is read. Such a body could never succeed today: the JSON endpoints truncate it and answer 400, and multipart answers 400.

### Limits that need a client key

- **Parse once.** The body is decoded once. The preview and the token estimate come from that decoded value, or from the raw bytes in a single pass, never from decoding it again.
- **Memory budget.** It guards decoding, which is where the measured blow-up happens. It does not guard the upload: raw bytes cost about their own size and must actually arrive.
  - **Admission.** Once a request's body has been read in full and before it is decoded, the request waits for room in a global budget counted in the body's real length. That includes multipart transcription, after `ParseMultipartForm`. The request gives the room back when it ends. A client that stalls its upload holds no room.
  - **Wait limit.** A request admitted within 25 s goes ahead. Otherwise it answers 503, `code` `busy`, with `Retry-After: 5`. Its body has been read, so the client sees a clean 503.
    - **Why 25 s.** annai gives up after 30 s without a first event by default. At 25 s it gets the 503 and its own fallback moves on.
  - **Size:**
    - After the parse-once change, measure the peak heap of the worst 32 MB body. The review used one of about 10 million `{}` messages.
    - Set the budget so that a full budget of such bodies peaks under 3 GB, the memory this host usually has free. The budget is never below 32 MB, so a single maximum-size body always fits.
    - Write the measured peak and the arithmetic beside the constant.
    - Ordinary requests should never wait: KB chat bodies, MB images and audio.
- **Model names.** At most 256 bytes, with no control characters. The longest name in 2 months of logs is 57 bytes. Anything else gets a 400, `code` `invalid_model`, before routing, logging or the in-flight list. The message states the limit.
- **JSON values.** A request body holding more than 100,000 JSON values gets a 400, `code` `too_many_values`, right after decoding and before routing, logging or the in-flight list. The message states the limit.
  - **What a value is.** Each object, array, string, number, bool and null counts once, including the top-level value. Object keys don't count.
  - **Why.** Dialect conversion builds Go maps for each message, so a 32 MiB body of tiny messages took 13.6 GiB on a Gemini target in the plan's Task 6 measurement. The memory budget can't stop a single request, because it always admits one body.
  - **Why 100,000.** The largest of 154,614 real requests (October–November 2026) held 3,452 values (p99.9: 1,542), so the cap leaves about 29× headroom.
  - **Exemption.** Multipart transcription has no JSON body and isn't counted.
- **Gemini model path.** A model name containing a `..` path segment, `?`, `#` or `%` is not a valid Gemini target. That target fails as it would for an unknown model, and nothing is sent.
- **Blackout map.** `Fail` also removes expired entries, so names that are never asked about again don't stay.

### Other fixes

- **Reload and `client_keys`:**
  - `GeneratedClientKey` is kept across reloads, so the router always knows which key it generated.
  - A reload that leaves `client_keys` empty carries over only that generated key. Keys that came from the file are gone once the file no longer lists them.
  - When emptying a file's keys makes `build()` generate a new key, it is logged the way startup logs a generated key.
- **Log permissions:** new log files are 0600, new directories 0700, and the TSV index 0600. The router doesn't touch existing files; the deploy changes them once.
- **Journal lines:** client-supplied text (model names, anything else from a request) goes into log lines through `%q`, never `%s`.

### systemd unit

- **Sandboxing.** `--generate-systemd` adds these directives to the unit it writes, keeping each one the router and `ffmpeg` work with:
  - `NoNewPrivileges=yes`
  - `ProtectSystem=strict`
  - `ReadWritePaths=<log_dir>`
  - `PrivateTmp=yes`
  - `PrivateDevices=yes`
  - `ProtectKernelTunables=yes`
  - `ProtectKernelModules=yes`
  - `ProtectControlGroups=yes`
  - `RestrictNamespaces=yes`
  - `LockPersonality=yes`
  - `RestrictSUIDSGID=yes`
  - `CapabilityBoundingSet=` (none; the router's ports need no capability)
  - `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`
  - `SystemCallArchitectures=native`
  - `UMask=0077`
- **Mounts.** `RequiresMountsFor=` the binary, config and log directories, so a unit on a separate disk waits for it. The live unit has this today, added by hand.
- **Home directories.** `ProtectHome=yes` when nothing the service uses is under `/home` or `/root`:
  - the binary;
  - `WorkingDirectory`;
  - the config;
  - the log directory;
  - an absolute `ffmpeg` path.

  Otherwise `ProtectHome=read-only`, with `ReadWritePaths=` still covering the log directory. With `ProtectHome=yes`, a binary in a home directory fails with 203/EXEC.
- **Log directory path.**
  - `<log_dir>` is resolved from the config as an absolute path. A relative `log_dir` (the default is `./logs`) is resolved against the unit's `WorkingDirectory`, as the service will resolve it.
  - The generator creates the directory if it is missing. `ReadWritePaths=` on a missing path fails the unit with 226/NAMESPACE.
  - If the config can't be read, the generator says so and writes the unit without `ReadWritePaths`, with a comment saying what to add.
- **User.** `User=` and `Group=` stay the current login, as today. When that is root, the generator prints a line recommending a dedicated user, and the README says how to set one up:
  - the config readable by that user's group;
  - the log directory owned by that user.

## Constraints

- Go 1.26 per `go.mod`; this host builds with 1.27.3. Standard library plus `github.com/BurntSushi/toml`.
- Every existing test passes unchanged.
- The router is a public repository: no private host names, addresses or names of local services in code, docs or tests.

## Testing

- **`api_listen`:**
  - `/v1/...` and `/healthz` answer there.
  - `/ui`, `/ui/logs`, `/ui/logs/entry`, `/ui/inflight` and a made-up `/ui/x` all answer 404 there and 200 on `listen`.
  - A reload that changes it is ignored and logged.
- **Unread bodies:** a chunked `POST` with no body gets its reply, and the connection closes within a second, for each of:
  - no auth;
  - a bad key;
  - `/healthz`;
  - an unknown path;
  - a CORS preflight.

  A `POST` that read its body keeps its connection alive.
- **Body deadline** (the test shortens it):
  - A stalled body ends at the deadline.
  - A streaming response that runs past the deadline after its body was read isn't cancelled.
  - A multipart transcription upload is covered.
- **Write progress** (the test shortens the deadline):
  - A client that stops reading is cut off after the deadline, for a stream and for a large plain response.
  - A slow but steady reader gets the whole response.
- **Declared size:** a `Content-Length` over 32 MB answers 413 without reading.
- **Idle:** an idle keep-alive connection closes after `IdleTimeout`; the test shortens it.
- **Memory budget:**
  - Requests over the budget wait, then run.
  - A wait past the limit answers 503 `busy`.
  - A stalled upload holds no budget.
  - The measured peak of the worst body is recorded.
- **Model names and Gemini:**
  - A name over 256 bytes, or with a control character, is refused before it is logged or added to the in-flight list.
  - A Gemini target given `../x` sends nothing.
- **Blackout:** expired entries are removed on `Fail`.
- **Error shape:** each new refusal has `message`, `type` and `code`.
- **`limit`:** capped at 500.
- **Reload:**
  - Emptying `client_keys` drops the file's keys and logs the new generated key.
  - A generated key survives two reloads in a row.
- **Log permissions:** new files and directories get the new modes.
- **`--generate-systemd`:**
  - the sandboxing directives and `RequiresMountsFor`;
  - the `ProtectHome` choice, including a binary under a home directory;
  - `ReadWritePaths`, with a relative and with a missing log directory;
  - the root warning.
- **End to end:**
  - **Second router.** It runs at `:18470` under `systemd-run` with the same sandbox properties as the generated unit, using `api_listen` on another loopback port, a copy of the config, and its own log directory.
    - Through `api_listen`: a streamed chat, a Responses call, and a speech and a transcription that run `ffmpeg`. This proves DNS, provider calls and `ffmpeg` work inside the sandbox.
    - The UI is refused on `api_listen` and works on `listen`.
  - **Once deployed:** the same through the real proxy, with annai pointed at it.
    - Also check that the proxy doesn't keep idle connections to the router longer than its 120 s `IdleTimeout`. If it does, the proxy can send a `POST` on a connection the router just closed, and the client gets an occasional 502. If so, raise `IdleTimeout` above the proxy's limit.

## Deployment (host-specific steps stay out of this repository)

The deploy:
- sets `api_listen` to a loopback port;
- adds a proxy entry with TLS for it, using an existing certificate;
- runs the service as a dedicated user under the generated sandboxed unit, after making the existing logs that user's (0600 files, 0700 directories) and giving its group read access to the config;
- points annai at the new HTTPS address.

The deploy's idle check stays on `/ui/inflight` through `listen`.

## Left to the builder

- How the two servers share one shutdown path in `main.go`; today there is none.
- How the budget fits into the three body readers (JSON endpoints, speech, multipart transcription): one shared helper, used by each after its body is read.

## Docs to update

- **README:**
  - `api_listen`, and that the UI is never served there;
  - the new limits: model name length, the body deadline, and the `busy` answer;
  - running as a dedicated user;
  - a line saying any client key can use every route (decision 5);
  - a note that a company network inspecting HTTPS can read a client key used on it, so give such devices their own key.
- **`config.example.toml`:** `api_listen`, commented.
- **`docs/limitations.md`** (new):
  - decision 5 as a known limitation, approved 2026-11-09 (the user picked "Not needed");
  - decisions 6 and 7 as deferred.
- **`docs/lessons-full.md` and `docs/lessons.md`** (new): the web UI shared one listener with the API, so a proxy exposing the API exposed the UI too. The original design assumed the whole port was only ever reached locally.
- **`/mnt/optane/work/agent-rules/LESSONS.md`:** one line under "Safety and authority": serve an admin or data-viewing UI on its own listener from the first version, because a proxy or tunnel on the same host makes every visitor look local, so no address check can keep it private.
