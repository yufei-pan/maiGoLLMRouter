# Lessons

One line per lesson from changes to this repo's APIs, formats and agreed behaviour; read them all before changing one. Newest last, added in the same commit as the change, and kept under 20 KB. The story behind each line is in `docs/lessons-full.md`, under the same date and name.

- **2026-11-09 API-only listener**: register the web UI, and any route that shows logged prompts or live requests, only on `listen`'s mux, never on `api_listen`'s: a proxy or tunnel on the same host makes every visitor look local, so no address check can keep such a route private.
- **2026-11-09 client_keys reload**: on a reload, a credential list the operator emptied revokes what it held, and only a credential the program issued itself may carry over; log what replaces the list.
