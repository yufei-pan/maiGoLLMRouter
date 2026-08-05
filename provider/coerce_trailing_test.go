package provider

import (
	"reflect"
	"testing"
)

func TestCoerceTrailingAssistantTurnFlipsRoleAndPronouns(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": "I think my mine 我 我的"},
		},
	}
	out := CoerceTrailingAssistantTurn(body, "messages")
	msgs := out["messages"].([]any)
	last := msgs[1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("role=%v", last["role"])
	}
	if last["content"] != "you think your yours 你 你的" {
		t.Fatalf("content=%q", last["content"])
	}
	// Original not mutated.
	orig := body["messages"].([]any)[1].(map[string]any)
	if orig["role"] != "assistant" || orig["content"] != "I think my mine 我 我的" {
		t.Fatalf("inbound mutated: %#v", orig)
	}
}

func TestCoerceTrailingAssistantTurnLeavesUserToolTyped(t *testing.T) {
	cases := []struct {
		name  string
		field string
		items []any
	}{
		{"user", "messages", []any{map[string]any{"role": "user", "content": "I"}}},
		{"tool", "messages", []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "I"},
		}},
		{"typed", "input", []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "f", "arguments": "{}"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]any{c.field: c.items}
			out := CoerceTrailingAssistantTurn(body, c.field)
			if !reflect.DeepEqual(out[c.field], c.items) {
				t.Fatalf("changed: %#v", out[c.field])
			}
			// Pronouns must not be rewritten when role was not flipped.
			last := c.items[len(c.items)-1].(map[string]any)
			if s, ok := last["content"].(string); ok && s != "I" && c.name != "typed" {
				// tool/user keep "I"
			}
			if c.name != "typed" {
				if last["content"] != "I" {
					t.Fatalf("pronoun rewritten without role flip: %#v", last)
				}
			}
		})
	}
}

func TestCoerceTrailingAssistantTurnMultimodal(t *testing.T) {
	body := map[string]any{
		"input": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "I see 我"},
				map[string]any{"type": "input_image", "image_url": "http://x"},
			}},
		},
	}
	out := CoerceTrailingAssistantTurn(body, "input")
	last := out["input"].([]any)[1].(map[string]any)
	parts := last["content"].([]any)
	textPart := parts[0].(map[string]any)
	imgPart := parts[1].(map[string]any)
	if last["role"] != "user" || textPart["text"] != "you see 你" {
		t.Fatalf("text part=%#v role=%v", textPart, last["role"])
	}
	if imgPart["image_url"] != "http://x" {
		t.Fatalf("image mutated: %#v", imgPart)
	}
}

func TestCoerceTrailingAssistantTurnEmptyNoop(t *testing.T) {
	body := map[string]any{"messages": []any{}}
	if CoerceTrailingAssistantTurn(body, "messages")["messages"] == nil {
		t.Fatal("unexpected nil")
	}
	out := CoerceTrailingAssistantTurn(map[string]any{}, "messages")
	if len(out) != 0 {
		t.Fatalf("empty body changed: %#v", out)
	}
}
