// Package provider translates OpenAI-style requests into provider-specific
// dialects (openai, anthropic, gemini), performs the HTTP call, and translates
// the response back into the OpenAI format.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Operation distinguishes the supported request kinds.
type Operation int

const (
	OpChat Operation = iota
	OpEmbed
	OpResponses
)

// ResponsesMode selects how an OpResponses request reaches a downstream that
// may or may not expose the /responses route.
type ResponsesMode int

const (
	ResponsesModeProbe    ResponsesMode = iota // try /responses, fall back to chat if absent
	ResponsesModeForce                         // /responses only, never fall back
	ResponsesModeChatOnly                      // translate to /chat/completions directly
)

// Request is a parsed inbound OpenAI-style request plus the resolved downstream
// model name to use for this attempt.
type Request struct {
	Op            Operation
	Model         string         // downstream model name (overrides inbound "model")
	Body          map[string]any // full inbound JSON body (enables passthrough of extras)
	ResponsesMode ResponsesMode  // OpResponses only; meaningful for kind "openai"
}

// Response is the normalized outcome of a downstream call.
type Response struct {
	HTTPStatus    int    // downstream HTTP status (0 if request never sent)
	OpenAIBody    []byte // response translated into OpenAI JSON
	FinishReason  string // normalized finish reason (chat only): stop, length, ...
	HasContent    bool   // whether the response carried non-empty content
	Prohibited    bool   // downstream blocked the request for content-policy reasons
	OutboundURL   string // downstream URL hit (for logging)
	OutboundBody  []byte // outbound request body sent downstream (for logging)
	RawResponse   []byte // raw downstream response body (for logging)
	LearnChatOnly bool   // a probe found that the downstream has no /responses route
	Incompatible  bool   // this Responses body cannot be served over the chat dialect
}

// OK reports whether the HTTP call itself succeeded (2xx). It does not assess
// finish-reason quality; the caller applies output verification separately.
func (r *Response) OK() bool {
	return r.HTTPStatus >= 200 && r.HTTPStatus < 300
}

// Call dispatches to the correct dialect. A non-nil error indicates a
// transport-level failure (no usable HTTP response); HTTP error statuses are
// reported via Response.HTTPStatus with a nil error.
func Call(ctx context.Context, client *http.Client, kind, baseURL, apiKey string, req Request) (*Response, error) {
	var resp *Response
	var err error
	if req.Op == OpResponses && kind != "openai" {
		resp, err = callResponsesViaChat(ctx, client, kind, baseURL, apiKey, req)
	} else {
		switch kind {
		case "openai":
			resp, err = callOpenAI(ctx, client, baseURL, apiKey, req)
		case "gemini":
			resp, err = callGemini(ctx, client, baseURL, apiKey, req)
		case "anthropic":
			resp, err = callAnthropic(ctx, client, baseURL, apiKey, req)
		default:
			return nil, fmt.Errorf("unknown provider kind %q", kind)
		}
	}
	if resp != nil && isProhibitedContent(resp.RawResponse) {
		resp.Prohibited = true
	}
	return resp, err
}

// callResponsesViaChat serves OpResponses for non-OpenAI dialects by translating
// through the Chat codec (Responses → Chat → dialect → Chat → Responses).
func callResponsesViaChat(ctx context.Context, client *http.Client, kind, baseURL, apiKey string, req Request) (*Response, error) {
	if err := PortableForChat(req.Body); err != nil {
		return &Response{Incompatible: true}, nil
	}
	chatBody, err := ResponsesToChat(req.Body, req.Model)
	if err != nil {
		return &Response{Incompatible: true}, nil
	}
	chatReq := Request{Op: OpChat, Model: req.Model, Body: chatBody}
	var resp *Response
	switch kind {
	case "anthropic":
		resp, err = callAnthropic(ctx, client, baseURL, apiKey, chatReq)
	case "gemini":
		resp, err = callGemini(ctx, client, baseURL, apiKey, chatReq)
	default:
		return nil, fmt.Errorf("unknown provider kind %q", kind)
	}
	if err != nil || resp == nil || !resp.OK() {
		return resp, err
	}
	// The translated path is verified as the Chat call it really was, so
	// FinishReason/HasContent from the Chat dialect are kept as-is.
	wrapped, werr := ChatToResponses(resp.OpenAIBody, req.Model, resp.FinishReason)
	if werr != nil {
		// Do not forward a Chat-shaped body on /v1/responses.
		resp.OpenAIBody = nil
		resp.FinishReason, resp.HasContent = "", false
		return resp, nil
	}
	resp.OpenAIBody = wrapped
	return resp, nil
}

// ProhibitedContentMarker is the sentinel a downstream emits when it refuses a
// request for content-policy reasons. It may arrive with an HTTP 200 status.
const ProhibitedContentMarker = "PROHIBITED_CONTENT"

// isProhibitedContent reports whether a raw downstream body signals that the
// request was blocked for content-policy reasons — for example
// {"error":{"message":"PROHIBITED_CONTENT","code":403}} (even with HTTP 200),
// OpenAI-style choices[].finish_reason, or a Gemini candidate/prompt-feedback
// block. Such a response is a deterministic
// policy decision, not a provider or key fault, so retrying the same
// provider/model with other keys would only reproduce it.
func isProhibitedContent(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Code    any    `json:"code"`
		} `json:"error"`
		Status            string `json:"status"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
		Candidates []struct {
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		Choices []struct {
			FinishReason       string `json:"finish_reason"`
			NativeFinishReason string `json:"native_finish_reason"`
			Message            struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Output []struct {
			Status string `json:"status"`
		} `json:"output"`
	}
	// Ignore the error: a partial decode still populates the fields we check, and
	// a fully invalid body leaves them empty (reported as not-prohibited).
	_ = json.Unmarshal(raw, &parsed)
	if matchesProhibited(parsed.Error.Message) || matchesProhibited(parsed.Error.Status) ||
		matchesProhibited(fmt.Sprint(parsed.Error.Code)) ||
		matchesProhibited(parsed.PromptFeedback.BlockReason) ||
		isGeminiPolicyBlock(parsed.PromptFeedback.BlockReason) {
		return true
	}
	// Responses API policy blocks often arrive as incomplete + content_filter
	// (or PROHIBITED_CONTENT) rather than chat-style choices[].finish_reason.
	if isContentFilterFinish(parsed.IncompleteDetails.Reason) ||
		matchesProhibited(parsed.IncompleteDetails.Reason) ||
		isContentFilterFinish(parsed.Status) ||
		matchesProhibited(parsed.Status) {
		return true
	}
	for _, c := range parsed.Candidates {
		if matchesProhibited(c.FinishReason) || isGeminiPolicyBlock(c.FinishReason) {
			return true
		}
	}
	for _, c := range parsed.Choices {
		if matchesProhibited(c.FinishReason) || matchesProhibited(c.NativeFinishReason) {
			return true
		}
		// Any content_filter finish is a policy block, even when partial content
		// was emitted before the provider cut off the response.
		if isContentFilterFinish(c.FinishReason) {
			return true
		}
	}
	for _, item := range parsed.Output {
		if isContentFilterFinish(item.Status) || matchesProhibited(item.Status) {
			return true
		}
	}
	return false
}

func isContentFilterFinish(s string) bool {
	return strings.EqualFold(strings.TrimSpace(s), "content_filter")
}

func matchesProhibited(s string) bool {
	return strings.EqualFold(strings.TrimSpace(s), ProhibitedContentMarker)
}

// isGeminiPolicyBlock reports Gemini finish/block reasons that are
// content-policy decisions (the same class we map to OpenAI content_filter).
func isGeminiPolicyBlock(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "SAFETY", "RECITATION", "BLOCKLIST",
		"PROHIBITED_CONTENT", "IMAGE_SAFETY", "SPII":
		return true
	default:
		return false
	}
}

// doJSON sends a JSON request and returns the status and raw body.
func doJSON(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// extras returns the inbound body fields that are not in the consumed set, so
// translated dialects can pass unknown arguments through (#13).
func extras(body map[string]any, consumed map[string]bool) map[string]any {
	out := make(map[string]any)
	for k, v := range body {
		if consumed[k] {
			continue
		}
		out[k] = v
	}
	return out
}

// asString safely extracts a string field.
func asString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// jsonContentText flattens an OpenAI message "content" value (string or array
// of content parts) into a single plain-text string.
func contentToText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b bytes.Buffer
		for _, part := range c {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := pm["text"].(string); ok {
				b.WriteString(t)
			}
		}
		return b.String()
	default:
		return ""
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
