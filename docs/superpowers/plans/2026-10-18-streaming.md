# Streaming (SSE) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stream Chat Completions and Responses replies to clients that send `"stream": true`. Each request commits to one attempt at its first output and keeps today's routing until then.

**Architecture:**
- The provider package opens a downstream call as a `*provider.Stream`: a pull iterator of client-format SSE frames (`provider.Frame`), with a running `StreamState` (output seen, finish reason, rebuilt body).
- Every source produces this one type:
  - dialects: OpenAI Chat pass-through, OpenAI Responses pass-through, Gemini translation;
  - wrappers: the Chat-to-Responses translator, and the buffered and refusal streams.
- The router's new `ExecuteStream` reuses `Execute`'s key and target loops. It reads frames until the first output, commits through a `StreamSink` that the server implements, then pumps the rest.

**Tech Stack:** Go 1.26, standard library (plus the existing `github.com/BurntSushi/toml`), tests with `net/http/httptest`.

**Spec:** `docs/superpowers/specs/2026-10-18-streaming-design.md`. "D*n*" below means the spec's decision *n*.

## Global Constraints

- Go 1.26; standard library plus `github.com/BurntSushi/toml` only.
- **Non-streamed requests are unchanged.** `Router.Execute` keeps stripping `stream` / `stream_options`, and every existing test stays green without edits.
- **What streams:** a request streams only when the op is `OpChat` or `OpResponses` and `body["stream"] == true` (a JSON boolean).
- **No rewriting:** streamed requests never run `coerce_trailing_assistant`.
- **Headers at commit:** `200`, `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `X-Accel-Buffering: no`. Flush after every frame.
- **Stream endings:** Chat ends with `data: [DONE]`. Responses ends with `response.completed` or `response.incomplete`, with no `[DONE]`. Neither comes after an error.
- **Router error frames:**
  - Chat: `{"error":{"message":…,"type":"upstream_error","code":502}}` (`504` for a timeout), with no `event:` line.
  - Responses: `event: error` and `{"type":"error","code":"server_error","message":…,"sequence_number":N}`.
- **Error code → status** (D19):
  - a number from 400 to 599 → itself;
  - `rate_limit_exceeded`, `slow_down`, `insufficient_quota` → 429;
  - `server_is_overloaded` → 503;
  - `invalid_api_key` → 401;
  - `model_not_found` → 404;
  - `context_length_exceeded` → 400;
  - anything else → 502.
- **Fixed failure replies:**
  - Empty stream → 502 `{"error":{"message":"the provider's stream ended without output","type":"upstream_error"}}`.
  - First-output timeout → 504 `{"error":{"message":"no output from the provider in time","type":"upstream_error"}}`.
- **Timeouts for streams** (D13): a provider's `timeout` limits the wait for the first output, then the longest silence between downstream reads (comments count as activity). There is no total limit. Streams use HTTP clients without `Client.Timeout`.
- **Gemini signatures** (D11, D12, D23):
  - Cache: 4096 entries, 24 h, keyed by tool-call id, and given back only to the same `provider/model` that signed.
  - On a miss, send the placeholder `skip_thought_signature_validator`. Only when the model name (without `models/`) starts with `gemini-`, and only on the first `functionCall` part of a model content.
- **Reloads:** every streamed request loads `routerState` once (as `Execute` does since `5410de8`). A reload never waits for a stream.
- **Failed-attempt logs:** `StreamState.Raw` keeps at most the first 64 KiB of downstream bytes (plan choice).

## Review Focus

1. **A client that disconnects mid-stream.** The router stops reading the downstream within a second, closes its body, logs `canceled`, and blacks out nothing. Test: Task 9, `TestStreamClientGoneStopsDownstream`.
2. **A very long SSE line**, such as a 1 MiB tool-argument or base64 delta. It is read whole, never failing on a scanner token limit. Test: Task 1, `TestSSEReaderLongLine`.
3. **A downstream that closes after `finish_reason` without `[DONE]`** (several local servers do). That is a clean end, and the router adds `[DONE]` itself. Test: Task 3, `TestOpenAIChatStreamAddsMissingDone`.
4. **A `data:` line that isn't JSON** in an OpenAI-kind stream. It is skipped: neither forwarded nor fatal. Test: Task 3, `TestOpenAIChatStreamSkipsNonJSONData`.
5. **Parallel tool calls whose fragments interleave by `index`**, with `id` and `name` only on each call's first fragment. They are merged per index in the rebuilt body. Test: Task 2, `TestChatAccumulatorInterleavedToolCalls`.

---

### Task 1: Reading and writing server-sent events

**Files:**
- Create: `provider/sse.go`
- Test: `provider/sse_test.go`

**Interfaces:**
- Produces:
  - `type Frame struct { Event string; Data []byte }`: one event for the client. `Event` is empty for Chat chunks.
  - `func (f Frame) Encode() []byte`
  - `type sseEvent struct { Event, Data string }`
  - `func newSSEReader(r io.Reader) *sseReader`
  - `func (s *sseReader) Next() (sseEvent, error)`: `io.EOF` when the body ends.

- [ ] **Step 1: Write the failing tests**

```go
func TestSSEReaderEvents(t *testing.T) {
	in := ": keep-alive\r\nevent: response.created\r\ndata: {\"a\":1}\r\n\r\ndata: x\ndata: y\n\ndata: [DONE]"
	r := newSSEReader(strings.NewReader(in))
	for _, want := range []sseEvent{{"response.created", `{"a":1}`}, {"", "x\ny"}, {"", "[DONE]"}} {
		got, err := r.Next()
		if err != nil || got != want {
			t.Fatalf("got %+v, %v; want %+v", got, err, want)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
}
```

Also add these tests:
- **`TestSSEReaderLongLine`:** one `data:` line of 1 MiB comes back whole.
- **`TestSSEReaderSplitReads`:** the same input as `TestSSEReaderEvents`, read through `iotest.OneByteReader`, gives the same events.
- **`TestFrameEncode`:**
  - `Frame{Data: []byte(`{"a":1}`)}` encodes to `"data: {\"a\":1}\n\n"`;
  - `Frame{Event: "response.completed", Data: []byte("{}")}` encodes to `"event: response.completed\ndata: {}\n\n"`;
  - `Frame{Data: []byte("x\ny")}` encodes to `"data: x\ndata: y\n\n"`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./provider -run 'TestSSEReader|TestFrameEncode' -count=1`
Expected: FAIL, with undefined `newSSEReader` / `Frame`.

- [ ] **Step 3: Implement `provider/sse.go`**
  - Use `bufio.Reader.ReadString('\n')` and trim a trailing `\r`. Don't use `bufio.Scanner`, which has a token limit.
  - Lines starting with `:` are comments.
  - The data lines of one event are joined with `\n`.
  - An event without an `event:` line has an empty `Event`.
  - A final event without its blank line is still returned before `io.EOF`.

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./provider -run 'TestSSEReader|TestFrameEncode' -count=1`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add provider/sse.go provider/sse_test.go
git commit -m "Read and write server-sent events for streaming."
```

---

### Task 2: The stream type, stream errors, and the Chat accumulator

**Files:**
- Create: `provider/stream.go`, `provider/chat_accum.go`
- Test: `provider/stream_test.go`, `provider/chat_accum_test.go`

**Interfaces:**
- Consumes: `Frame` (Task 1); `matchesProhibited`, `isProhibitedContent` (existing).
- Produces:

```go
type Stream struct {
	HTTPStatus    int
	OutboundURL   string
	OutboundBody  []byte
	LearnChatOnly bool
	src           streamSource
}
type streamSource interface {
	next() (Frame, error)
	state() StreamState
	errorFrame(status int, message string) Frame
	close()
}
func (s *Stream) Next() (Frame, error)                         // io.EOF after the final frame; *StreamError on failure
func (s *Stream) State() StreamState
func (s *Stream) ErrorFrame(status int, message string) Frame // the router's own error event, in this stream's format
func (s *Stream) Close()

type StreamState struct {
	Output       bool   // real output has been produced: the commit point
	FinishReason string // normalized, as Response.FinishReason
	HasContent   bool
	Prohibited   bool
	Body         []byte // the reply so far as one complete JSON body (chat.completion or response object)
	Raw          []byte // downstream bytes read, first 64 KiB
}
type StreamError struct {
	Status     int    // status for the client if this failure ends the run (D19)
	Body       []byte // {"error": <object>}
	Frame      *Frame // the downstream's own error event in the client's format; nil when the router writes its own
	Prohibited bool
	Timeout    bool
	Err        error
}
func (e *StreamError) Error() string
func errorStatus(code any) int
func streamErrorFromObject(obj map[string]any, frame *Frame) *StreamError
func chatErrorFrame(status int, message string) Frame
func responsesErrorFrame(seq int, message string) Frame
func WantsUsage(body map[string]any) bool // stream_options.include_usage == true
type rawLog struct{ b []byte }            // write(p []byte) keeps the first 64 KiB
func newChatAccumulator(model string) *chatAccumulator
func (a *chatAccumulator) add(chunk map[string]any) (output bool)
func (a *chatAccumulator) state() StreamState // Body is a chat.completion; Raw left empty
```

- [ ] **Step 1: Write the failing tests**
  - **`TestErrorStatus`**, table-driven:

    | Input | Status |
    |---|---|
    | `float64(429)` | 429 |
    | `float64(600)` | 502 |
    | `"429"` | 502 (strings are codes, not numbers) |
    | `"rate_limit_exceeded"` | 429 |
    | `"slow_down"` | 429 |
    | `"insufficient_quota"` | 429 |
    | `"server_is_overloaded"` | 503 |
    | `"invalid_api_key"` | 401 |
    | `"model_not_found"` | 404 |
    | `"context_length_exceeded"` | 400 |
    | `"server_error"` | 502 |
    | `nil` | 502 |

  - **`TestStreamErrorFromObject`:** for `obj = {"message":"Rate limit","code":429}`:
    - `Status == 429`;
    - `json.Unmarshal(Body)` equals `{"error": obj}`;
    - `Frame` is passed through;
    - and for `obj = {"message":"PROHIBITED_CONTENT"}`, `Prohibited == true`.
  - **`TestErrorFrames`:**
    - `chatErrorFrame(504, "m")` has `Event == ""` and data `{"error":{"message":"m","type":"upstream_error","code":504}}`;
    - `responsesErrorFrame(7, "m")` has `Event == "error"` and data `{"type":"error","code":"server_error","message":"m","sequence_number":7}`.
  - **`TestChatAccumulatorTextAndUsage`:** chunks
    - a role-only `delta` → `add` returns false;
    - `"Hel"` → true;
    - `"lo"`;
    - a finish chunk with `finish_reason:"stop"`;
    - a chunk with `choices: []` and `usage: {prompt_tokens:5, completion_tokens:2, total_tokens:7}`.

    State: `Output`, `HasContent`, `FinishReason == "stop"`, Body `choices[0].message.content == "Hello"`, `usage.prompt_tokens == 5`.
  - **`TestChatAccumulatorInterleavedToolCalls`** (Review Focus 5):
    - fragments `{index:0,id:"c0",function:{name:"a",arguments:"{\"x\""}}`, `{index:1,id:"c1",function:{name:"b",arguments:"{}"}}`, `{index:0,function:{arguments:":1}"}}`;
    - Body tool calls `[0]` = `c0` / `a` / `{"x":1}` and `[1]` = `c1` / `b` / `{}`;
    - a `thought_signature` on a fragment is kept on that call.
  - **`TestChatAccumulatorReasoningCountsAsOutput`:** `delta.reasoning_content:"think"` gives output true, Body `message.reasoning_content == "think"`, and `HasContent == false`. `delta.reasoning` counts the same way.
  - **`TestChatAccumulatorContentFilterIsProhibited`:** a finish of `content_filter` with no output gives `Prohibited == true` and `Output == false`.
  - **`TestWantsUsage`:** true only for `stream_options.include_usage == true`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./provider -run 'TestErrorStatus|TestStreamErrorFromObject|TestErrorFrames|TestChatAccumulator|TestWantsUsage' -count=1`. Expected: FAIL (undefined).

- [ ] **Step 3: Implement `provider/stream.go` and `provider/chat_accum.go`**
  - **Output:** a non-empty `content`, `reasoning_content`, `reasoning` or `refusal` delta, or any `tool_calls` entry.
  - **Body:** a `chat.completion` with choices by index, `message.content` (null when only tool calls), `reasoning_content` when any arrived, `tool_calls`, `finish_reason` and `usage`.
  - **Prohibited:** `isProhibitedContent` of the Body.
  - **`Stream.Next`:** after the `src` returns `io.EOF` or an error, every later call returns the same thing.

- [ ] **Step 4: Run the tests and watch them pass.** Use the same command; expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add provider/stream.go provider/chat_accum.go provider/stream_test.go provider/chat_accum_test.go
git commit -m "Add the stream type, stream errors and the Chat chunk accumulator."
```

---

### Task 3: OpenAI Chat streaming, timeouts, and `CallStream`

**Files:**
- Create: `provider/openai_stream.go`, `provider/stream_timer.go`
- Test: `provider/openai_stream_test.go`
- Modify:
  - `provider/provider.go`: add `CallStream`;
  - `provider/openai.go`: the comments on `callOpenAI` and `openAIOutboundBody` now describe the buffered path only.

**Interfaces:**
- Consumes: Tasks 1–2; `postOpenAI`, `openAIChatOutcome`, `isProhibitedContent`.
- Produces:

```go
// CallStream opens a streamed call. It returns a *Stream when the downstream
// answered 2xx with text/event-stream. Otherwise it returns a *Response to be
// judged exactly like Call's: a non-2xx reply, an incompatible body, a 2xx JSON
// reply (decoded as today), or the anthropic kind (which just calls Call).
// err is as Call's. Response.Prohibited is set as Call sets it.
func CallStream(ctx context.Context, client *http.Client, kind, baseURL, apiKey string, timeout time.Duration, req Request) (*Stream, *Response, error)
func openAIChatStream(ctx context.Context, client *http.Client, baseURL, apiKey string, timeout time.Duration, req Request, forwardUsage bool) (*Stream, *Response, error)
func openAIStreamBody(req Request) map[string]any // copy, pin model, stream:true; for OpChat merge include_usage:true into stream_options
var errStreamTimeout = errors.New("no output from the provider in time")
func newStreamTimer(limit time.Duration, cancel context.CancelCauseFunc) *streamTimer // limit <= 0: never fires
func (t *streamTimer) outputStarted() // from now on, each read resets the limit
func (t *streamTimer) read()          // a body read returned bytes
func (t *streamTimer) stop()
```

In this task, `CallStream` handles `kind == "openai"` with `OpChat`. Every other kind and op returns `Call`'s result as a `*Response`; Tasks 5 and 7 replace those cases.

- [ ] **Step 1: Write the failing tests** (each against an `httptest` downstream that writes SSE and flushes)
  - **`TestOpenAIChatStreamPassesChunksThrough`:**
    - downstream sends a role chunk, a content chunk, a finish chunk, a usage chunk and `[DONE]`; the client body asked for usage;
    - the frames' `Data` equal the downstream data byte for byte, ending with `[DONE]`, then `io.EOF`;
    - the outbound body has `stream:true`, `stream_options.include_usage:true` and the pinned model.
  - **`TestOpenAIChatStreamStripsUsageClientDidNotAsk`:**
    - the client body has only `stream:true`, and the outbound body still asks for usage;
    - the usage-only chunk is not forwarded;
    - a chunk with choices and `usage` is forwarded without its `usage` key;
    - `State().Body` still has usage.
  - **`TestOpenAIChatStreamErrorChunk`:** a downstream `data: {"error":{"message":"Rate limit","code":429}}` makes `Next` return a `*StreamError` with `Status 429`, `Frame.Data` equal to that data, and a Body with that error.
  - **`TestOpenAIChatStreamDropsComments`:** `: OPENROUTER PROCESSING` lines produce no frames.
  - **`TestOpenAIChatStreamAddsMissingDone`** (Review Focus 3): a finish chunk, then the connection closes. The last frame is `[DONE]`, then `io.EOF`.
  - **`TestOpenAIChatStreamBrokenWithoutFinish`:** content, then the connection closes with no finish and no `[DONE]`. `Next` returns a `*StreamError` with `Status 502`.
  - **`TestOpenAIChatStreamSkipsNonJSONData`** (Review Focus 4): a `data: hello` line between two chunks is skipped, and the stream goes on.
  - **`TestOpenAIChatStreamFirstOutputTimeout`:** headers and a role chunk, then silence, with a timeout of 100 ms. Within 1 s, `Next` returns a `*StreamError` with `Timeout`, `Status 504` and the fixed 504 body.
  - **`TestOpenAIChatStreamIdleTimeoutAfterOutput`:** content, then silence, with a timeout of 100 ms. `Next` returns a `*StreamError` with `Timeout` after the content frame.
  - **`TestOpenAIChatStreamCommentsKeepIdleAlive`:** after content, a comment every 50 ms for 300 ms with a timeout of 100 ms, then finish and `[DONE]`. No error.
  - **`TestCallStreamJSONReplyComesBackAsResponse`** (D16): the downstream ignores `stream` and answers `application/json` with a normal completion. `Stream == nil`, `Response.OK()`, `FinishReason == "stop"`, `HasContent`.
  - **`TestCallStreamHTTPErrorComesBackAsResponse`:** a 500 gives `Stream == nil` and `Response.HTTPStatus == 500`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./provider -run 'TestOpenAIChatStream|TestCallStream' -count=1`. Expected: FAIL.

- [ ] **Step 3: Implement**
  - **The request:** send it with a context from `context.WithCancelCause`. The timer cancels it with `errStreamTimeout`. Wrap the body so each read feeds `streamTimer.read` and `rawLog`.
  - **Telling a stream from JSON:** `Content-Type` starting with `text/event-stream` means a stream. A 2xx with any other type is read whole and judged like `callOpenAI` does.
  - **Inbound cancellation:** when the inbound context is cancelled, `Next` returns its error unwrapped. The router checks `ctx.Err()` first.

- [ ] **Step 4: Run the tests and watch them pass.** Use the same command; expected: PASS. Then run `go test ./... -count=1` and confirm every existing test still passes.

- [ ] **Step 5: Commit**

```bash
git add provider/openai_stream.go provider/stream_timer.go provider/openai_stream_test.go provider/provider.go provider/openai.go
git commit -m "Stream OpenAI-kind Chat Completions with first-output and idle limits."
```

---

### Task 4: The Chat-to-Responses event translator

**Files:**
- Create: `provider/responses_stream.go`
- Test: `provider/responses_stream_test.go`

**Interfaces:**
- Consumes: `*Stream` carrying Chat frames (Task 3); `ChatToResponses` (existing).
- Produces:

```go
func chatToResponses(inner *Stream, model string) *Stream
func newResponsesEmitter(model string) *responsesEmitter // shared with Task 8
func (e *responsesEmitter) start() []Frame               // response.created, response.in_progress
func (e *responsesEmitter) reasoning(delta string) []Frame
func (e *responsesEmitter) text(delta string) []Frame
func (e *responsesEmitter) toolCall(index int, callID, name, argsDelta string) []Frame // first sight of index opens the item
func (e *responsesEmitter) finish(final map[string]any) []Frame                       // closes open items, then completed or incomplete
func (e *responsesEmitter) seq() int                                                  // next sequence_number
func responsesFinal(chatBody []byte, model, finish, reasoning string) (map[string]any, error)
```

- **`responsesFinal`:** `ChatToResponses` plus:
  - a leading `{"type":"reasoning","summary":[{"type":"summary_text","text":reasoning}]}` item when `reasoning != ""`;
  - for a `content_filter` finish, `status:"incomplete"` and `incomplete_details.reason:"content_filter"`.
- **Final object:** `finish` writes the streamed item ids into the final object's output items.
- **State:** `Output`, `FinishReason`, `HasContent` and `Prohibited` mirror the inner stream (D8: the Chat call is what gets checked). `Body` is the final object once it exists.

- [ ] **Step 1: Write the failing tests** (the inner stream is a `*Stream` over canned Chat frames; a small test helper builds one)
  - **`TestChatToResponsesTextSequence`:** chunks role, `"Hi"`, `" there"`, stop, usage, `[DONE]`.
    - Event types are exactly `response.created, response.in_progress, response.output_item.added, response.content_part.added, response.output_text.delta, response.output_text.delta, response.output_text.done, response.content_part.done, response.output_item.done, response.completed`.
    - `sequence_number` runs 0 to 9.
    - Each `Frame.Event` equals its data's `type`.
    - The completed object's `output[0].content[0].text == "Hi there"`, and `usage.input_tokens` is present.
    - No frame is `[DONE]`.
  - **`TestChatToResponsesToolCallSequence`:** argument fragments `{"te` and `xt":"a"}` for index 0, `id:"c0"`, `name:"speak"`.
    - Events: `response.output_item.added` (a `function_call` with `call_id:"c0"`, `name:"speak"`, `arguments:""`), two `response.function_call_arguments.delta` with the same `output_index`, `.done` with `arguments:"{\"text\":\"a\"}"`, `response.output_item.done`, `response.completed`.
  - **`TestChatToResponsesFinalMatchesChatToResponses`:** for a reply with text and one tool call, the final `output` (ignoring `id` keys) deep-equals `ChatToResponses(<inner Body>, model, "tool_calls")`'s `output`.
  - **`TestChatToResponsesReasoningSummaryOnly`:** `reasoning_content` deltas produce reasoning events. The `output_item.done` item has its summary text and neither a `content` nor an `encrypted_content` key (D18).
  - **`TestChatToResponsesIncomplete`:**
    - a `length` finish ends with `response.incomplete` and `incomplete_details.reason == "max_output_tokens"`;
    - a `content_filter` finish ends with `response.incomplete` and reason `content_filter`.
  - **`TestChatToResponsesInnerError`:** the inner stream fails with `*StreamError{Status:429}`, carrying a Chat error frame with message `"Rate limit"`. `Next` returns a `*StreamError` with `Status 429` and `Frame.Event == "error"`; the frame data has `type:"error"`, `message:"Rate limit"`, and the next `sequence_number`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./provider -run TestChatToResponses -count=1`. Expected: FAIL.

- [ ] **Step 3: Implement `provider/responses_stream.go`**
  - **Item ids:** `msg_`, `rs_` or `fc_` plus `randomID()`.
  - **Ordering:** items close in order, and `output_index` goes up by one per item. Text after a tool call opens a new message item.
  - **Error code:** keep the inner error's code when it's a string; otherwise use `"server_error"`.

- [ ] **Step 4: Run the tests and watch them pass.** Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add provider/responses_stream.go provider/responses_stream_test.go
git commit -m "Translate a Chat stream into Responses streaming events."
```

---

### Task 5: OpenAI Responses streaming (pass-through, modes, probe)

**Files:**
- Modify: `provider/openai_stream.go`, `provider/provider.go` (`CallStream` dispatch)
- Test: `provider/openai_responses_stream_test.go`

**Interfaces:**
- Consumes: Tasks 1–4; `ResponsesToChat`, `isResponsesUnsupported`, `responsesOutcome`.
- Produces: `func openAIResponsesStream(ctx context.Context, client *http.Client, baseURL, apiKey string, timeout time.Duration, req Request) (*Stream, *Response, error)`. `CallStream` dispatches `openai` + `OpResponses` here.

Behavior:
- **`ResponsesModeChatOnly`:** `ResponsesToChat` (on error, `Response{Incompatible: true}`), then `openAIChatStream(…, forwardUsage=true)`, then `chatToResponses`.
- **Force and Probe:** `POST {base}/responses` with the client body, model pinned and `stream:true`. No `stream_options` is added here.
  - **A non-2xx reply:**
    - In Probe, when `isResponsesUnsupported` matches, take the chat path with `LearnChatOnly = true`.
    - If that chat path turns out incompatible, keep the failed `/responses` attempt's outbound fields, as `callOpenAIResponses` does.
    - Any other non-2xx comes back as a `*Response`.
  - **A 2xx stream** passes through:
    - each event becomes `Frame{Event: <event name, or the data's type>, Data}`;
    - **output** is any `*.delta` event, an `output_item.added` whose item isn't an empty `message` or `reasoning`, or any `output_item.done`;
    - `error` / `response.failed` become a `*StreamError` (from the `error` object, or the event itself) whose `Frame` is that event;
    - `response.completed` / `response.incomplete` are the final frame;
    - an EOF without a final event is a `*StreamError` with `Status 502`.
  - **State:**
    - `Body` is the final `response` object, or `{"object":"response","status":"in_progress","output":[]}` before it arrives;
    - `FinishReason` and `HasContent` come from `responsesOutcome(final)`;
    - `Prohibited` is true for an incomplete reply with reason `content_filter`, or when `isProhibitedContent(final)`.
  - **Error frames:** `errorFrame` uses the last downstream `sequence_number` plus 1.
- **A 2xx JSON reply:** a `*Response`, judged with `responsesOutcome`.

- [ ] **Step 1: Write the failing tests**
  - **`TestResponsesStreamPassesEventsThrough`:** `response.created`, `output_item.added` (an empty message), `output_text.delta`, `response.completed`. The frames equal the downstream's. `State().Output` is false before the delta and true after it. `FinishReason == "stop"` at the end.
  - **`TestResponsesStreamFunctionCallAddedIsOutput`:** an `output_item.added` with a `function_call` item sets `Output` to true.
  - **`TestResponsesStreamFailedBeforeOutput`:** `response.failed` with `error.code:"rate_limit_exceeded"` gives a `*StreamError` with `Status 429`.
  - **`TestResponsesStreamProbeFallsBackToChat`:** `/responses` answers 404 with `{"error":{"code":"unknown_url"}}`, and `/chat/completions` streams Chat. Then `Stream.LearnChatOnly == true`, and the first frame's `Event == "response.created"`.
  - **`TestResponsesStreamChatOnlyIncompatible`:** a body with `previous_response_id:"x"` in mode ChatOnly gives `Response.Incompatible == true`.
  - **`TestResponsesStreamAddsNoStreamOptions`:** the outbound `/responses` body has `stream:true` and no `stream_options` key.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./provider -run TestResponsesStream -count=1`. Expected: FAIL.

- [ ] **Step 3: Implement** as described above.

- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -count=1`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add provider/openai_stream.go provider/provider.go provider/openai_responses_stream_test.go
git commit -m "Stream OpenAI-kind Responses natively or through Chat translation."
```

---

### Task 6: Gemini thought-signature cache (buffered and streamed paths)

**Files:**
- Create: `provider/signatures.go`
- Modify:
  - `provider/provider.go`: `Request` fields;
  - `provider/gemini.go`: `assistantToolCallParts`, `buildGeminiChatBody`, `geminiChat`;
  - `router/router.go`: `Router.signatures`, set on the `Request` in `callOnce`.
- Test: `provider/signatures_test.go`, `provider/gemini_signature_test.go`, `router/signature_test.go`

**Interfaces:**
- Produces:

```go
type SignatureCache struct {
	now func() time.Time // time.Now; tests override
	// mu, entries, insertion order
}
func NewSignatureCache(max int, ttl time.Duration) *SignatureCache
func (c *SignatureCache) Put(callID, signer, signature string) // nil-safe
func (c *SignatureCache) Get(callID, signer string) string     // "" on miss, expiry or another signer; nil-safe
const geminiPlaceholderSignature = "skip_thought_signature_validator"

// New Request fields:
//   Provider   string          // provider name; with Model, names who signed a Gemini tool call
//   Signatures *SignatureCache // nil disables
func (r Request) signer() string                            // Provider + "/" + Model
func rememberGeminiSignatures(openAIBody []byte, req Request) // Put for each tool call carrying thought_signature
func assistantToolCallParts(v any, req Request) []map[string]any
```

- **Router:** `Router.signatures` is created in `New` with `NewSignatureCache(4096, 24*time.Hour)` and survives reloads. `callOnce` sets `Provider: p.Name, Signatures: r.signatures`.

- [ ] **Step 1: Write the failing tests**
  - **`TestSignatureCache`:**
    - `Put("c1","google/gemini-3.5-flash","s")`, then `Get` with the same signer gives `"s"`, and another signer gives `""`;
    - after advancing `now` by 25 h, `Get` gives `""`;
    - with `max` 2, a third `Put` evicts the oldest;
    - `(*SignatureCache)(nil).Get(...) == ""`, and a nil `Put` doesn't panic.
  - **`TestGeminiRestoresSignatureFromCache`:**
    - the cache holds `("call_1","google/gemini-3.5-flash","sig")`;
    - the request has `Provider:"google"`, `Model:"gemini-3.5-flash"`, and an assistant message with `tool_calls:[{id:"call_1", function:{name:"f", arguments:"{}"}}]` (no signature);
    - the model content's first part has `thoughtSignature == "sig"`.
  - **`TestGeminiPlaceholderOnMiss`:** on a cache miss:
    - the first `functionCall` part gets `"skip_thought_signature_validator"`, and the second part of the same message gets none;
    - model `"gemma-4-31b-it"` gets none;
    - model `"models/gemini-3.5-flash"` gets the placeholder.
  - **`TestGeminiOtherModelGetsPlaceholder`:** an entry signed by `google/gemini-3.6-flash`, with a request to `gemini-3.5-flash`, gives the placeholder.
  - **`TestGeminiClientSignatureWins`:** a tool call carrying `thought_signature:"own"` gives `"own"`.
  - **`TestGeminiChatRemembersSignatures`:**
    - an `httptest` Gemini answers with a `functionCall` part carrying `thoughtSignature:"s1"`;
    - `Call` with `Provider:"google"` and a cache;
    - then the cache's `Get(<id from the reply's tool_calls[0]>, "google/<model>") == "s1"`.
  - **`router/signature_test.go` `TestGeminiSignatureRoundTripThroughRouter`:**
    - the first `Execute` returns a signed `functionCall`;
    - the second `Execute` sends that tool call back without a signature, plus a tool result;
    - the downstream's second request body contains `"thoughtSignature":"s1"`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./provider ./router -run 'TestSignatureCache|TestGemini.*Signature|TestGeminiPlaceholder|TestGeminiOtherModel|TestGeminiClientSignature|TestGeminiChatRemembers' -count=1`. Expected: FAIL.

- [ ] **Step 3: Implement**
  - The cache is a map plus an insertion-order slice (or `container/list`); evict the oldest past `max`.
  - Order of precedence in `assistantToolCallParts`: the client's own signature, then the cache, then the placeholder (first part only, `gemini-*` only).

- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -count=1`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add provider/signatures.go provider/signatures_test.go provider/gemini_signature_test.go provider/provider.go provider/gemini.go router/router.go router/signature_test.go
git commit -m "Remember Gemini thought signatures for clients that drop them."
```

---

### Task 7: Gemini streaming

**Files:**
- Create: `provider/gemini_stream.go`
- Modify: `provider/provider.go` (`CallStream` dispatch)
- Test: `provider/gemini_stream_test.go`

**Interfaces:**
- Consumes: Tasks 1–4 and 6; `buildGeminiChatBody`, `normalizeGeminiFinish`, `isGeminiPolicyBlock`, `PortableForChat`, `ResponsesToChat`.
- Produces:

```go
func geminiChatStream(ctx context.Context, client *http.Client, baseURL, apiKey string, timeout time.Duration, req Request, forwardUsage bool) (*Stream, *Response, error)
func newGeminiStreamTranslator(req Request) *geminiStreamTranslator
func (t *geminiStreamTranslator) translate(raw []byte) ([]map[string]any, *StreamError) // Chat chunks for one GenerateContentResponse
func (t *geminiStreamTranslator) final(forwardUsage bool) []map[string]any            // the usage chunk, when asked and known
```

**`CallStream` dispatch after this task:**
- gemini + `OpChat`: `geminiChatStream(…, WantsUsage(req.Body))`;
- gemini + `OpResponses`: `PortableForChat` / `ResponsesToChat` (failure gives `Response{Incompatible:true}`), then `geminiChatStream(…, true)`, then `chatToResponses`;
- anthropic: `Call` (buffered, path 6).

**Translation** (spec path 4):
- **Request:** `POST {base}/models/{model}:streamGenerateContent?alt=sse`, header `x-goog-api-key`, body `buildGeminiChatBody(req)`.
- **Chunks:** one `chatcmpl-<randomID>` id per stream, the same `created` throughout, and `model = req.Model`. A first chunk carries `delta.role:"assistant"`.
- **Parts:**
  - a non-empty text part becomes `delta.content`;
  - a `thought:true` part becomes `delta.reasoning_content` (D17);
  - an empty-text part (often carrying only a signature) emits nothing;
  - a `functionCall` becomes one `delta.tool_calls` entry `{index:<running count>, id:"call_"+randomID(), type:"function", function:{name, arguments:<JSON of args, "{}" if none>}, thought_signature?}`, and calls `req.Signatures.Put(id, req.signer(), sig)` when it is signed.
- **Candidates:** candidate *i* becomes choice *i*.
- **Finish:**
  - `normalizeGeminiFinish`, turned into `tool_calls` when that candidate sent a tool call;
  - `promptFeedback.blockReason` becomes a finish chunk with `content_filter`;
  - an `error` object becomes a `*StreamError` via `streamErrorFromObject`.
- **End:** on EOF after a finish, emit `final(forwardUsage)` and then `[DONE]`. On EOF without a finish, return a `*StreamError` with `Status 502`.

- [ ] **Step 1: Write the failing tests** (an `httptest` server writes canned `data: {GenerateContentResponse}` events)
  - **`TestGeminiStreamTextAndThoughts`:**
    - events `{candidates:[{content:{parts:[{text:"Let me",thought:true}]}}]}`, `…[{text:"Hello"}]…`, `…[{text:" world"}]…,finishReason:"STOP"`, and `usageMetadata{promptTokenCount:1,candidatesTokenCount:2,totalTokenCount:3}`;
    - frames, in order: role, `reasoning_content:"Let me"`, `content:"Hello"`, `content:" world"`, `finish_reason:"stop"`, a usage chunk (`forwardUsage=true`), `[DONE]`;
    - `State().Body` has content `"Hello world"` and `reasoning_content "Let me"`.
  - **`TestGeminiStreamFunctionCallWhole`:**
    - `{functionCall:{name:"speak",args:{text:"hi"}},thoughtSignature:"sig1"}`, then `finishReason:"STOP"`;
    - one `tool_calls` delta with `index 0`, an id starting `call_`, `arguments == "{\"text\":\"hi\"}"` and `thought_signature == "sig1"`;
    - `finish_reason == "tool_calls"`;
    - `cache.Get(id, "google/gemini-3.5-flash") == "sig1"`.
  - **`TestGeminiStreamEmptyTextPartIsNotOutput`:** a single `{text:"",thoughtSignature:"s"}` part gives `Output == false` and no content frame.
  - **`TestGeminiStreamPromptBlocked`:** `promptFeedback.blockReason:"SAFETY"` gives `Prohibited == true` and `Output == false`.
  - **`TestGeminiStreamSafetyAfterText`:** text, then `finishReason:"SAFETY"`, gives `Output == true` and a last finish of `content_filter`.
  - **`TestGeminiStreamURL`:** the path ends with `:streamGenerateContent`, and the query has `alt=sse`.
  - **`TestCallStreamGeminiResponses`:** a portable `OpResponses` body to a Gemini downstream gives Responses frames, with a final `response.completed` whose output text is the streamed text.
  - **`TestCallStreamAnthropicIsBuffered`:** kind `anthropic` gives `Stream == nil` and a non-nil `Response`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./provider -run 'TestGeminiStream|TestCallStreamGemini|TestCallStreamAnthropic' -count=1`. Expected: FAIL.

- [ ] **Step 3: Implement** as described above. The builder should compare the chunk shapes with one live call before relying on them (spec path 4).

- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -count=1`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add provider/gemini_stream.go provider/gemini_stream_test.go provider/provider.go
git commit -m "Stream Gemini replies as Chat chunks, and as Responses events through the translator."
```

---

### Task 8: Buffered and refusal streams

**Files:**
- Create: `provider/buffered_stream.go`
- Test: `provider/buffered_stream_test.go`

**Interfaces:**
- Consumes: `responsesEmitter` (Task 4), `Stream` (Task 2).
- Produces:

```go
func BufferedStream(op Operation, resp *Response, wantsUsage bool) *Stream // a complete, already checked reply as a stream (path 6, D15, D16)
func RefusalStream(op Operation, model string) *Stream                     // every target refused (D20)
```

**Frames:**
- **Chat** (from `resp.OpenAIBody`, choice 0):
  - a role chunk;
  - one content chunk if there is content;
  - one chunk per tool call (`index`, `id`, `type`, `function{name, arguments}`, plus `thought_signature` if present);
  - a finish chunk;
  - a usage chunk if `wantsUsage` and the body has usage;
  - `[DONE]`.
- **Responses:** `start`, then for each output item:
  - `reasoning` → `reasoning(<summary text>)`;
  - `message` → `text(<full text>)`;
  - `function_call` → `toolCall`;
  
  then `finish(<the object>)`.
- **State:** `Output` true when there is content or a tool call; `FinishReason` and `HasContent` from `resp`; `Body` is `resp.OpenAIBody`.
- **Refusal:**
  - Chat: a role chunk, then `finish_reason:"content_filter"`, then `[DONE]`.
  - Responses: `response.created`, then `response.incomplete` with `incomplete_details.reason:"content_filter"`.
  - `State().Prohibited == true`.

- [ ] **Step 1: Write the failing tests**
  - **`TestBufferedChatStream`:** a body with content `"hi"` and one tool call. The frames are as listed, and feeding them to `newChatAccumulator` rebuilds the same content and tool call. The last frame is `[DONE]`.
  - **`TestBufferedChatStreamUsageOnlyWhenAsked`.**
  - **`TestBufferedResponsesStream`:** an object with a message and a `function_call`. The event types are in the Task 4 order, and the final `response.completed`'s `response` equals the original object.
  - **`TestRefusalStreams`:** both formats, exactly as listed.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./provider -run 'TestBuffered|TestRefusal' -count=1`. Expected: FAIL.

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run the tests and watch them pass.** Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add provider/buffered_stream.go provider/buffered_stream_test.go
git commit -m "Send a complete reply, or a refusal, as a stream."
```

---

### Task 9: Router `ExecuteStream`, and the `streaming` switch

**Files:**
- Create: `router/stream.go`
- Test: `router/stream_test.go`
- Modify:
  - `router/router.go`: extract `run` and `judge`; `routerState.streamClients`; `Result.Committed`;
  - `router/progress.go`: `CommitObserver`;
  - `config/config.go`, `config/config_test.go`: `Server.Streaming`;
  - `config.example.toml`.

**Interfaces:**
- Consumes: `provider.CallStream`, `Stream`, `StreamError`, `StreamState`, `BufferedStream`, `RefusalStream`, `WantsUsage`, `Frame`.
- Produces:

```go
type StreamSink interface {
	Commit() error              // send the 200 and the SSE headers
	Write(f provider.Frame) error // write and flush one event
}
func (r *Router) ExecuteStream(ctx context.Context, op provider.Operation, inboundModel string, body map[string]any, sink StreamSink) *Result
// Result gains: Committed bool `json:"-"` // the reply went out through the sink; otherwise send Status/Body as JSON
type CommitObserver interface{ OnCommit(provider, model string) } // optional; checked on the ctx's AttemptObserver
func notifyCommit(ctx context.Context, provider, model string)    // progress.go, like notifyAttempt
func onlyProhibited(attempts []Attempt) bool                      // like onlyIncompatible: every attempt that reached a provider was prohibited_content, at least one
// config.Server gains: Streaming bool // toml "streaming", default true; reloads with [server]
type tryFunc func(ctx context.Context, st *routerState, res *Result, p *config.Provider, model string, op provider.Operation, body map[string]any, key, keyType string, last **provider.Response) (done, skip bool)
func (r *Router) run(ctx context.Context, st *routerState, op provider.Operation, inboundModel string, body map[string]any, try tryFunc) (res *Result, last *provider.Response, done bool)
func (r *Router) judge(ctx context.Context, st *routerState, p *config.Provider, model string, op provider.Operation, key string, resp *provider.Response, err error, att Attempt) (Attempt, outcome)
```

**Refactor first, with behavior unchanged:**
- `run` holds the resolution and both key phases from `Execute`. `Execute` becomes `run(…, r.tryKey)` plus today's exhaustion code.
- `judge` is `callOnce`'s classification after the HTTP call.
- `newRouterState` adds `streamClients[name] = &http.Client{}` (no `Timeout`).

**One streamed attempt (`tryKeyStream`):**

| Situation | What happens |
|---|---|
| `Streaming` is false | `provider.Call` with `st.clients`, then `judge`. On success, `BufferedStream(op, resp, WantsUsage(body))` is committed and pumped (D15). |
| `CallStream` returns a `*Response` | `judge` it. On success, commit and pump a `BufferedStream` (D16). Otherwise, today's outcome rules. |
| A frame arrives before any output | Keep it in a slice. |
| `State().Output` becomes true | `sink.Commit()`, `notifyCommit(ctx, p.Name, model)`, write the kept frames, then pump. |
| Before commit, a `*StreamError` | `*last = &provider.Response{HTTPStatus: se.Status, OpenAIBody: se.Body, RawResponse: State().Raw, Prohibited: se.Prohibited, OutboundURL: …}`. A prohibited error is `prohibited_content` (skip the combo, no blackout). Any other error is `provider_error`, with a blackout for a normal key unless `se.Status == 400`. A timeout gets status 504. |
| Before commit, `io.EOF` with no output | Prohibited state gives `prohibited_content`. Otherwise `bad_output` with no blackout, and `*last` gets status 502 and the fixed empty-stream body. |
| Inbound `ctx.Err() != nil` | `canceled`: stop, no blackout. |
| After commit, a `*StreamError` | Write `se.Frame`, or `stream.ErrorFrame(se.Status, <message>)` when it's nil. A prohibited error is `prohibited_content`. Any other error is `provider_error`, with a blackout unless the status is 400 (D14). |
| After commit, `sink.Write` fails or the inbound context is done | `Close` the stream; `canceled`; no blackout. |
| After commit, `io.EOF` | Check with `st.outputOK(op, &provider.Response{FinishReason: s.FinishReason, HasContent: s.HasContent})`, where `s = State()`. `success` calls `blackout.Success`. A prohibited state gives `prohibited_content`; otherwise `bad_output`. No blackout. |

**After any commit:**
- the attempt is recorded with `HTTPStatus`, `FinishReason`, `OutboundURL` and `LatencyMS`, plus `Response` (the raw text) on failure;
- the result is `Committed = true`, `Status = 200`, `Body = State().Body`, with `Provider` and `Model` set and `Success` true only on `success`;
- `tryKeyStream` returns `done = true`.

**When every attempt fails (streams):**
- `onlyIncompatible` → today's 400.
- `onlyProhibited(res.Attempts)` (every attempt that reached a provider was `prohibited_content`) → commit and pump `RefusalStream(op, inboundModel)`, with `Success` false.
- Otherwise, `last`'s status and body, except that a 2xx or 0 status becomes 502 with `errorBody("all providers, keys, and models were exhausted", "upstream_error")`.
- With no `last`, today's 502.

- [ ] **Step 1: Write the failing tests** (`recordingSink{commits int; frames []provider.Frame; failAfter int}`; `httptest` downstreams with keys told apart by `bearer`)
  - **`TestStreamCommitsOnFirstOutput`:** the sink is committed exactly once, before its first `Write`. The frames start with the role chunk, then the content.
  - **`TestStreamFailsOverBeforeOutput`:**
    - key `n1` streams `data: {"error":{"code":503,"message":"busy"}}` before any output, and `n2` streams a good reply;
    - `Success`, 2 attempts, `attempts[0].Outcome == "provider_error"`;
    - `r.blackout.Blocked("n1", model)`.
  - **`TestStreamErrorChunk400DoesNotBlackOut`.**
  - **`TestStreamNoFailoverAfterCommit`:**
    - key `n1` streams content, then an error chunk;
    - the sink's last frame is that error chunk, and no frame is `[DONE]`;
    - 1 attempt, `Committed && !Success`, and `n1` is blacked out.
  - **`TestStreamBrokenAfterCommitWritesRouterError`:** content, then the downstream connection drops (`http.Hijacker`, then close). The last frame's data has `error.type == "upstream_error"` and `error.code == 502`.
  - **`TestStreamEmptyStreamIsBadOutput`:** key 1 sends `[DONE]` with no output, and key 2 is good. `attempts[0].Outcome == "bad_output"`, and key 1 isn't blacked out.
  - **`TestStreamExhaustedNeverReturns2xx`:** every key sends an empty stream. `!Committed`, `Status == 502`, and the body has `error.type "upstream_error"`.
  - **`TestStreamExhaustedUsesDownstreamErrorStatus`:** every key sends an error chunk with `code 429`. `Status == 429`, and the body has the downstream message.
  - **`TestStreamAllProhibitedSendsRefusal`:** every target ends with `content_filter` and no output. `Committed`, with frames role, `finish_reason:"content_filter"`, `[DONE]`.
  - **`TestStreamJSONReplyIsCheckedThenStreamed`:** key 1 answers JSON with `finish_reason:"unexpected"`, and key 2 answers good JSON. Key 2 serves the reply, and the frames end with `[DONE]`.
  - **`TestStreamingOffUsesBufferedPath`:** with `[server] streaming = false`, the outbound body has no `stream` key, and the frames end with `[DONE]`.
  - **`TestStreamClientGoneStopsDownstream`** (Review Focus 1):
    - the sink fails on its second `Write`;
    - the downstream handler sees `r.Context().Done()` within 1 s;
    - the attempt is `canceled`, and nothing is blacked out.
  - **`TestStreamCommitNotifiesObserver`:** an observer that implements both interfaces gets `OnCommit("openai","real-model")`.
  - **`TestStreamFirstOutputTimeoutFailsOver`:** provider `timeout = "100ms"`; key 1 never outputs, and key 2 is good. `Success`, and `attempts[0]` is `provider_error` with status 504.
  - **`config/config_test.go`:**
    - `TestStreamingDefaultsOn`;
    - `TestStreamingCanBeTurnedOff`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./router ./config -run 'TestStream' -count=1`. Expected: FAIL.

- [ ] **Step 3: Implement**
  - Do the refactor first, then run `go test ./router -count=1` and confirm every existing router test still passes before adding the streaming code.
  - In `config.example.toml`, under `[server]`, add a commented `# streaming = true` with one line: false sends each streamed reply only once it is complete.

- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -race -count=1`. Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add router/ config/config.go config/config_test.go config.example.toml
git commit -m "Route streamed requests: fail over until the first output, then commit."
```

---

### Task 10: The server's streaming endpoint, logging, the in-flight view, and the README

**Files:**
- Create: `server/stream.go`
- Test: `server/stream_test.go`, `inflight/inflight_test.go`
- Modify: `server/server.go` (`handle`, `log`), `logstore/logstore.go` (`Entry.Stream`), `inflight/inflight.go` (`OnCommit`), `README.md`.

**Interfaces:**
- Consumes: `router.ExecuteStream`, `router.StreamSink`, `provider.Frame`.
- Produces:

```go
func isStreamRequest(op provider.Operation, body map[string]any) bool // OpChat/OpResponses and body["stream"] == true
type sseSink struct{ w http.ResponseWriter; rc *http.ResponseController }
func (s *sseSink) Commit() error
func (s *sseSink) Write(f provider.Frame) error
// logstore.Entry gains: Stream bool `json:"stream,omitempty"`
func (e *Entry) OnCommit(provider, model string) // inflight: CurrentTarget = provider/model, LastOutcome = "streaming"
```

**Changes to `handle`:**
- Coercion runs only when `!isStreamRequest`.
- A streamed request calls `ExecuteStream` with an `sseSink`. When `!res.Committed`, write `res.Status` / `res.Body` as JSON, as today.
- `s.log` gains a `stream bool` parameter, stored in `Entry.Stream`.

- [ ] **Step 1: Write the failing tests** (through a real `httptest.NewServer(mux)`, so flushing is real)
  - **`TestStreamRequestGetsEventStream`:** the reply headers are `Content-Type: text/event-stream`, `Cache-Control: no-cache` and `X-Accel-Buffering: no`. The body ends with `"data: [DONE]\n\n"`.
  - **`TestStreamFlushesEachEvent`:** the downstream sends one content chunk, then waits on a channel. The client reads that content event (within 2 s) before the test lets the downstream finish.
  - **`TestStreamExhaustedIsJSONError`:** the downstream answers 500. The client gets 500 with `application/json`.
  - **`TestStreamSkipsCoercion`:**
    - coercion is on in the config;
    - a streamed `/v1/responses` request ends with `{"role":"developer","content":"my calendar"}`;
    - the downstream receives `role "developer"` and `"my calendar"` unchanged.
  - **`TestStreamStringTrueIsNotStreamed`:** `"stream":"true"` gives an `application/json` reply.
  - **`TestStreamOnEmbeddingsIgnored`:** `/v1/embeddings` with `stream:true` gives a JSON reply.
  - **`TestStreamLoggedWithStreamFlag`:**
    - the server is built with a `logstore.New(t.TempDir())` store;
    - after a streamed request, read `Recent()[0].Path` back with `ReadEntry`;
    - it has `"stream":true`, and a `response` that is a `chat.completion` with the streamed content.
  - **`inflight_test.go` `TestOnCommitShowsStreaming`:** after `OnCommit("openai","m")`, the snapshot has `CurrentTarget "openai/m"` and `LastOutcome "streaming"`.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./server ./inflight -count=1`. Expected: FAIL.

- [ ] **Step 3: Implement, then update `README.md`:**
  - streaming in Features;
  - a provider's `timeout` for streams;
  - `[server] streaming`;
  - reverse proxies must not buffer `text/event-stream`;
  - Gemini thought signatures (the cache, the placeholder, and that OpenRouter's Gemini needs the client to keep `reasoning_details`).

- [ ] **Step 4: Run the tests and watch them pass**, then run `go test ./... -race -count=1` and `go vet ./...`. Expected: PASS and clean.

- [ ] **Step 5: Commit**

```bash
git add server/ logstore/logstore.go inflight/ README.md
git commit -m "Serve streamed requests as server-sent events, logged and shown in flight."
```

---

### Task 11: End-to-end check with annai's own readers, then deploy

**Files:** none in the repo. The check scripts and the config copy live in the session scratchpad.

- [ ] **Step 1: Run a second router.** Build it, copy `config.toml` to the scratchpad, set `listen = ":18470"` and a scratchpad `log_dir`, and start `./maiGoLLMRouter -config <copy>` in the background.
- [ ] **Step 2: Capture streams.** With `curl -N`, record (a) one OpenAI-kind model and (b) `google/gemini-3.5-flash`, each on `/v1/chat/completions` and on `/v1/responses`. Use `stream:true` and a `speak` tool (`{"text": string}`) that the prompt asks the model to call. Save the raw SSE, and confirm with timestamps that the events arrived over time.
- [ ] **Step 3: Feed each capture to annai's readers.** A Node script in the scratchpad:
  - imports `activate` from `~/work/annai/plugins-official/chat-completions/index.js` and `…/openai-responses/index.js`;
  - passes a fake `annai` (`helpers: {sse, ProviderError, errorFromResponse}` from `~/work/annai/src/core/llm/`, `providers.register: x => x`);
  - runs `read(new Response(bytes), {tunables: <the type's tunables>})` and prints the events.

  Expected:
  - text events, a `tool_call` for `speak`, and a `stop`, with no error;
  - for OpenAI-kind, several `tool_input` events with a growing `partial`.
- [ ] **Step 4: Check Gemini's second turn.** Send the Gemini history back with the `speak` call (no `thought_signature`) and a tool result. Expected: HTTP 200 and a stream, not a 400.
- [ ] **Step 5: Wrap up.** Stop the second router and run `go test ./... -race -count=1`. Then ask the user before deploying. Deploy with the steps in memory `live-router-deploy`, and push `main` only when asked.
- [ ] **Step 6: Hand the manual check to the user.** After deploying, ask them to point annai at the router with voice on (spec, Testing → Manual check):
  - **OpenAI-kind model:** Shiori starts speaking while `speak` is still being written.
  - **`google` Gemini model:** she speaks once the call arrives, and the turn after a tool call isn't refused.
