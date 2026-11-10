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
- **What the operator sees:** nothing limits what one client key does over time. A leaked key can fill the log disk and spend provider credit at any rate the memory budget allows. Until then, removing the key from `client_keys` and reloading stops it.
- **Why:** this version bounds what one request can hold (body size and arrival time, JSON values, the memory budget, stalled replies). Quotas over time need per-key accounting and config, a piece of work of their own.
- **Revisit when:** the next version.

### DNS rebinding against the UI
- **Kind:** deferred, to the next version (decision 7).
- **What the operator sees:** a browser on the LAN or VPN that can reach `listen`, visiting a hostile page, could be used to read the UI: the page points its own host name at the router's address, and `/ui/logs`, `/ui/logs/entry` and `/ui/inflight` answer it with logged prompts and replies and the live requests.
- **Why:** the UI has no sign-in (decision 2) and is served only on `listen`, off the internet, so the attack needs a browser on that network and a page made for this router.
- **Revisit when:** the next version. The fix is a `Host` check on UI routes.
