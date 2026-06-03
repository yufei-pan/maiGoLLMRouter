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
)

// Operation distinguishes the two supported request kinds.
type Operation int

const (
	OpChat Operation = iota
	OpEmbed
)

// Request is a parsed inbound OpenAI-style request plus the resolved downstream
// model name to use for this attempt.
type Request struct {
	Op    Operation
	Model string         // downstream model name (overrides inbound "model")
	Body  map[string]any // full inbound JSON body (enables passthrough of extras)
}

// Response is the normalized outcome of a downstream call.
type Response struct {
	HTTPStatus   int    // downstream HTTP status (0 if request never sent)
	OpenAIBody   []byte // response translated into OpenAI JSON
	FinishReason string // normalized finish reason (chat only): stop, length, ...
	HasContent   bool   // whether the response carried non-empty content
	OutboundURL  string // downstream URL hit (for logging)
	OutboundBody []byte // outbound request body sent downstream (for logging)
	RawResponse  []byte // raw downstream response body (for logging)
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
	switch kind {
	case "openai":
		return callOpenAI(ctx, client, baseURL, apiKey, req)
	case "gemini":
		return callGemini(ctx, client, baseURL, apiKey, req)
	case "anthropic":
		return callAnthropic(ctx, client, baseURL, apiKey, req)
	default:
		return nil, fmt.Errorf("unknown provider kind %q", kind)
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
