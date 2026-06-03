// Package server exposes the OpenAI-compatible HTTP API and records each
// request to the log store.
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"maiGoLLMRouter/config"
	"maiGoLLMRouter/logstore"
	"maiGoLLMRouter/provider"
	"maiGoLLMRouter/router"
)

// Server wires the router and log store to HTTP handlers.
type Server struct {
	cfg        *config.Config
	rt         *router.Router
	logs       *logstore.Store
	clientKeys map[string]bool
}

// New creates a Server.
func New(cfg *config.Config, rt *router.Router, logs *logstore.Store) *Server {
	keys := make(map[string]bool, len(cfg.Server.ClientKeys))
	for _, k := range cfg.Server.ClientKeys {
		keys[k] = true
	}
	return &Server{cfg: cfg, rt: rt, logs: logs, clientKeys: keys}
}

// Register attaches the API routes to the mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/chat/completions", s.handle(provider.OpChat))
	mux.HandleFunc("/v1/embeddings", s.handle(provider.OpEmbed))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func (s *Server) handle(op provider.Operation) http.HandlerFunc {
	endpoint := "/v1/chat/completions"
	if op == provider.OpEmbed {
		endpoint = "/v1/embeddings"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}
		clientKey, ok := s.authorize(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "invalid or missing bearer token", "authentication_error")
			return
		}

		raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
			return
		}
		model, _ := body["model"].(string)
		if strings.TrimSpace(model) == "" {
			writeError(w, http.StatusBadRequest, "missing required field: model", "invalid_request_error")
			return
		}

		start := time.Now()
		res := s.rt.Execute(r.Context(), op, model, body)
		latency := time.Since(start).Milliseconds()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.Status)
		_, _ = w.Write(res.Body)

		s.log(endpoint, clientKey, model, latency, raw, res)
	}
}

func (s *Server) authorize(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}
	token := strings.TrimSpace(auth[len(prefix):])
	if token == "" {
		return "", false
	}
	if !s.clientKeys[token] {
		return token, false
	}
	return token, true
}

func (s *Server) log(endpoint, clientKey, model string, latency int64, reqBody []byte, res *router.Result) {
	if s.logs == nil {
		return
	}
	entry := logstore.Entry{
		Time:         time.Now().UTC().Format(time.RFC3339Nano),
		ID:           "req-" + randomID(),
		ClientKey:    maskKey(clientKey),
		Endpoint:     endpoint,
		InboundModel: model,
		Targets:      res.Targets,
		Provider:     res.Provider,
		Model:        res.Model,
		Success:      res.Success,
		Status:       res.Status,
		LatencyMS:    latency,
		Attempts:     res.Attempts,
		Request:      json.RawMessage(reqBody),
		Response:     json.RawMessage(res.Body),
	}
	_ = s.logs.Write(entry)
}

func writeError(w http.ResponseWriter, status int, msg, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
}
