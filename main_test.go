package main

import (
	"bytes"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"maiGoLLMRouter/config"
	"maiGoLLMRouter/inflight"
	"maiGoLLMRouter/logstore"
	"maiGoLLMRouter/router"
	"maiGoLLMRouter/server"
)

// writeConfig writes content to a config file in a new temp dir and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	rewriteConfig(t, path, content)
	return path
}

func rewriteConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

// captureLog sends the standard logger to a buffer until the test ends.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func newTestReloader(path string, cfg *config.Config) *configReloader {
	rt := router.New(cfg)
	return newConfigReloader(path, cfg, rt, server.New(cfg, rt, nil, nil), nil)
}

func TestAPIListenerServesOnlyAPI(t *testing.T) {
	cfg := loadConfig(t, writeConfig(t, `
[server]
client_keys = ["sk-test"]
`))
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.Write(logstore.Entry{Time: "2026-11-09T12:00:00Z", ID: "entry1", Endpoint: "/v1/chat/completions", InboundModel: "m", Status: 200, Success: true}); err != nil {
		t.Fatal(err)
	}
	recent := logs.Recent()
	if len(recent) != 1 {
		t.Fatalf("want one logged entry, got %d", len(recent))
	}
	active := inflight.New()
	full, api := buildHandlers(server.New(cfg, router.New(cfg), logs, active), logs, active)
	fullTS := httptest.NewServer(full)
	t.Cleanup(fullTS.Close)
	apiTS := httptest.NewServer(api)
	t.Cleanup(apiTS.Close)

	status := func(base, path string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sk-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	for name, base := range map[string]string{"full": fullTS.URL, "api": apiTS.URL} {
		for _, p := range []string{"/healthz", "/v1/models"} {
			if got := status(base, p); got != http.StatusOK {
				t.Errorf("%s %s: status %d, want 200", name, p, got)
			}
		}
	}
	for _, p := range []string{"/ui", "/ui/logs", "/ui/logs/entry?path=" + url.QueryEscape(recent[0].Path), "/ui/inflight", "/ui/x"} {
		if got := status(apiTS.URL, p); got != http.StatusNotFound {
			t.Errorf("api %s: status %d, want 404", p, got)
		}
		if got := status(fullTS.URL, p); got != http.StatusOK {
			t.Errorf("full %s: status %d, want 200", p, got)
		}
	}
}

// freeAddr returns a loopback address nothing listens on right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	return probe.Addr().String()
}

// TestServePairsAnswersUIOnlyOnListen verifies, through the pairing main
// serves and real bound listeners, that the web UI (which has no auth) answers
// on listen and never on api_listen: swapping the two handlers must fail here.
func TestServePairsAnswersUIOnlyOnListen(t *testing.T) {
	listen, apiListen := freeAddr(t), freeAddr(t)
	cfg := loadConfig(t, writeConfig(t, `
[server]
client_keys = ["sk-test"]
listen = "`+listen+`"
api_listen = "`+apiListen+`"
`))
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	active := inflight.New()
	pairs, err := servePairs(cfg, server.New(cfg, router.New(cfg), logs, active), logs, active)
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Fatalf("%d listeners, want listen and api_listen", len(pairs))
	}
	for _, p := range pairs {
		srv := server.NewHTTPServer(p.h, server.DefaultConnLimits)
		go srv.Serve(p.ln)
		t.Cleanup(func() { srv.Close(); p.ln.Close() })
	}

	for addr, want := range map[string]struct{ ui, healthz int }{
		listen:    {http.StatusOK, http.StatusOK},
		apiListen: {http.StatusNotFound, http.StatusOK},
	} {
		for path, wantStatus := range map[string]int{"/ui": want.ui, "/healthz": want.healthz} {
			resp, err := http.Get("http://" + addr + path)
			if err != nil {
				t.Fatalf("GET %s%s: %v", addr, path, err)
			}
			resp.Body.Close()
			if resp.StatusCode != wantStatus {
				t.Errorf("GET %s%s: status %d, want %d", addr, path, resp.StatusCode, wantStatus)
			}
		}
	}
}

func TestListenAllSameAddressFails(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	cfg := loadConfig(t, writeConfig(t, `
[server]
client_keys = ["sk-test"]
listen = "`+addr+`"
api_listen = "`+addr+`"
`))
	full, api, err := listenAll(cfg)
	if err == nil {
		for _, ln := range []net.Listener{full, api} {
			if ln != nil {
				ln.Close()
			}
		}
		t.Fatal("listen and api_listen on one address: want an error")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("error %q does not name %s", err, addr)
	}
	again, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("%s still bound after the failure: %v", addr, err)
	}
	again.Close()

	cfg = loadConfig(t, writeConfig(t, `
[server]
client_keys = ["sk-test"]
listen = "127.0.0.1:0"
`))
	full, api, err = listenAll(cfg)
	if err != nil {
		t.Fatalf("listen alone: %v", err)
	}
	full.Close()
	if api != nil {
		api.Close()
		t.Error("api_listen unset: want no API listener")
	}
}

func TestReloadIgnoresAPIListen(t *testing.T) {
	path := writeConfig(t, `
[server]
client_keys = ["sk-test"]
api_listen = "127.0.0.1:1"
`)
	r := newTestReloader(path, loadConfig(t, path))
	rewriteConfig(t, path, `
[server]
client_keys = ["sk-test"]
api_listen = "127.0.0.1:2"
`)
	logged := captureLog(t)
	r.tryReload()

	if got := r.cfg.Server.APIListen; got != "127.0.0.1:1" {
		t.Errorf("api_listen after reload = %q, want 127.0.0.1:1 kept until a restart", got)
	}
	if want := `api_listen "127.0.0.1:1" -> "127.0.0.1:2" ignored`; !strings.Contains(logged.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logged)
	}
}

func TestReloadLogsNewGeneratedKey(t *testing.T) {
	path := writeConfig(t, `
[server]
client_keys = ["sk-file"]
`)
	r := newTestReloader(path, loadConfig(t, path))
	rewriteConfig(t, path, `
[server]
client_keys = []
`)
	logged := captureLog(t)
	r.tryReload()

	gen := r.cfg.Server.GeneratedClientKey
	if gen == "" {
		t.Fatal("emptying client_keys should generate a key")
	}
	if !strings.Contains(logged.String(), gen) {
		t.Errorf("log lacks the new generated key:\n%s", logged)
	}
	if slices.Contains(r.cfg.Server.ClientKeys, "sk-file") {
		t.Errorf("client_keys still has the file's old key: %v", r.cfg.Server.ClientKeys)
	}

	// The same key carried over by the next reload is not new, so it is not logged again.
	logged.Reset()
	r.tryReload()
	if strings.Contains(logged.String(), gen) {
		t.Errorf("a reload that keeps the generated key logged it again:\n%s", logged)
	}
}
