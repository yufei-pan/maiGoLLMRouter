package server

import (
	"net/http"
	"strings"
)

// WithCORS lets web apps in a browser call the API: /v1/... and /healthz, never
// the web UI, which has no key. It answers a browser's preflight itself, without
// asking for the key the real request will carry, and marks the API's answers as
// readable by the calling page. Off with cors = false; cors_origins limits it to
// those origins, otherwise any origin may call (the client key is the guard).
func (s *Server) WithCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		api := strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/healthz"
		s.mu.RLock()
		on, origins := s.cfg.Server.CORS, s.cfg.Server.CORSOrigins
		s.mu.RUnlock()
		if origin == "" || !api || !on {
			next.ServeHTTP(w, r)
			return
		}
		allow := "*"
		if len(origins) > 0 {
			allow = ""
			for _, o := range origins {
				if o == origin {
					allow = origin
					break
				}
			}
			w.Header().Add("Vary", "Origin")
			if allow == "" {
				next.ServeHTTP(w, r)
				return
			}
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", allow)
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if asked := r.Header.Get("Access-Control-Request-Headers"); asked != "" {
				h.Set("Access-Control-Allow-Headers", asked)
			} else {
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			}
			if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
				h.Set("Access-Control-Allow-Private-Network", "true")
			}
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
