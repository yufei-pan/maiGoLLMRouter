package provider

import (
	"encoding/json"
	"testing"
)

// MaiBot's wait tool, as it sends it: a top-level oneOf that Anthropic rejects with
// "tools.0.custom.input_schema: input_schema does not support oneOf, allOf, or anyOf
// at the top level".
const waitToolJSON = `[{"type": "function", "function": {
	"name": "wait",
	"description": "seconds or until, not both",
	"parameters": {
		"type": "object",
		"properties": {
			"until": {"type": "string"},
			"seconds": {"type": "integer"},
			"mode": {"anyOf": [{"type": "string"}, {"type": "null"}]}
		},
		"required": ["mode"],
		"oneOf": [
			{"required": ["seconds"], "not": {"required": ["until"]}},
			{"required": ["until"], "not": {"required": ["seconds"]}}
		],
		"anyOf": [{"required": ["seconds"]}, {"required": ["until"]}],
		"allOf": [{"required": ["mode"]}]
	}
}}]`

func TestAnthropicToolsDropTopLevelCombinators(t *testing.T) {
	var tools any
	if err := json.Unmarshal([]byte(waitToolJSON), &tools); err != nil {
		t.Fatal(err)
	}
	out := anthropicTools(tools, nil)
	if len(out) != 1 {
		t.Fatalf("got %d tools, want 1", len(out))
	}
	schema := out[0].(map[string]any)["input_schema"].(map[string]any)
	for _, k := range []string{"oneOf", "anyOf", "allOf"} {
		if _, ok := schema[k]; ok {
			t.Errorf("input_schema still has top-level %s", k)
		}
	}
	if schema["type"] != "object" {
		t.Errorf("type = %v, want object", schema["type"])
	}
	props := schema["properties"].(map[string]any)
	for _, k := range []string{"until", "seconds", "mode"} {
		if _, ok := props[k]; !ok {
			t.Errorf("property %s lost", k)
		}
	}
	if _, ok := props["mode"].(map[string]any)["anyOf"]; !ok {
		t.Error("nested anyOf was removed; Anthropic accepts it below the top level")
	}
	if req, _ := schema["required"].([]any); len(req) != 1 || req[0] != "mode" {
		t.Errorf("required = %v, want [mode]", schema["required"])
	}

	// The caller's request body is left as it was.
	params := tools.([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if _, ok := params["oneOf"]; !ok {
		t.Error("the caller's schema was changed")
	}
}

func TestAnthropicToolsKeepPlainSchema(t *testing.T) {
	var tools any
	if err := json.Unmarshal([]byte(`[{"type": "function", "function": {"name": "f",
		"parameters": {"type": "object", "properties": {"x": {"type": "string"}}, "required": ["x"]}}}]`), &tools); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(anthropicTools(tools, nil)[0].(map[string]any)["input_schema"])
	want := `{"properties":{"x":{"type":"string"}},"required":["x"],"type":"object"}`
	if string(got) != want {
		t.Errorf("input_schema = %s, want %s", got, want)
	}
}
