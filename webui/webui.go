// Package webui serves an embedded HTML log viewer backed by the log store.
package webui

import (
	_ "embed"
	"encoding/json"
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
	// /ui/logs returns only lightweight summaries for the list view. The heavy
	// request/response/attempt bodies are fetched per entry, on expansion, via
	// /ui/logs/entry so periodic refreshes stay small.
	mux.HandleFunc("/ui/logs", func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		entries, err := logs.TailSummaries(limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
	})
	// /ui/logs/entry returns the full record for a single entry by id.
	mux.HandleFunc("/ui/logs/entry", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}
		entry, err := logs.Get(id)
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
