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
		{"log dir as written under /root, resolved elsewhere", func(p *systemdUnitParams) { p.LogDirAsWritten = "/root/x/logs" }, "read-only"},
		{"ffmpeg as written under /home, resolved elsewhere", func(p *systemdUnitParams) { p.FFmpegAsWritten = "/home/u/ffmpeg" }, "read-only"},
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

// TestProtectHomeJudgesPathsAsWritten verifies ProtectHome= is judged from the
// log_dir and ffmpeg as the config writes them as well as from their real
// paths: the service opens them as written, and a link from a home directory
// to a disk elsewhere leaves nothing behind it once the home directories are
// hidden.
func TestProtectHomeJudgesPathsAsWritten(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home") // stands in for /home
	disk, etc := filepath.Join(root, "disk"), filepath.Join(root, "etc")
	for _, d := range []string{home, disk, etc} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// home/x and home/ffmpeg are links out of home, to files on another disk.
	if err := os.Symlink(disk, filepath.Join(home, "x")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(disk, "ffmpeg"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(disk, "ffmpeg"), filepath.Join(home, "ffmpeg")); err != nil {
		t.Fatal(err)
	}
	old := homeDirs
	homeDirs = []string{home}
	t.Cleanup(func() { homeDirs = old })

	for _, tc := range []struct{ name, logDir, ffmpeg, want string }{
		{"log_dir written under home, really elsewhere", filepath.Join(home, "x", "logs"), "ffmpeg", "read-only"},
		{"ffmpeg written under home, really elsewhere", filepath.Join(disk, "logs"), filepath.Join(home, "ffmpeg"), "read-only"},
		{"nothing under home", filepath.Join(disk, "logs"), filepath.Join(disk, "ffmpeg"), "yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := filepath.Join(etc, "config.toml")
			rewriteConfig(t, cfgPath, fmt.Sprintf("[server]\nclient_keys = [\"sk-test\"]\nlog_dir = %q\nffmpeg = %q\n", tc.logDir, tc.ffmpeg))
			p, err := resolveSystemdParams(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if underHome(p.LogDir) || underHome(p.FFmpeg) {
				t.Fatalf("setup: the resolved paths %q and %q should be outside home", p.LogDir, p.FFmpeg)
			}
			unit := renderSystemdUnit(p)
			lines := unitLines(unit)
			if !slices.Contains(lines, "ProtectHome="+tc.want) {
				t.Errorf("want ProtectHome=%s:\n%s", tc.want, unit)
			}
			// systemd resolves ReadWritePaths= on the host: the real path stays.
			if !slices.Contains(lines, "ReadWritePaths="+filepath.Join(disk, "logs")) {
				t.Errorf("ReadWritePaths= is not the resolved log dir:\n%s", unit)
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

// TestGenerateResolvesSymlinks verifies a run from a symlinked directory
// writes the real paths into the unit: a dedicated user may be unable to
// traverse the link (a mode-700 home), and ProtectHome must judge where the
// files really are.
func TestGenerateResolvesSymlinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home") // stands in for /home: the real files are under it, the link is not
	realDir := filepath.Join(home, "svc")
	link := filepath.Join(root, "link")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "ffmpeg"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	old := homeDirs
	homeDirs = []string{home}
	t.Cleanup(func() { homeDirs = old })

	for name, cfg := range map[string]string{
		"relative log_dir": fmt.Sprintf("[server]\nclient_keys = [\"sk-test\"]\nffmpeg = %q\n", filepath.Join(link, "ffmpeg")),
		"absolute log_dir that doesn't exist yet": fmt.Sprintf("[server]\nclient_keys = [\"sk-test\"]\nlog_dir = %q\nffmpeg = %q\n",
			filepath.Join(link, "new", "logs"), filepath.Join(link, "ffmpeg")),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(realDir, "config.toml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			p, err := resolveSystemdParams(filepath.Join(link, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			wantLogDir := filepath.Join(realDir, "logs")
			if strings.Contains(name, "absolute") {
				wantLogDir = filepath.Join(realDir, "new", "logs")
			}
			if p.WorkingDirectory != realDir || p.ConfigPath != filepath.Join(realDir, "config.toml") ||
				p.LogDir != wantLogDir || p.FFmpeg != filepath.Join(realDir, "ffmpeg") {
				t.Errorf("params = %+v, want the paths under %s", p, realDir)
			}
			unit := renderSystemdUnit(p)
			if strings.Contains(unit, link) {
				t.Errorf("unit holds the symlink path %s:\n%s", link, unit)
			}
			if !slices.Contains(unitLines(unit), "ProtectHome=read-only") {
				t.Errorf("ProtectHome was not judged from the real paths:\n%s", unit)
			}
		})
	}
}

// TestGenerateKeepsConfigLinkDir verifies a config file that is itself a link
// keeps its own directory as the working directory, where the router was run
// and where its relative log_dir points; only -config names the real file.
func TestGenerateKeepsConfigLinkDir(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	realCfg := filepath.Join(b, "real.toml")
	rewriteConfig(t, realCfg, "[server]\nclient_keys = [\"sk-test\"]\nlog_dir = \"logs\"\n")
	if err := os.Symlink(realCfg, filepath.Join(a, "config.toml")); err != nil {
		t.Fatal(err)
	}
	aLink := filepath.Join(root, "alink") // a itself reached through a link
	if err := os.Symlink(a, aLink); err != nil {
		t.Fatal(err)
	}

	for name, cfgPath := range map[string]string{
		"config link in a real directory":   filepath.Join(a, "config.toml"),
		"config link in a linked directory": filepath.Join(aLink, "config.toml"),
	} {
		t.Run(name, func(t *testing.T) {
			p, err := resolveSystemdParams(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			unit := renderSystemdUnit(p)
			lines := unitLines(unit)
			for _, want := range []string{
				"WorkingDirectory=" + a,
				"ReadWritePaths=" + filepath.Join(a, "logs"),
			} {
				if !slices.Contains(lines, want) {
					t.Errorf("unit lacks %q:\n%s", want, unit)
				}
			}
			if p.ConfigPath != realCfg || !strings.HasSuffix(p.ExecStart, " -config "+realCfg) {
				t.Errorf("-config should name the real file %s: ConfigPath %q, ExecStart %q", realCfg, p.ConfigPath, p.ExecStart)
			}
		})
	}
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
