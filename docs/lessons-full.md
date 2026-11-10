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
