package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// unitLines splits a rendered unit into its lines.
func unitLines(unit string) []string {
	return strings.Split(unit, "\n")
}

func TestRenderSandboxDirectives(t *testing.T) {
	unit := renderSystemdUnit(systemdUnitParams{
		ExecStart:        `/opt/router/maiGoLLMRouter -config /etc/router/config.toml`,
		ExecPath:         `/opt/router/maiGoLLMRouter`,
		WorkingDirectory: `/etc/router`,
		ConfigPath:       `/etc/router/config.toml`,
		LogDir:           `/var/lib/router/logs`,
		FFmpeg:           `ffmpeg`,
		User:             `router`,
		Group:            `router`,
	})
	lines := unitLines(unit)

	mounts := slices.Index(lines, "RequiresMountsFor=/opt/router/maiGoLLMRouter /etc/router/config.toml /var/lib/router/logs")
	if mounts < 0 || mounts > slices.Index(lines, "[Service]") {
		t.Errorf("[Unit] lacks the RequiresMountsFor line:\n%s", unit)
	}

	// The sandbox follows RestartSec, in the spec's order, with ProtectHome
	// right after ReadWritePaths.
	sandbox := []string{
		"RestartSec=5",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ReadWritePaths=/var/lib/router/logs",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"PrivateDevices=yes",
		"ProtectKernelTunables=yes",
		"ProtectKernelModules=yes",
		"ProtectControlGroups=yes",
		"RestrictNamespaces=yes",
		"LockPersonality=yes",
		"RestrictSUIDSGID=yes",
		"CapabilityBoundingSet=",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
		"SystemCallArchitectures=native",
		"UMask=0077",
	}
	last := -1
	for _, want := range sandbox {
		i := slices.Index(lines, want)
		if i < 0 {
			t.Fatalf("unit missing the line %q:\n%s", want, unit)
		}
		if i <= last {
			t.Fatalf("line %q is out of order:\n%s", want, unit)
		}
		last = i
	}
}

// TestRenderMemoryCap verifies the unit caps the service's memory, so a runaway
// restarts the router instead of starving the host, with swap equal to the cap.
func TestRenderMemoryCap(t *testing.T) {
	lines := unitLines(renderSystemdUnit(systemdUnitParams{ExecPath: "/opt/router/r", ConfigPath: "/etc/router/config.toml"}))
	service, install := slices.Index(lines, "[Service]"), slices.Index(lines, "[Install]")
	for _, want := range []string{"MemoryMax=6G", "MemorySwapMax=6G"} {
		i := slices.Index(lines, want)
		if i < service || i > install {
			t.Errorf("[Service] lacks the whole line %q", want)
		}
	}
}

func TestRenderRequiresMountsQuotes(t *testing.T) {
	unit := renderSystemdUnit(systemdUnitParams{
		ExecPath:   `/opt/my router/r`,
		ConfigPath: `/etc/router/config.toml`,
		LogDir:     `/var/lib/my router/logs`,
	})
	lines := unitLines(unit)
	for _, want := range []string{
		`RequiresMountsFor="/opt/my router/r" /etc/router/config.toml "/var/lib/my router/logs"`,
		`ReadWritePaths="/var/lib/my router/logs"`,
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("unit missing the line %q:\n%s", want, unit)
		}
	}
}

func TestUnderHome(t *testing.T) {
	for path, want := range map[string]bool{
		"/home":         true,
		"/home/u/r":     true,
		"/root":         true,
		"/root/x":       true,
		"/home/":        true,
		"//home/u":      true,
		"/homework/r":   false,
		"/rootfs/r":     false,
		"/opt/home/r":   false,
		"/":             false,
		"":              false,
		"ffmpeg":        false,
		"/usr/../home/": true,
	} {
		if got := underHome(path); got != want {
			t.Errorf("underHome(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestProtectHomeChoice(t *testing.T) {
	base := systemdUnitParams{
		ExecPath:         "/opt/router/maiGoLLMRouter",
		WorkingDirectory: "/etc/router",
		ConfigPath:       "/etc/router/config.toml",
		LogDir:           "/opt/router/logs",
		FFmpeg:           "/opt/ffmpeg/bin/ffmpeg",
	}
	tests := []struct {
		name string
		edit func(*systemdUnitParams)
		want string
	}{
		{"all under /opt or /etc", func(*systemdUnitParams) {}, "yes"},
		// With ProtectHome=yes this binary would fail with 203/EXEC.
		{"binary under /home", func(p *systemdUnitParams) { p.ExecPath = "/home/u/bin/r" }, "read-only"},
		{"working directory under /root", func(p *systemdUnitParams) { p.WorkingDirectory = "/root/x" }, "read-only"},
		{"config under /root", func(p *systemdUnitParams) { p.ConfigPath = "/root/x/config.toml" }, "read-only"},
		{"log dir under /home", func(p *systemdUnitParams) { p.LogDir = "/home/u/logs" }, "read-only"},
		{"ffmpeg under /home", func(p *systemdUnitParams) { p.FFmpeg = "/home/u/ffmpeg" }, "read-only"},
		{"bare ffmpeg", func(p *systemdUnitParams) { p.FFmpeg = "ffmpeg" }, "yes"},
		{"binary under /homework", func(p *systemdUnitParams) { p.ExecPath = "/homework/r" }, "yes"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.edit(&p)
			if got := protectHome(p); got != tc.want {
				t.Errorf("protectHome(%+v) = %q, want %q", p, got, tc.want)
			}
		})
	}
}

func TestGenerateResolvesLogDir(t *testing.T) {
	t.Chdir(t.TempDir())

	t.Run("default log_dir is under the config's dir", func(t *testing.T) {
		cfgPath := writeConfig(t, "[server]\nclient_keys = [\"sk-test\"]\n")
		p, err := resolveSystemdParams(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(filepath.Dir(cfgPath), "logs")
		if p.LogDir != want || p.ConfigError != "" {
			t.Fatalf("LogDir = %q (ConfigError %q), want %q", p.LogDir, p.ConfigError, want)
		}
		if p.FFmpeg != "ffmpeg" {
			t.Errorf("FFmpeg = %q, want the default %q", p.FFmpeg, "ffmpeg")
		}
		if _, err := os.Stat(want); !os.IsNotExist(err) {
			t.Fatalf("log dir exists before writeSystemdUnit: %v", err)
		}

		outPath, err := writeSystemdUnit(p)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(want)
		if err != nil {
			t.Fatalf("log dir not created: %v", err)
		}
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("log dir mode = %v, want a 0700 directory", info.Mode())
		}
		unit, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(unitLines(string(unit)), "ReadWritePaths="+want) {
			t.Errorf("written unit lacks ReadWritePaths=%s:\n%s", want, unit)
		}
	})

	t.Run("absolute log_dir is kept", func(t *testing.T) {
		abs := filepath.Join(t.TempDir(), "abs")
		cfgPath := writeConfig(t, fmt.Sprintf("[server]\nclient_keys = [\"sk-test\"]\nlog_dir = %q\n", abs))
		p, err := resolveSystemdParams(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if p.LogDir != abs {
			t.Fatalf("LogDir = %q, want %q", p.LogDir, abs)
		}
	})
}

func TestGenerateUnreadableConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	cfgPath := writeConfig(t, "[server\n")
	p, err := resolveSystemdParams(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigError == "" || p.LogDir != "" {
		t.Fatalf("ConfigError = %q, LogDir = %q; want an error and no log dir", p.ConfigError, p.LogDir)
	}

	outPath, err := writeSystemdUnit(p)
	if err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := unitLines(string(unit))
	for _, l := range lines {
		if strings.HasPrefix(l, "ReadWritePaths=") {
			t.Errorf("unit has %q although the config could not be read", l)
		}
	}
	const comment = "# ReadWritePaths=<log_dir>: add the absolute log directory; the config could not be read when this unit was generated"
	if !slices.Contains(lines, comment) {
		t.Errorf("unit lacks the ReadWritePaths comment:\n%s", unit)
	}

	notes := systemdNotes(p)
	if !slices.ContainsFunc(notes, func(n string) bool {
		return strings.Contains(n, "Could not read the config ("+p.ConfigError+")")
	}) {
		t.Errorf("notes lack the config error: %q", notes)
	}
}

func TestRootNote(t *testing.T) {
	const rootNote = `Running as root. A dedicated user is safer: see "Running as a dedicated user" in the README.`
	if notes := systemdNotes(systemdUnitParams{User: "root"}); !slices.Contains(notes, rootNote) {
		t.Errorf("root notes = %q, want the dedicated-user line", notes)
	}
	if notes := systemdNotes(systemdUnitParams{User: "router"}); len(notes) != 0 {
		t.Errorf("notes for a dedicated user with a readable config = %q, want none", notes)
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
