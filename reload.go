package main

import (
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"maiGoLLMRouter/config"
	"maiGoLLMRouter/logstore"
	"maiGoLLMRouter/router"
	"maiGoLLMRouter/server"
)

// configReloader applies config file changes without restarting the HTTP listener.
type configReloader struct {
	path string
	srv  *server.Server
	rt   *router.Router
	logs *logstore.Store

	mu  sync.Mutex
	cfg *config.Config
}

func newConfigReloader(path string, cfg *config.Config, rt *router.Router, srv *server.Server, logs *logstore.Store) *configReloader {
	return &configReloader{path: path, cfg: cfg, rt: rt, srv: srv, logs: logs}
}

func (r *configReloader) tryReload() {
	r.mu.Lock()
	defer r.mu.Unlock()

	prev := r.cfg
	next, err := config.Reload(r.path, prev)
	if err != nil {
		log.Printf("config reload failed: %v", err)
		return
	}

	if next.Server.Listen != prev.Server.Listen {
		log.Printf("config reload: listen %q -> %q ignored (restart required)", prev.Server.Listen, next.Server.Listen)
		next.Server.Listen = prev.Server.Listen
	}
	if next.Server.LogDir != prev.Server.LogDir {
		log.Printf("config reload: log_dir %q -> %q ignored (restart required)", prev.Server.LogDir, next.Server.LogDir)
		next.Server.LogDir = prev.Server.LogDir
	}
	if next.Server.LogRetention != prev.Server.LogRetention {
		log.Printf("config reload: log_retention change ignored (restart required)")
		next.Server.LogRetention = prev.Server.LogRetention
	}

	r.rt.Reload(next)
	r.srv.Reload(next)
	r.cfg = next
	logReload(next)
}

func (r *configReloader) startWatch(interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		var lastMod time.Time
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			st, err := os.Stat(r.path)
			if err != nil {
				continue
			}
			mod := st.ModTime()
			if lastMod.IsZero() {
				lastMod = mod
				continue
			}
			if mod.After(lastMod) {
				lastMod = mod
				r.tryReload()
			}
		}
	}()
}

func startSignalReload(reload func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		for range ch {
			reload()
		}
	}()
}

func logReload(cfg *config.Config) {
	log.Printf("config reloaded: %d provider(s), %d model route(s)", len(cfg.Providers), len(cfg.Models))
	if len(cfg.FallbackProviders) > 0 {
		log.Printf("config reloaded: fallback providers (%s): %s", cfg.FallbackSelection, strings.Join(cfg.FallbackProviders, ", "))
	}
}
