// Package webui serves an embedded HTML log viewer backed by the log store.
package webui

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"maiGoLLMRouter/logstore"
)

//go:embed index.html
var indexHTML []byte

// Register attaches the web UI routes to the mux.
func Register(mux *http.ServeMux, logs *logstore.Store) {
	mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("/ui/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	// /ui/logs returns lightweight index rows for the list view. The heavy
	// request/response/attempt bodies are fetched per entry, on expansion, via
	// /ui/logs/entry so periodic refreshes stay small. The response always
	// carries the server time so the client can cursor by server-issued
	// timestamps instead of its own (possibly skewed) clock.
	//
	// Modes (newest first in every case):
	//   - no params:        the cached recent entries (initial page load)
	//   - ?since=<ts>:      only entries strictly newer than ts (auto-refresh)
	//   - ?before=<ts>&limit=N: up to N entries older than ts (infinite scroll)
	mux.HandleFunc("/ui/logs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var (
			entries []logstore.IndexEntry
			err     error
		)
		switch {
		case q.Get("before") != "":
			limit := 50
			if v := q.Get("limit"); v != "" {
				if n, e := strconv.Atoi(v); e == nil && n > 0 {
					limit = n
				}
			}
			entries, err = logs.Before(q.Get("before"), limit)
		case q.Get("since") != "":
			entries = logs.Since(q.Get("since"))
		default:
			entries = logs.Recent()
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if entries == nil {
			entries = []logstore.IndexEntry{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries":     entries,
			"server_time": logs.ServerTime(),
		})
	})
	// /ui/logs/entry returns the full record for a single entry, located by its
	// index path (YYYY-MM/DD/<id>) and read directly from disk.
	mux.HandleFunc("/ui/logs/entry", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			http.Error(w, "missing path", http.StatusBadRequest)
			return
		}
		entry, err := logs.ReadEntry(path)
		if errors.Is(err, logstore.ErrInvalidPath) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if entry == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(entry)
	})
}
