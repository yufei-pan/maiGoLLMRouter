# Limitations

Known limitations the user approved, and work deferred to the next version: one `###` entry each.
Reviewers read this file first, and raise an entry again only with new evidence that it is wrong.

### Every client key reaches every route
- **Kind:** known limitation.
- **What the operator sees:** any key in `client_keys` can call every `/v1/...` route (chat, embeddings, Responses, models, speech, transcription) and every model, on both `listen` and `api_listen`. A key can't be limited to the routes or models one client needs.
- **Why:** not needed (decision 5 of `docs/superpowers/specs/2026-11-09-internet-exposure-design.md`). Scoping would add per-key route and model lists to the config and a check on every route.
- **Revisit when:** a key goes to a client that should reach only part of the API, such as another person or a third-party app.
- **Approved:** 2026-11-09; the user picked "Not needed".

### Per-key disk and rate quotas
- **Kind:** deferred, to the next version (decision 6).
- **What the operator sees:** nothing limits what one client key does over time. A leaked key can fill the log disk and spend provider credit at any rate the memory budget allows. It can also hold memory: bodies are read before they are admitted to the decode budget, so each stalled upload connection holds up to about 43 MiB of RAM (measured: 24 uploads stalled one byte short of 32 MiB held 1,064 MiB RSS) for up to 5 minutes, outside that budget. Until the quotas exist, removing the key from `client_keys` and reloading stops it, and the generated unit's `MemoryMax=6G` turns a memory runaway into a router restart.
- **Why:** this version bounds what one request can hold (body size and arrival time, JSON values, the memory budget, stalled replies). Quotas over time need per-key accounting and config, a piece of work of their own. The planned memory fix is a second budget over declared upload bytes, admitted before the body is read.
- **Revisit when:** the next version.

### DNS rebinding against the UI
- **Kind:** deferred, to the next version (decision 7).
- **What the operator sees:** a browser on the LAN or VPN that can reach `listen`, visiting a hostile page, could be used to read the UI: the page points its own host name at the router's address, and `/ui/logs`, `/ui/logs/entry` and `/ui/inflight` answer it with logged prompts and replies and the live requests.
- **Why:** the UI has no sign-in (decision 2) and is served only on `listen`, off the internet, so the attack needs a browser on that network and a page made for this router.
- **Revisit when:** the next version. The fix is a `Host` check on UI routes.

### Transcription conversion timeout answered as unreadable audio
- **Kind:** deferred, to the next version.
- **What the operator sees:** when stepfun transcription's ffmpeg conversion ends because the request's context did (a wait for a conversion slot, or a conversion that outlasts `audio_timeout`, under a flood of conversions), the client gets 400 "this provider can't read <format> audio" (`provider/transcription.go`), not a busy or timeout answer.
- **Why:** the correct answer depends on how the router treats an attempt that ran out of time (blackout, the next target), which this change doesn't touch.
- **Revisit when:** the next version.

### ffmpeg reached through a home directory
- **Kind:** deferred, to the next version.
- **What the operator sees:** `--generate-systemd` judges `ProtectHome=` from an absolute `ffmpeg` path only. A relative one with a slash (`ffmpeg = "bin/ffmpeg"`), or a bare `ffmpeg` whose file on systemd's PATH is a link into `/home` or `/root`, can get `ProtectHome=yes`; the service then can't start ffmpeg, so speech goes out unconverted and stepfun transcription of formats it can't read answers 400. Setting `ProtectHome=read-only` in the unit fixes it.
- **Why:** both layouts are unusual (the usual `/usr/bin/ffmpeg` is a real file), and the problem predates this change.
- **Revisit when:** the next version. The fix: judge ffmpeg where the service finds it (a relative path joined to `WorkingDirectory`, a bare name looked up on systemd's default PATH).
