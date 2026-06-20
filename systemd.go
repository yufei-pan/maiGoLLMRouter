package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

const systemdUnitFilename = "mai-go-llm-router.service"

// systemdUnitParams holds resolved paths and identity for a generated unit.
type systemdUnitParams struct {
	ExecStart         string
	WorkingDirectory  string
	ConfigPath        string
	User              string
	Group             string
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

	return systemdUnitParams{
		ExecStart:        execStart,
		WorkingDirectory: workDir,
		ConfigPath:       cfgAbs,
		User:             username,
		Group:            group,
	}, nil
}

func renderSystemdUnit(p systemdUnitParams) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=maiGo LLM Router (OpenAI-compatible API router)\n")
	b.WriteString("Documentation=https://github.com/yufei-pan/maiGoLLMRouter\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")

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
	b.WriteString("RestartSec=5\n\n")

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

func writeSystemdUnit(configPath string) (string, error) {
	params, err := resolveSystemdParams(configPath)
	if err != nil {
		return "", err
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
