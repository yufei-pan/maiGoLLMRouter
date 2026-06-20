package main

import (
	"strings"
	"testing"
)

func TestRenderSystemdUnit(t *testing.T) {
	unit := renderSystemdUnit(systemdUnitParams{
		ExecStart:        `/opt/maiGoLLMRouter -config /etc/maiGoLLMRouter/config.toml`,
		WorkingDirectory: `/etc/maiGoLLMRouter`,
		ConfigPath:       `/etc/maiGoLLMRouter/config.toml`,
		User:             `router`,
		Group:            `router`,
	})

	for _, want := range []string{
		"[Unit]",
		"Description=maiGo LLM Router",
		"[Service]",
		"Type=simple",
		"User=router",
		"Group=router",
		"WorkingDirectory=/etc/maiGoLLMRouter",
		"ExecStart=/opt/maiGoLLMRouter -config /etc/maiGoLLMRouter/config.toml",
		"ExecReload=/bin/kill -HUP $MAINPID",
		"Restart=on-failure",
		"[Install]",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
}

func TestSystemdQuote(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/usr/bin/maiGoLLMRouter", "/usr/bin/maiGoLLMRouter"},
		{`/path/with spaces/bin`, `"/path/with spaces/bin"`},
		{`/path/with"quote`, `"/path/with\"quote"`},
	}
	for _, tc := range tests {
		if got := systemdQuote(tc.in); got != tc.want {
			t.Errorf("systemdQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
