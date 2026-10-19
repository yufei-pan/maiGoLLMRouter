# Audio: Speech, Transcription and Audio Input

**Date:** 2026-10-19
**Status:** Approved by the user on 2026-10-19; next step is the implementation plan
**Goal:** Let annai (annai.guide) speak, listen and send recorded speech to models through maiGoLLMRouter, with Gemini, OpenRouter and stepfun all working, and Gemini's speech compressed to Opus before it reaches the phone.

## Why

annai is a voice-first travel guide that runs in the browser. Its speech plugin calls OpenAI's audio API: `POST /audio/speech` for speech and `POST /audio/transcriptions` for transcription. Its "send audio to the model" option puts `input_audio` parts in Chat Completions requests. The router has no `/v1/audio/*` routes, and its Gemini translation drops `input_audio` without a word. So none of annai's voice features work through the router today.

The user wants all three to work through the router: speech output, transcription, and models that take audio input. At least OpenRouter, stepfun and Gemini must work. stepfun covers speech and transcription only (decision 3).

## What the providers do

Checked on 2026-10-19 against the providers' documentation, plus live calls with the router's own keys:

- **Gemini** (`google`, kind `gemini`):
  - **Speech:** `generateContent` on a TTS model (`gemini-3.8-flash-lite-tts` worked) with `generationConfig.responseModalities: ["AUDIO"]` and `speechConfig.voiceConfig.prebuiltVoiceConfig.voiceName` (e.g. `Kore`; 30 prebuilt voices). The reply came back as `candidates[0].content.parts[0].inlineData` with `mimeType: audio/wav`: a complete RIFF WAV, 24 kHz mono 16-bit. Older models returned raw `audio/L16;rate=24000`, so handle both.
  - **No transcription endpoint.** A chat model (`gemini-3.5-flash`) given the audio as `inlineData` plus "transcribe exactly" returned the exact words.
  - **Audio input:** AI Studio documents `audio/wav`, `mp3`, `aiff`, `aac`, `ogg` and `flac`; Vertex also lists `webm`, `mp4` and `m4a`.
- **OpenRouter** (`openrouter`, kind `openai`):
  - `POST /audio/speech` and `POST /audio/transcriptions` are OpenAI-compatible, including the multipart upload annai sends. Whisper transcription worked.
  - **Speech formats:** `mp3` or `pcm` only, and its Gemini voices only `pcm`: asking for `mp3` gave a 400, "Gemini TTS only supports response_format=pcm". Raw PCM is 24 kHz mono 16-bit, `Content-Type: audio/pcm`.
  - Chat models take `input_audio` in OpenAI's shape.
- **stepfun** (`stepfun`, kind `openai`, base `https://api.stepfun.ai/step_plan/v1`, a Step Plan key):
  - **Speech:** `POST {base}/audio/speech` is OpenAI-shaped and worked with `stepaudio-2.5-tts`, voice `zixinnansheng`, returning `audio/wav`. Two fields differ from OpenAI: style is `instruction` (singular), and `sample_rate` selects the rate. Formats are `wav`, `mp3`, `flac`, `opus` and `pcm`. Input is at most 1,000 characters.
  - **Transcription:** `POST {base}/audio/transcriptions` gives a **404**. Transcription is `POST {base}/audio/asr/sse` (headers `Content-Type: application/json`, `Accept: text/event-stream`). Body: `{"audio": {"data": <base64>, "input": {"transcription": {"model", "language", "enable_itn"}, "format": {"type": "wav"|"mp3"|"ogg"|"m4a"|"pcm", ...}}}}`. The reply is SSE: `transcript.text.delta`, then `transcript.text.done` with the full `text`; failures arrive as an `error` event. It worked with `stepaudio-2.5-asr`.
  - **No audio-input chat on this key.** The plan's models are `step-3.5-flash`, `step-3.5-flash-2603`, `step-3.7-flash`, `step-5-preview`, `stepaudio-2.5-asr` and `stepaudio-2.5-tts`. `stepaudio-2.5-chat` isn't included, and the chat models reply "doesn't support audio input".

## What annai does today

Read from `~/work/annai` on 2026-10-19. The user says annai's audio pipeline can still change, so these are today's facts, not constraints:

- **Speech:** `plugins-official/speech-openai/index.js` sends JSON `{model, input, voice, response_format, instructions?, speed?}` with `response_format: "mp3"` by default. It plays whatever audio type the reply's `Content-Type` says, through an `<audio>` element.
- **Transcription:** a multipart upload: `file` (16 kHz mono WAV from its recorder), `model`, `response_format: "json"`, and optionally `language` and `prompt`. It reads `{"text": …}`.
- **Audio input:** the Chat plugin sends `{"type": "input_audio", "input_audio": {"data": <base64>, "format": "wav"}}`.
- **Opus playback:** Safari plays Ogg Opus through `<audio>` from Safari 18.4 (iOS 18.4) on. Older iPhones can't, so such a client should ask for `mp3` or `aac`.

## Goals

1. `POST /v1/audio/speech` returns speech in the format the client asks for (`opus`, `mp3`, `aac`, `flac`, `wav`, `pcm`) from Gemini, OpenRouter and stepfun targets.
2. `POST /v1/audio/transcriptions` returns OpenAI's `{"text": …}` from Gemini, OpenRouter and stepfun targets.
3. Chat requests with `input_audio` work through Gemini targets, and keep passing through to OpenRouter.
4. Speech from Gemini, which only produces WAV or raw PCM, reaches the client compressed as Ogg Opus when the client asks for `opus`, using `ffmpeg` on the host.
5. Both audio routes use the router's usual model routes, keys, blackout and fallback, with a voice set per target so a fallback still speaks in a chosen voice.

## Non-goals

- Streaming speech within one request. annai asks for speech a sentence at a time, so each reply is sent whole.
- Realtime and WebSocket voice APIs, voice cloning, and multi-speaker speech.
- Audio-input chat on stepfun (its plan has no such model), and on the Responses route.
- Audio on Anthropic targets: they are skipped for audio requests.
- `/v1/files`, and listing voices.

## Anti-goals

- Any change to chat, embeddings or Responses behavior, apart from Gemini now receiving `input_audio`, and audio being left out of what the log stores (decision 20).
- A new Go dependency. `ffmpeg` is an external program the user chose, run with `os/exec`.
- Failing a speech request only because `ffmpeg` is missing or a conversion failed. The client gets the audio as the provider sent it, labeled with its true type.
- Audio bytes in the logs.

## Decisions

"User" means the user decided it. "Claude, accepted" means Claude proposed it and the user accepted it in conversation on 2026-10-19. "Claude, approved with the document" means Claude's call, made while writing this document and approved by the user with it.

| # | Topic | Decision | Who |
|---|---|---|---|
| 1 | Scope | Speech, transcription and audio input to chat, through the router. | User |
| 2 | Providers | OpenRouter, stepfun and Gemini must work. | User |
| 3 | stepfun | Speech and transcription only; no audio-input chat. | User |
| 4 | Compression | The router compresses speech to Opus with `ffmpeg` on the host. | User |
| 5 | Formats | The router returns whatever `response_format` the client asks for, converting with `ffmpeg` when the provider's reply differs. | Claude, accepted |
| 6 | Routing | Audio uses the same model routes, keys, blackout and fallback as chat. | Claude, accepted |
| 7 | Per-target speech settings | A route can set, per target, the voice to use and the format to ask the provider for. | Claude, accepted |
| 8 | stepfun's dialect | A provider option `audio = "stepfun"` marks stepfun's audio API. | Claude, accepted |
| 9 | Transcription input | Accept OpenAI's multipart upload and OpenRouter's base64 JSON. | Claude, accepted |
| 10 | `ffmpeg` missing | Send the audio unconverted with its true type and log a warning; never fail the request. | Claude, accepted |
| 11 | Logs | Audio is logged as size and type, never as bytes. | Claude, accepted |
| 12 | Gemini style | `instructions` go to Gemini only if a live check shows they shape the speech without being read aloud; otherwise they are dropped for Gemini. | Claude, accepted |
| 13 | Voice precedence | A target's configured voice wins over the client's `voice`; with none configured, the client's passes through. | Claude, approved with the document |
| 14 | Default speech format | `mp3` when the client sends no `response_format` (OpenAI's default). | Claude, approved with the document |
| 15 | `pcm` | Means 24 kHz, mono, 16-bit signed little-endian, wherever the router asks for, reads or produces it. When asking stepfun for `pcm`, the router also sends `sample_rate: 24000`. | Claude, approved with the document |
| 16 | Opus encoding | Ogg container, `libopus`, mono, 32 kbps, `-application voip`, `Content-Type: audio/ogg`. The other encodings are in the table below. | Claude, approved with the document |
| 17 | Formats a provider can't read | When the client's audio is in a format the target can't take (e.g. `webm` to stepfun), the router converts it with `ffmpeg` to Ogg Opus first. | Claude, approved with the document |
| 18 | Transcription `response_format` | `json` (default) and `text` everywhere; `verbose_json` passes through to OpenAI-kind targets, and other targets answer plain `json`. | Claude, approved with the document |
| 19 | Success | Speech: a 2xx reply carrying non-empty audio. Transcription: a 2xx reply with a `text` string, which may be empty (silence). Anything else is `bad_output`: next key, no blackout. | Claude, approved with the document |
| 20 | Audio in chat logs | Logged chat and Responses request bodies have each `input_audio`'s `data` replaced by `"<N bytes audio/wav>"`. Previews and token estimates are computed from the request as today. annai resends earlier clips every turn, so otherwise each turn would store every earlier recording again. MaiBot sends no audio, so its logs don't change. | Claude, approved with the document (raised by the builder check) |
| 21 | Format asked of OpenAI-kind providers | `pcm` (24 kHz), unless the target's `format` says otherwise; the router converts it to the client's format. Every OpenAI-kind TTS here takes `pcm` (OpenAI, OpenRouter, stepfun), but not all take the same compressed formats: OpenRouter's Gemini voices take only `pcm`, and stepfun has no `aac`. | Claude, approved with the document (raised by the builder check) |
| 22 | Unknown Gemini voice | A voice that isn't one of Gemini's prebuilt names (e.g. annai's default `marin`) becomes `Kore`. | Claude, approved with the document (raised by the builder check) |
| 23 | Audio time limit | `[server] audio_timeout`, default `10s`, limits each audio attempt (speech and transcription), taking the shorter of it and the provider's `timeout`. annai gives up on speech after 10 s and on transcription after 15 s by default, and provider timeouts here are 200–3600 s, so without it a hung provider is never failed over in time. | Claude, approved with the document (raised by the builder check) |
| 24 | stepfun `error` events | An error caused by the request itself (empty, too short or unreadable audio, an unsupported format) counts like an HTTP 400: next target, no blackout. Any other error event is a provider error, blacked out as usual. The builder checks the event's shape with a live call on an empty and a 0.1 s clip. | Claude, approved with the document (raised by the builder check) |

## Configuration

```toml
[server]
# Path to ffmpeg, used to convert speech (e.g. to Opus). Default "ffmpeg", found on PATH.
# ffmpeg = "/usr/bin/ffmpeg"
# Time limit for each speech or transcription attempt (the shorter of this and the provider's timeout).
# audio_timeout = "10s"

[[provider]]
name = "stepfun"
kind = "openai"
base_url = "https://api.stepfun.ai/step_plan/v1"
audio = "stepfun"        # speech and transcription use stepfun's audio API; default: OpenAI's

[model."shiori-voice"]
targets = ["google/gemini-3.8-flash-lite-tts", "openrouter/google/gemini-3.8-flash-lite-tts", "stepfun/stepaudio-2.5-tts"]

[model."shiori-voice".speech."google/gemini-3.8-flash-lite-tts"]
voice = "Kore"

[model."shiori-voice".speech."openrouter/google/gemini-3.8-flash-lite-tts"]
voice = "Kore"
# format = "mp3"         # optional: what to ask this provider for instead of pcm; converted to the client's format

[model."shiori-voice".speech."stepfun/stepaudio-2.5-tts"]
voice = "zixinnansheng"

[model."listen"]
targets = ["stepfun/stepaudio-2.5-asr", "openrouter/openai/whisper-1", "google/gemini-3.5-flash"]
```

- **`[server] ffmpeg`:** optional; reloads with the config.
- **`[server] audio_timeout`:** optional, default `10s` (decision 23); reloads with the config.
- **Provider `audio`:** optional, `"openai"` (the default) or `"stepfun"`. Only valid on `kind = "openai"`.
- **`[model."…".speech."<provider/model>"]`:** optional per-target settings, read only by `/v1/audio/speech`. The keys are expanded `provider/model` targets. `voice` and `format` are both optional; `format` is one of the speech formats below and replaces the `pcm` the router would otherwise ask an OpenAI-kind provider for (decision 21). It's meaningless for Gemini, which always returns WAV. A key that names no target of the route is a config error. When a route's targets come from another named model (a nested route), the inner route's settings apply, and the outer route's settings for the same target win (Claude, approved with the document).
- **A request naming `provider/model` directly** has no per-target settings: the client's `voice` and `response_format` pass through.

## Behavior

### `POST /v1/audio/speech`

**Request:** JSON in OpenAI's shape: `model`, `input`, `voice`, `response_format`, `speed`, `instructions`. Other fields pass through to OpenAI-kind providers, as chat extras do. `stream_format` is removed (non-goal).

**Per target:**
- **Voice:** the target's configured voice, else the client's `voice`.
- **Format asked of the provider:** OpenAI-kind targets are asked for the target's configured `format`, else `pcm` (decision 21). Gemini always returns WAV.
- **Format for the client:** the client's `response_format`, else `mp3` (decision 14).

**By dialect:**

| Target | Request | Reply |
|---|---|---|
| OpenAI-kind, `audio = "openai"` | `POST {base}/audio/speech` with the body, model pinned, voice and `response_format` as above | the audio bytes; their type is the reply's `Content-Type` |
| OpenAI-kind, `audio = "stepfun"` | the same route, with `instructions` renamed to `instruction`, and `sample_rate: 24000` when asking for `pcm` | the same |
| Gemini | `POST {base}/models/{model}:generateContent` with `contents: [{role: "user", parts: [{text: input}]}]` and `generationConfig: {responseModalities: ["AUDIO"], speechConfig: {voiceConfig: {prebuiltVoiceConfig: {voiceName}}}}` (voice `Kore` when none is given or the given one isn't a Gemini voice, decision 22). `speed` is dropped; `instructions` per decision 12. | `inlineData` from the first part: `audio/wav`, or `audio/L16` / `audio/pcm` with its `rate` (default 24000) |
| Anthropic | skipped as incompatible, like a non-portable Responses body | — |

**Converting to the client's format.** The router compares the reply's type with the format the client asked for. If they differ, it converts with `ffmpeg`, reading `pcm` and `L16` as raw `s16le` mono at the stated rate (24 kHz by default). Otherwise it sends the reply unchanged.

| Client format | `ffmpeg` output | `Content-Type` |
|---|---|---|
| `opus` | `-c:a libopus -b:a 32k -ac 1 -application voip -f ogg` | `audio/ogg` |
| `mp3` | `-c:a libmp3lame -b:a 64k -ac 1 -f mp3` | `audio/mpeg` |
| `aac` | `-c:a aac -b:a 48k -ac 1 -f adts` | `audio/aac` |
| `flac` | `-c:a flac -f flac` | `audio/flac` |
| `wav` | `-c:a pcm_s16le -f wav` | `audio/wav` |
| `pcm` | `-f s16le -ar 24000 -ac 1` | `audio/pcm` |

- **Running `ffmpeg`:** fed through stdin and stdout with the request's context, so a client that goes away stops it.
- **On failure:** if `ffmpeg` is missing, fails or returns nothing, the client gets the provider's audio as it was. A raw `pcm` / `L16` reply is first wrapped in a WAV header (in Go), so it is always playable. `Content-Type` is always the true type, and the log records the reason as a warning (decision 10).
- **The reply:** `200`, the audio bytes, `Content-Type` as above.

### `POST /v1/audio/transcriptions`

**Request,** in either of two shapes:
- **Multipart** (OpenAI's): `file`, `model`, and optionally `language`, `prompt`, `response_format`, `temperature`.
- **JSON** (OpenRouter's): `{"model", "input_audio": {"data": <base64>, "format"}, "language"?, "response_format"?, "temperature"?}`.

**The audio's format** comes from `input_audio.format`, else the upload's file name extension, else its part `Content-Type`, else `wav`.

**By dialect:**

| Target | Request | Reply read as |
|---|---|---|
| OpenAI-kind, `audio = "openai"` | multipart `POST {base}/audio/transcriptions`: `file`, `model` (pinned), `language`, `prompt`, `response_format`, `temperature` | the provider's JSON (or text) |
| OpenAI-kind, `audio = "stepfun"` | `POST {base}/audio/asr/sse` with `{"audio": {"data": b64, "input": {"transcription": {"model": <target model>, "language"?: <language>, "enable_itn": true}, "format": {"type": <wav/mp3/ogg/m4a>}}}}`; any other format is converted first (decision 17) | the SSE stream: `text` from `transcript.text.done`; an `error` event is a provider error, or counts like an HTTP 400 when the request caused it (decision 24); ending without `done` is a provider error |
| Gemini | `generateContent` on the target model, with parts `[{text: <instruction>}, {inlineData: {mimeType, data}}]` and `temperature` (default 0) | the first candidate's text, trimmed |
| Anthropic | skipped as incompatible | — |

**Gemini's instruction** (Claude, approved with the document): "Transcribe the speech in this audio exactly, in the language spoken. Output only the transcript, with no notes, labels or timestamps. If there is no speech, output nothing." Then, when given, "The language is probably <language>." and "Context: <prompt>". The builder should turn off or minimize thinking where the model allows it, since transcription latency matters for a voice app.

**The reply:**
- `response_format` `json`: `{"text": <text>}` as `application/json`.
- `text`: the text as `text/plain`.
- `verbose_json` on an OpenAI-kind target: the provider's body as it came.

### Audio input to chat (Gemini)

`buildGeminiChatBody`'s user parts gain `input_audio`: `{"type": "input_audio", "input_audio": {"data", "format"}}` becomes `{"inlineData": {"mimeType": <mime>, "data": <base64>}}`. A `data:` URI in `data` is reduced to its base64 and its MIME type.

| `format` | Gemini MIME |
|---|---|
| `wav` | `audio/wav` |
| `mp3` | `audio/mp3` |
| `ogg`, `opus` | `audio/ogg` |
| `webm` | `audio/webm` |
| `flac` | `audio/flac` |
| `aac` | `audio/aac` |
| `m4a`, `mp4` | `audio/mp4` |
| `aiff` | `audio/aiff` |

This applies to buffered and streamed chat alike: the streaming path builds its body with the same function. OpenAI-kind providers already pass `input_audio` through unchanged.

### Routing, failures and logs

- **Routing:** both audio routes resolve model names, rotate keys, black out failing keys and fall over to fallback keys and targets exactly as buffered chat does. Exhaustion answers with today's JSON error and status rules.
- **Time limit:** each audio attempt is limited by `audio_timeout` (decision 23). Running out is a provider error, blacked out like a buffered timeout today.
- **Outcomes:** the usual ones. Decision 19 defines a good reply. A Gemini speech reply without audio (a text reply, or a finish without parts) is `bad_output`. A Gemini safety block is `prohibited`, as for chat.
- **Request size:** up to 32 MB, as for chat. Both shapes of transcription request count toward it.
- **Logs (decision 11):**
  - **Request:** the JSON fields, with the audio replaced by `"<N bytes audio/wav>"`.
  - **Response:** `{"audio": "<N bytes audio/ogg>"}` for speech, the JSON for transcription.
  - **Attempts:** keep their usual fields; a downstream audio body is never stored.
  - **Chat and Responses requests** carrying `input_audio`: logged with each clip's `data` replaced by its size and type (decision 20).
  - **In-flight preview:** speech shows the start of `input`; transcription shows `audio <size> <format>`.

## Constraints

- Go 1.26, standard library plus `github.com/BurntSushi/toml`; `ffmpeg` through `os/exec` only.
- **The host's `ffmpeg`** is 7.1.5 at `/usr/bin/ffmpeg`, with `libopus`, `libmp3lame` and `aac`, and muxers for `ogg`, `mp3`, `adts`, `flac` and `wav`.
- **Tests that need `ffmpeg`** skip themselves when it isn't installed. CI (`.github/workflows/release.yml`) only builds releases and doesn't run tests, but other machines may lack `ffmpeg`.
- **Unchanged:** chat, embeddings and Responses behavior, apart from Gemini `input_audio`. Every existing test passes unchanged.
- **The `[server]` table** reloads as today. The new provider `audio` option and per-target `speech` tables reload with the routes.

## Testing

- **Request builders and reply readers** for each dialect, against `httptest` servers:
  - Gemini speech (`audio/wav` and `audio/L16` replies);
  - Gemini transcription;
  - stepfun speech (`instruction`, `sample_rate`);
  - stepfun SSE transcription: done, error, and a stream that ends early;
  - OpenAI-kind speech and multipart transcription passing through with the model pinned.
- **Conversion,** with `ffmpeg`, skipped when absent: WAV to Ogg Opus (checked with `ffprobe` or the Ogg magic `OggS` and an `OpusHead` page); PCM 24 kHz to Opus; to mp3 / aac / wav / pcm; and the fallbacks when `ffmpeg` is missing (point the path at a non-existent file) or fails.
- **Router:**
  - a speech route with per-target voices, failing over from Gemini to stepfun, where each target receives its own voice;
  - transcription failing over when stepfun returns an `error` event;
  - Anthropic targets skipped;
  - a `speech` key naming no target is rejected at config load.
- **Server:**
  - both routes with auth;
  - a multipart and a JSON transcription;
  - `Content-Type` of the speech reply;
  - logs without audio bytes.
- **Gemini chat with `input_audio`:** the body carries `inlineData` with the right MIME type, for buffered and streamed requests.
- **Decisions 20–24:**
  - a logged chat request with `input_audio` has no audio bytes, while its preview is unchanged;
  - an OpenAI-kind speech target with no `format` is asked for `pcm`, and the client gets its own format;
  - `marin` reaches Gemini as `Kore`;
  - a target slower than `audio_timeout` fails over;
  - a stepfun `error` event caused by the clip moves on without blackout.
- **End to end, with real keys,** on a second router at `:18470`, as for streaming:
  - speech from each of the three providers as `opus` (the reply is Ogg Opus);
  - transcription of a known clip through each of the three;
  - chat with `input_audio` through Gemini and OpenRouter;
  - and the replies fed to annai's own `speech-openai` readers (`readSpeech`, `readTranscript`).

## Changes needed in annai

annai is built in another session (`~/work/annai`). These are not router work:

- **Speech format:** ask for `response_format: "opus"` (Ogg Opus) where the browser plays it, i.e. Safari 18.4 and later and other modern browsers; elsewhere ask for `mp3` or `aac`.
- **Speech model:** use the router's speech route as the model id (e.g. `shiori-voice`). The voice can be left empty when the route sets one per target.
- **Recordings may be compressed:** the router passes Ogg, MP3 and M4A/MP4 through, and converts what a target can't read. Safari's `MediaRecorder` records MP4/AAC, not Ogg.
- **Send the transcription `language`** once annai's language support (a planned feature) lands. stepfun assumes English when it isn't given, and returns a wrong transcript of Chinese or Japanese that still counts as a success. Gemini and Whisper detect the language themselves, so until then a transcription route should try them before stepfun.

## Left to the builder

- The internal shape of the audio entry points (new `provider.Operation`s, or separate `CallSpeech` / `CallTranscribe` functions), as long as routing reuses `run` / `judge` / `settle`.
- The exact `ffmpeg` flags beyond those fixed above, and how `ffmpeg`'s stderr is logged.
- **Verify against a live call:**
  - whether Gemini accepts `audio/webm` and `audio/mp4` inline (convert them first if not);
  - whether Gemini speech `instructions` can go in `systemInstruction` without being read aloud (decision 12);
  - which thinking setting keeps Gemini transcription fast.

## Docs to update

- **README:**
  - the two audio routes and their formats;
  - the `ffmpeg` requirement and the `[server] ffmpeg` setting;
  - the provider `audio` option;
  - per-target `speech` settings;
  - Gemini `input_audio`;
  - a note that stepfun's plan has no audio-input chat model.
- **`config.example.toml`:** the new settings, commented.
