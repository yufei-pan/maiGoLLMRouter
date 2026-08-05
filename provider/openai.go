package provider

import (
	"context"
	"encoding/json"
	"net/http"
)

// callOpenAI handles the OpenAI dialect, which is a near pass-through: the
// inbound body is forwarded with the model name overridden. Streaming controls
// are stripped because the router buffers and verifies complete JSON responses.
// Responses requests take the /responses route described by callOpenAIResponses.
func callOpenAI(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request) (*Response, error) {
	if req.Op == OpResponses {
		return callOpenAIResponses(ctx, client, baseURL, apiKey, req)
	}

	body := openAIOutboundBody(req)

	path := "/chat/completions"
	if req.Op == OpEmbed {
		path = "/embeddings"
	}

	resp, err := postOpenAI(ctx, client, baseURL+path, apiKey, body)
	if err != nil || !resp.OK() {
		return resp, err
	}
	if req.Op == OpChat {
		resp.FinishReason, resp.HasContent = openAIChatOutcome(resp.RawResponse)
	} else {
		resp.HasContent = embedHasContent(resp.RawResponse)
	}
	return resp, nil
}

// callOpenAIResponses serves a Responses request either natively on /responses
// or, when the downstream has no such route, by translating to and from
// /chat/completions. See ResponsesMode for the selection rules.
func callOpenAIResponses(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request) (*Response, error) {
	body := openAIOutboundBody(req)

	if req.ResponsesMode == ResponsesModeChatOnly {
		return openAIResponsesViaChat(ctx, client, baseURL, apiKey, req, body, false)
	}

	resp, err := postOpenAI(ctx, client, baseURL+"/responses", apiKey, body)
	if err != nil {
		return resp, err
	}
	if resp.OK() {
		resp.FinishReason, resp.HasContent = responsesOutcome(resp.RawResponse)
		return resp, nil
	}
	// Only a probe downgrades to chat: force mode surfaces a missing route as a
	// plain provider error so the router can retry a different target instead.
	if req.ResponsesMode == ResponsesModeProbe && isResponsesUnsupported(resp.HTTPStatus, resp.RawResponse) {
		chatResp, err := openAIResponsesViaChat(ctx, client, baseURL, apiKey, req, body, true)
		// When the chat translation is incompatible, keep the failed /responses
		// attempt's outbound fields for logging (HTTPStatus stays 0).
		if chatResp != nil && chatResp.Incompatible {
			chatResp.OutboundURL = resp.OutboundURL
			chatResp.OutboundBody = resp.OutboundBody
			chatResp.RawResponse = resp.RawResponse
		}
		return chatResp, err
	}
	return resp, nil
}

// openAIResponsesViaChat translates body to a Chat Completions request, sends
// it, and wraps the reply back into a Responses object. learnedChatOnly records
// that a probe already proved the /responses route absent.
func openAIResponsesViaChat(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request, body map[string]any, learnedChatOnly bool) (*Response, error) {
	chatBody, err := ResponsesToChat(body, req.Model)
	if err != nil {
		return &Response{Incompatible: true, LearnChatOnly: learnedChatOnly}, nil
	}

	chatReq := withClaudeTrailingUserCoercion(Request{Op: OpChat, Model: req.Model, Body: chatBody})
	resp, err := callOpenAI(ctx, client, baseURL, apiKey, chatReq)
	if resp != nil {
		resp.LearnChatOnly = learnedChatOnly
	}
	if err != nil || resp == nil || !resp.OK() {
		return resp, err
	}

	wrapped, err := ChatToResponses(resp.OpenAIBody, req.Model)
	if err != nil {
		// Do not forward a Chat-shaped body on /v1/responses.
		resp.OpenAIBody = nil
		resp.FinishReason, resp.HasContent = "", false
		return resp, nil
	}
	resp.OpenAIBody = wrapped
	resp.FinishReason, resp.HasContent = responsesOutcome(wrapped)
	return resp, nil
}

// openAIOutboundBody copies the inbound body, drops the streaming controls the
// router cannot buffer, and pins the resolved downstream model name.
func openAIOutboundBody(req Request) map[string]any {
	body := make(map[string]any, len(req.Body)+1)
	for k, v := range req.Body {
		if k == "stream" || k == "stream_options" {
			continue
		}
		body[k] = v
	}
	body["model"] = req.Model
	return body
}

func postOpenAI(ctx context.Context, client *http.Client, url, apiKey string, body map[string]any) (*Response, error) {
	out := mustJSON(body)
	headers := map[string]string{"Authorization": "Bearer " + apiKey}
	status, raw, err := doJSON(ctx, client, http.MethodPost, url, headers, out)
	return &Response{
		HTTPStatus:   status,
		OutboundURL:  url,
		OutboundBody: out,
		RawResponse:  raw,
		OpenAIBody:   raw,
	}, err
}

func openAIChatOutcome(raw []byte) (finish string, hasContent bool) {
	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   any `json:"content"`
				ToolCalls any `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 {
		return "", false
	}
	c := parsed.Choices[0]
	hasContent = contentToText(c.Message.Content) != "" || c.Message.ToolCalls != nil
	return c.FinishReason, hasContent
}

func embedHasContent(raw []byte) bool {
	var parsed struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false
	}
	return len(parsed.Data) > 0
}
