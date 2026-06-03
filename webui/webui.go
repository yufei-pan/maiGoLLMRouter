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
	mux.HandleFunc("/ui/logs", func(w http.ResponseWriter, r *http.Request) {
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		entries, err := logs.Tail(limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
	})
}
