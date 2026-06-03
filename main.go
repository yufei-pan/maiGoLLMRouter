// Command maiGoLLMRouter is a lightweight OpenAI-compatible API router that
// translates requests to OpenAI/Anthropic/Gemini providers with multi-key
// selection, fallbacks, blackout, output verification, and JSONL logging.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"maiGoLLMRouter/config"
	"maiGoLLMRouter/logstore"
	"maiGoLLMRouter/router"
	"maiGoLLMRouter/server"
	"maiGoLLMRouter/webui"
)

// version is the build version. Override at build time with:
//
//	go build -ldflags "-X main.version=1.2.3"
var version = "0.1.3"

func main() {
	const defaultConfig = "config.toml"
	var configPath string
	var showVersion bool
	var generateSystemd bool
	flag.StringVar(&configPath, "config", defaultConfig, "path to TOML config file")
	flag.StringVar(&configPath, "f", defaultConfig, "path to TOML config file (shorthand for --config)")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&showVersion, "V", false, "print version and exit (shorthand for --version)")
	flag.BoolVar(&generateSystemd, "generate-systemd", false, "write ./mai-go-llm-router.service and print install instructions")
	flag.Usage = usage
	flag.Parse()

	if showVersion {
		fmt.Printf("maiGoLLMRouter %s\n", version)
		return
	}

	if generateSystemd {
		outPath, err := writeSystemdUnit(configPath)
		if err != nil {
			log.Fatalf("generate-systemd: %v", err)
		}
		printSystemdInstallInstructions(outPath)
		return
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		// Give actionable guidance when the config file is simply missing.
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "config file %q not found.\n\n", configPath)
			configHelp(os.Stderr)
			os.Exit(1)
		}
		log.Fatalf("config: %v", err)
	}

	logs, err := logstore.New(cfg.Server.LogDir)
	if err != nil {
		log.Fatalf("logstore: %v", err)
	}

	rt := router.New(cfg)
	srv := server.New(cfg, rt, logs)

	mux := http.NewServeMux()
	srv.Register(mux)
	webui.Register(mux, logs)

	httpServer := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	logStartup(cfg, configPath)
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// logStartup reports the inbound endpoint, API type, key sources, and config
// location at startup.
func logStartup(cfg *config.Config, configPath string) {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		abs = configPath
	}
	base := "http://" + cfg.Server.Listen
	if strings.HasPrefix(cfg.Server.Listen, ":") {
		base = "http://localhost" + cfg.Server.Listen
	}

	log.Printf("maiGoLLMRouter %s", version)
	log.Printf("config loaded from %s", abs)
	log.Printf("inbound API type: OpenAI-compatible (bearer auth)")
	log.Printf("inbound endpoint: %s/v1  (POST /v1/chat/completions, POST /v1/embeddings, GET /v1/models)", base)

	if cfg.Server.GeneratedClientKey != "" {
		log.Printf("client auth: no client_keys in config; generated one for this run:")
		log.Printf("    %s", cfg.Server.GeneratedClientKey)
	} else {
		log.Printf("client auth: %d client key(s) read from config file (%s)", len(cfg.Server.ClientKeys), abs)
	}

	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	totalKeys := 0
	for _, n := range names {
		p := cfg.Providers[n]
		totalKeys += len(p.Keys) + len(p.FallbackKeys)
		parts = append(parts, fmt.Sprintf("%s[%s, %d keys, %d fallback]", n, p.Kind, len(p.Keys), len(p.FallbackKeys)))
	}
	log.Printf("providers: %d, %d provider API key(s) read from config file", len(cfg.Providers), totalKeys)
	for _, pt := range parts {
		log.Printf("    - %s", pt)
	}
	log.Printf("models: %d", len(cfg.Models))
	log.Printf("web UI: %s/ui", base)
}

func usage() {
	w := flag.CommandLine.Output()
	fmt.Fprintf(w, "maiGoLLMRouter %s - OpenAI-compatible LLM API router\n\n", version)
	fmt.Fprintf(w, "Usage:\n  %s [-f|--config <path>] [-V|--version] [--generate-systemd]   (config default: config.toml)\n\n", os.Args[0])
	fmt.Fprintf(w, "Flags:\n")
	flag.PrintDefaults()
	fmt.Fprintln(w)
	configHelp(w)
}

// configHelp prints a reminder of the essential config fields and where to get
// provider API keys.
func configHelp(w io.Writer) {
	fmt.Fprint(w, `Each provider needs (see config.example.toml for the full format):

  [[provider]]
  name     = "openai"                     # used in routing as <name>/<model>
  kind     = "openai"                     # dialect: "openai" | "anthropic" | "gemini"
  base_url = "https://api.openai.com/v1"  # INCLUDE the version segment (e.g. /v1, /v1beta, /api/v1)
  keys     = ["sk-..."]                   # one or more provider API keys

Where to get provider API keys:
  openai      -> https://platform.openai.com/api-keys        (kind = "openai")
  anthropic   -> https://console.anthropic.com/settings/keys (kind = "anthropic")
  gemini      -> https://aistudio.google.com/app/apikey      (kind = "gemini")
  openrouter  -> https://openrouter.ai/keys                  (kind = "openai")

Inbound auth (the bearer token clients send to THIS router):
  set [server].client_keys = ["sk-..."], or leave it empty to have one
  generated and printed at startup.
`)
}
