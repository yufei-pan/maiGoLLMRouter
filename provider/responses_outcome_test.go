package provider

import "testing"

func TestResponsesOutcome(t *testing.T) {
	cases := []struct {
		name string
		body string
		fr   string
		hc   bool
	}{
		{"text", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`, "stop", true},
		{"tools", `{"status":"completed","output":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}]}`, "tool_calls", true},
		{"reasoning", `{"status":"completed","output":[{"type":"reasoning","summary":[{"text":"think"}]}]}`, "stop", true},
		{"failed", `{"status":"failed","output":[]}`, "", false},
		{"empty", `{"status":"completed","output":[]}`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fr, hc := responsesOutcome([]byte(c.body))
			if fr != c.fr || hc != c.hc {
				t.Fatalf("got (%q,%v) want (%q,%v)", fr, hc, c.fr, c.hc)
			}
		})
	}
}

func TestIsResponsesUnsupported(t *testing.T) {
	if !isResponsesUnsupported(404, []byte(`{}`)) {
		t.Fatal("404")
	}
	if !isResponsesUnsupported(400, []byte(`{"error":{"message":"Unknown path /responses"}}`)) {
		t.Fatal("explicit unknown path")
	}
	if isResponsesUnsupported(401, []byte(`{"error":{"message":"invalid api key"}}`)) {
		t.Fatal("auth must not count as unsupported")
	}
	if isResponsesUnsupported(400, []byte(`{"error":{"message":"missing required parameter: input"}}`)) {
		t.Fatal("valid Responses validation error must not count as unsupported")
	}
}
