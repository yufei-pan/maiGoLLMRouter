package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesViaGeminiPortable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ":generateContent") {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") != "k" {
			t.Fatalf("missing gemini api key header")
		}
		fmt.Fprint(w, `{
		  "candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP"}],
		  "usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}
		}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", Request{
		Op: OpResponses, Model: "gemini-2.5-flash",
		Body: map[string]any{"model": "in", "input": "hello", "instructions": "be brief"},
	})
	if err != nil || !resp.OK() || !resp.HasContent {
		t.Fatalf("err=%v resp=%+v body=%s", err, resp, resp.OpenAIBody)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish=%q, want stop", resp.FinishReason)
	}
	var parsed map[string]any
	if err := json.Unmarshal(resp.OpenAIBody, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["object"] != "response" || parsed["status"] != "completed" {
		t.Fatalf("body=%s", resp.OpenAIBody)
	}
	usage, _ := parsed["usage"].(map[string]any)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(1) {
		t.Fatalf("usage=%v", usage)
	}
}

func TestResponsesViaGeminiNonPortable(t *testing.T) {
	resp, err := Call(context.Background(), http.DefaultClient, "gemini", "http://127.0.0.1:0", "k", Request{
		Op: OpResponses, Model: "gemini-2.5-flash",
		Body: map[string]any{
			"input": "hi",
			"tools": []any{map[string]any{"type": "web_search"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Incompatible {
		t.Fatalf("want incompatible, got %+v", resp)
	}
}

func TestResponsesViaGeminiMapsToolsSchemaAndImages(t *testing.T) {
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	_, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", Request{
		Op: OpResponses, Model: "gemini-2.5-flash",
		Body: map[string]any{
			"input": []any{map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "see"},
					map[string]any{"type": "input_image", "image_url": "data:image/png;base64,BBBB"},
				},
			}},
			"tools": []any{map[string]any{
				"type": "function", "name": "lookup", "description": "d",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"q": map[string]any{"type": "string"},
					},
					"additionalProperties": false,
				},
			}},
			"tool_choice":       "required",
			"max_output_tokens": float64(64),
			"text": map[string]any{"format": map[string]any{
				"type": "json_schema", "name": "Out",
				"schema": map[string]any{"type": "object", "properties": map[string]any{}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := outbound["response_format"]; ok {
		t.Fatalf("chat response_format leaked: %v", outbound)
	}
	gen, _ := outbound["generationConfig"].(map[string]any)
	if gen["responseMimeType"] != "application/json" {
		t.Fatalf("generationConfig=%v", gen)
	}
	if gen["maxOutputTokens"] != float64(64) {
		t.Fatalf("maxOutputTokens=%v", gen["maxOutputTokens"])
	}
	if _, ok := gen["response_format"]; ok {
		t.Fatalf("response_format leaked into generationConfig: %v", gen)
	}
	if gen["responseSchema"] == nil {
		t.Fatalf("expected responseSchema, got %v", gen)
	}

	tools, _ := outbound["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", outbound["tools"])
	}
	tool := tools[0].(map[string]any)
	decls, _ := tool["functionDeclarations"].([]any)
	if len(decls) != 1 {
		t.Fatalf("functionDeclarations=%v", tool)
	}
	decl := decls[0].(map[string]any)
	params, _ := decl["parameters"].(map[string]any)
	if _, leaked := params["additionalProperties"]; leaked {
		t.Fatalf("additionalProperties must be stripped for Gemini: %v", params)
	}

	tc, _ := outbound["toolConfig"].(map[string]any)
	fcc, _ := tc["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "ANY" {
		t.Fatalf("toolConfig=%v", outbound["toolConfig"])
	}

	raw, _ := json.Marshal(outbound["contents"])
	if !strings.Contains(string(raw), "inlineData") || !strings.Contains(string(raw), "BBBB") {
		t.Fatalf("image missing from gemini contents: %s", raw)
	}
}

func TestResponsesViaGeminiFunctionCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
		  "candidates":[{
		    "content":{"role":"model","parts":[
		      {"functionCall":{"name":"lookup","args":{"q":"paris"}}}
		    ]},
		    "finishReason":"STOP"
		  }]
		}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", Request{
		Op: OpResponses, Model: "gemini-2.5-flash",
		Body: map[string]any{
			"input": "weather?",
			"tools": []any{map[string]any{"type": "function", "name": "lookup"}},
		},
	})
	if err != nil || !resp.OK() || !resp.HasContent {
		t.Fatalf("err=%v resp=%+v body=%s", err, resp, resp.OpenAIBody)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish=%q, want tool_calls", resp.FinishReason)
	}
	var parsed map[string]any
	if err := json.Unmarshal(resp.OpenAIBody, &parsed); err != nil {
		t.Fatal(err)
	}
	output, _ := parsed["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%s", resp.OpenAIBody)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "function_call" || item["name"] != "lookup" {
		t.Fatalf("item=%v", item)
	}
	args, _ := item["arguments"].(string)
	if !strings.Contains(args, "paris") {
		t.Fatalf("arguments=%v", args)
	}
	if parsed["status"] != "completed" {
		t.Fatalf("status=%v", parsed["status"])
	}
}

func TestResponsesViaGeminiThoughtPartsAreNotVisibleText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
		  "candidates":[{
		    "content":{"role":"model","parts":[
		      {"text":"I should answer briefly.","thought":true},
		      {"text":"hello"}
		    ]},
		    "finishReason":"STOP"
		  }]
		}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", Request{
		Op: OpResponses, Model: "gemini-2.5-flash",
		Body: map[string]any{"input": "hi"},
	})
	if err != nil || !resp.OK() {
		t.Fatalf("err=%v resp=%+v body=%s", err, resp, resp.OpenAIBody)
	}
	raw := string(resp.OpenAIBody)
	if strings.Contains(raw, "I should answer briefly.") {
		t.Fatalf("thought text leaked into Responses output: %s", raw)
	}
	if !strings.Contains(raw, `"hello"`) {
		t.Fatalf("visible answer missing: %s", raw)
	}
}

func TestResponsesViaGeminiThoughtSignatureRoundtrip(t *testing.T) {
	var outbound map[string]any
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &outbound)
		step++
		if step == 1 {
			fmt.Fprint(w, `{
			  "candidates":[{
			    "content":{"role":"model","parts":[
			      {"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"sig-abc"}
			    ]},
			    "finishReason":"STOP"
			  }]
			}`)
			return
		}
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"done"}],"role":"model"},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	first, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", Request{
		Op: OpResponses, Model: "gemini-2.5-flash",
		Body: map[string]any{
			"input": "q",
			"tools": []any{map[string]any{"type": "function", "name": "lookup"}},
		},
	})
	if err != nil || !first.OK() {
		t.Fatalf("first: err=%v body=%s", err, first.OpenAIBody)
	}
	var parsed map[string]any
	if err := json.Unmarshal(first.OpenAIBody, &parsed); err != nil {
		t.Fatal(err)
	}
	output, _ := parsed["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%s", first.OpenAIBody)
	}
	fc, _ := output[0].(map[string]any)
	callID, _ := fc["call_id"].(string)
	if callID == "" {
		t.Fatalf("missing call_id: %v", fc)
	}
	sig, _ := fc["thought_signature"].(string)
	if sig == "" {
		sig, _ = fc["thoughtSignature"].(string)
	}

	_, err = Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", Request{
		Op: OpResponses, Model: "gemini-2.5-flash",
		Body: map[string]any{
			"input": []any{
				map[string]any{"role": "user", "content": "q"},
				fc,
				map[string]any{"type": "function_call_output", "call_id": callID, "output": `{"ok":true}`},
			},
			"tools": []any{map[string]any{"type": "function", "name": "lookup"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(outbound)
	if !strings.Contains(string(raw), "sig-abc") && sig == "" {
		t.Fatalf("thought signature was dropped on the Gemini roundtrip: %s", raw)
	}
	if !strings.Contains(string(raw), "sig-abc") {
		t.Fatalf("thought signature not forwarded to Gemini: %s", raw)
	}
}

func TestResponsesViaGeminiDropsOpenAIOnlyExtras(t *testing.T) {
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	_, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", Request{
		Op: OpResponses, Model: "gemini-2.5-flash",
		Body: map[string]any{
			"input":               "hi",
			"parallel_tool_calls": true,
			"metadata":            map[string]any{"src": "test"},
			"service_tier":        "default",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := outbound["generationConfig"].(map[string]any)
	for _, k := range []string{"parallel_tool_calls", "metadata", "service_tier"} {
		if _, ok := gen[k]; ok {
			t.Fatalf("%s leaked into generationConfig: %v", k, gen)
		}
		if _, ok := outbound[k]; ok {
			t.Fatalf("%s leaked onto Gemini body: %v", k, outbound)
		}
	}
}

func TestResponsesViaGeminiFunctionCallFollowup(t *testing.T) {
	var outbound map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&outbound)
		fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"22c"}],"role":"model"},"finishReason":"STOP"}]}`)
	}))
	defer srv.Close()

	resp, err := Call(context.Background(), srv.Client(), "gemini", srv.URL, "k", Request{
		Op: OpResponses, Model: "gemini-2.5-flash",
		Body: map[string]any{
			"input": []any{
				map[string]any{"role": "user", "content": "temp?"},
				map[string]any{"type": "function_call", "call_id": "c1", "name": "lookup", "arguments": `{"q":"paris"}`},
				map[string]any{"type": "function_call_output", "call_id": "c1", "output": `{"temp":22}`},
			},
			"tools": []any{map[string]any{"type": "function", "name": "lookup"}},
		},
	})
	if err != nil || !resp.OK() {
		t.Fatalf("err=%v body=%s", err, resp.OpenAIBody)
	}
	raw, _ := json.Marshal(outbound["contents"])
	if !strings.Contains(string(raw), `"functionCall"`) {
		t.Fatalf("replayed functionCall missing: %s", raw)
	}
	if !strings.Contains(string(raw), `"functionResponse"`) {
		t.Fatalf("functionResponse missing: %s", raw)
	}
	if !strings.Contains(string(raw), `"lookup"`) {
		t.Fatalf("function name missing: %s", raw)
	}
}
