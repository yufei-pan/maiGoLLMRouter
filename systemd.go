package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"maiGoLLMRouter/config"
)

const systemdUnitFilename = "mai-go-llm-router.service"

// systemdUnitParams holds resolved paths and identity for a generated unit.
type systemdUnitParams struct {
	ExecStart        string
	ExecPath         string // absolute binary path
	WorkingDirectory string
	ConfigPath       string
	LogDir           string // absolute; "" when the config could not be read
	FFmpeg           string // the config's ffmpeg value
	ConfigError      string // why the config could not be read; "" when it was
	User             string
	Group            string
}

// underHome reports whether path is /home, /root, or below either: the
// directories ProtectHome= hides ("/homework" is not one of them).
func underHome(path string) bool {
	path = filepath.Clean(path)
	for _, home := range []string{"/home", "/root"} {
		if path == home || strings.HasPrefix(path, home+"/") {
			return true
		}
	}
	return false
}

// protectHome is the unit's ProtectHome= value: "yes" hides the home
// directories, so anything the service uses there (a binary there fails with
// 203/EXEC) needs "read-only" instead. A bare ffmpeg is found on systemd's
// PATH, which has no home directory.
func protectHome(p systemdUnitParams) string {
	paths := []string{p.ExecPath, p.WorkingDirectory, p.ConfigPath, p.LogDir}
	if filepath.IsAbs(p.FFmpeg) {
		paths = append(paths, p.FFmpeg)
	}
	for _, path := range paths {
		if underHome(path) {
			return "read-only"
		}
	}
	return "yes"
}

// systemdNotes are the lines printed after the install instructions.
func systemdNotes(p systemdUnitParams) []string {
	var notes []string
	if p.User == "root" {
		notes = append(notes, `Running as root. A dedicated user is safer: see "Running as a dedicated user" in the README.`)
	}
	if p.ConfigError != "" {
		notes = append(notes, fmt.Sprintf("Could not read the config (%s); add ReadWritePaths=<log_dir> to the unit by hand.", p.ConfigError))
	}
	return notes
}

func resolveSystemdParams(configPath string) (systemdUnitParams, error) {
	execPath, err := os.Executable()
	if err != nil {
		execPath = os.Args[0]
	}
	execAbs, err := filepath.Abs(execPath)
	if err != nil {
		return systemdUnitParams{}, fmt.Errorf("executable path: %w", err)
	}

	cfgAbs, err := filepath.Abs(configPath)
	if err != nil {
		return systemdUnitParams{}, fmt.Errorf("config path: %w", err)
	}

	workDir := filepath.Dir(cfgAbs)

	u, err := user.Current()
	username := ""
	group := ""
	if err == nil {
		username = u.Username
		group = u.Username
		if u.Gid != "" {
			if g, gerr := user.LookupGroupId(u.Gid); gerr == nil {
				group = g.Name
			}
		}
	}

	cfgQuoted := systemdQuote(cfgAbs)
	execQuoted := systemdQuote(execAbs)
	execStart := fmt.Sprintf("%s -config %s", execQuoted, cfgQuoted)

	p := systemdUnitParams{
		ExecStart:        execStart,
		ExecPath:         execAbs,
		WorkingDirectory: workDir,
		ConfigPath:       cfgAbs,
		User:             username,
		Group:            group,
	}

	// The same parser the service uses, so the unit opens the log directory
	// the service will write.
	cfg, err := config.Load(cfgAbs)
	if err != nil {
		p.ConfigError = err.Error()
		return p, nil
	}
	p.FFmpeg = cfg.Server.FFmpeg
	// The service resolves a relative log_dir against its working directory.
	logDir := cfg.Server.LogDir
	if !filepath.IsAbs(logDir) {
		logDir = filepath.Join(workDir, logDir)
	}
	p.LogDir = filepath.Clean(logDir)
	return p, nil
}

// sandboxDirectives are the rest of the sandbox, after ProtectHome=. Only
// ReadWritePaths= is needed for writing: the router writes nothing outside
// its log directory, and ffmpeg works on pipes.
var sandboxDirectives = []string{
	"PrivateTmp=yes",
	"PrivateDevices=yes",
	"ProtectKernelTunables=yes",
	"ProtectKernelModules=yes",
	"ProtectControlGroups=yes",
	"RestrictNamespaces=yes",
	"LockPersonality=yes",
	"RestrictSUIDSGID=yes",
	"CapabilityBoundingSet=", // none: the router's ports need no capability
	"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
	"SystemCallArchitectures=native",
	"UMask=0077",
}

func renderSystemdUnit(p systemdUnitParams) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=maiGo LLM Router (OpenAI-compatible API router)\n")
	b.WriteString("Documentation=https://github.com/yufei-pan/maiGoLLMRouter\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	// So a unit on a separate disk waits for it.
	var mounts []string
	for _, path := range []string{p.ExecPath, p.ConfigPath, p.LogDir} {
		if path != "" {
			mounts = append(mounts, systemdQuote(path))
		}
	}
	if len(mounts) > 0 {
		fmt.Fprintf(&b, "RequiresMountsFor=%s\n", strings.Join(mounts, " "))
	}
	b.WriteString("\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	if p.User != "" {
		fmt.Fprintf(&b, "User=%s\n", p.User)
	}
	if p.Group != "" {
		fmt.Fprintf(&b, "Group=%s\n", p.Group)
	}
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", p.WorkingDirectory)
	fmt.Fprintf(&b, "ExecStart=%s\n", p.ExecStart)
	fmt.Fprintf(&b, "ExecReload=/bin/kill -HUP $MAINPID\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("NoNewPrivileges=yes\n")
	b.WriteString("ProtectSystem=strict\n")
	if p.LogDir != "" {
		fmt.Fprintf(&b, "ReadWritePaths=%s\n", systemdQuote(p.LogDir))
	} else {
		b.WriteString("# ReadWritePaths=<log_dir>: add the absolute log directory; the config could not be read when this unit was generated\n")
	}
	fmt.Fprintf(&b, "ProtectHome=%s\n", protectHome(p))
	for _, d := range sandboxDirectives {
		b.WriteString(d + "\n")
	}
	b.WriteString("\n")

	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String()
}

// systemdQuote wraps a path in double quotes if it contains spaces or special chars.
func systemdQuote(s string) string {
	if s == "" {
		return `""`
	}
	needsQuote := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '"' || r == '\\' || r == '$' {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return s
	}
	var escaped strings.Builder
	escaped.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			escaped.WriteByte('\\')
			escaped.WriteRune(r)
		default:
			escaped.WriteRune(r)
		}
	}
	escaped.WriteByte('"')
	return escaped.String()
}

// writeSystemdUnit creates the log directory, since ReadWritePaths= on a
// missing path fails the unit with 226/NAMESPACE, then writes the unit to the
// current directory.
func writeSystemdUnit(params systemdUnitParams) (string, error) {
	if params.LogDir != "" {
		// 0700, as the log store makes its own directories.
		if err := os.MkdirAll(params.LogDir, 0o700); err != nil {
			return "", fmt.Errorf("log directory: %w", err)
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	outPath := filepath.Join(cwd, systemdUnitFilename)

	content := renderSystemdUnit(params)
	if err := os.WriteFile(outPath, []byte(content), 0o644); err != nil {
		return "", err
	}
	return outPath, nil
}

func printSystemdInstallInstructions(outPath string) {
	unitName := systemdUnitFilename
	fmt.Printf("Wrote %s\n\n", outPath)
	fmt.Println("Install and start the service:")
	fmt.Println()
	fmt.Printf("  sudo cp %s /etc/systemd/system/\n", unitName)
	fmt.Println("  sudo systemctl daemon-reload")
	fmt.Printf("  sudo systemctl enable --now %s\n", strings.TrimSuffix(unitName, ".service"))
	fmt.Println()
	fmt.Println("Useful commands:")
	fmt.Printf("  systemctl status %s\n", strings.TrimSuffix(unitName, ".service"))
	fmt.Printf("  systemctl reload %s   # reload config (SIGHUP)\n", strings.TrimSuffix(unitName, ".service"))
	fmt.Printf("  journalctl -u %s -f\n", strings.TrimSuffix(unitName, ".service"))
	fmt.Println()
	fmt.Println("Edit User=/Group= in the unit file if the service should not run as your current login user.")
	fmt.Println()
	fmt.Println("Re-run with --generate-systemd after moving the binary and config to their final paths so ExecStart and WorkingDirectory are correct.")
}
