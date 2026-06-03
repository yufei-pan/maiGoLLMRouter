package provider

import (
	"context"
	"encoding/json"
	"net/http"
)

// callOpenAI handles the OpenAI dialect, which is a near pass-through: the
// inbound body is forwarded verbatim (with the model name overridden), so all
// extra arguments pass through automatically (#13).
func callOpenAI(ctx context.Context, client *http.Client, baseURL, apiKey string, req Request) (*Response, error) {
	body := make(map[string]any, len(req.Body)+1)
	for k, v := range req.Body {
		body[k] = v
	}
	body["model"] = req.Model

	var path string
	switch req.Op {
	case OpEmbed:
		path = "/embeddings"
	default:
		path = "/chat/completions"
	}
	url := baseURL + path
	out := mustJSON(body)

	headers := map[string]string{"Authorization": "Bearer " + apiKey}
	status, raw, err := doJSON(ctx, client, http.MethodPost, url, headers, out)
	resp := &Response{HTTPStatus: status, OutboundURL: url, OutboundBody: out, RawResponse: raw, OpenAIBody: raw}
	if err != nil {
		return resp, err
	}
	if req.Op == OpChat && resp.OK() {
		resp.FinishReason, resp.HasContent = openAIChatOutcome(raw)
	} else if resp.OK() {
		resp.HasContent = embedHasContent(raw)
	}
	return resp, nil
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
