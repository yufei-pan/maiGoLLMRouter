// Package logstore persists request/response records as append-only JSONL and
// supports tailing recent entries for the web UI.
package logstore

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry is a single logged request/response record. Sensitive downstream API
// keys are never stored here; only masked keys (set by the caller) appear in
// Attempts.
type Entry struct {
	Time         string          `json:"time"`
	ID           string          `json:"id"`
	ClientKey    string          `json:"client_key"` // masked inbound bearer
	Endpoint     string          `json:"endpoint"`
	InboundModel string          `json:"inbound_model"`
	Targets      []string        `json:"targets"`
	Provider     string          `json:"provider,omitempty"`
	Model        string          `json:"model,omitempty"`
	Success      bool            `json:"success"`
	Status       int             `json:"status"`
	LatencyMS    int64           `json:"latency_ms"`
	Attempts     any             `json:"attempts"`
	Request      json.RawMessage `json:"request,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
}

// Store is a thread-safe JSONL log writer/reader rooted at a directory.
type Store struct {
	dir string
	mu  sync.Mutex
}

// New creates the log directory if needed and returns a Store.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) fileFor(t time.Time) string {
	return filepath.Join(s.dir, t.Format("2006-01-02")+".jsonl")
}

// Write appends one entry to today's log file.
func (s *Store) Write(e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.fileFor(time.Now()), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return nil
}

// Summary is a lightweight projection of an Entry for the log list view. It
// deliberately omits the heavy Request, Response, and per-attempt bodies so the
// list can be refreshed cheaply; the full record is fetched per entry via Get.
type Summary struct {
	Time          string `json:"time"`
	ID            string `json:"id"`
	ClientKey     string `json:"client_key"`
	Endpoint      string `json:"endpoint"`
	InboundModel  string `json:"inbound_model"`
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	Success       bool   `json:"success"`
	Status        int    `json:"status"`
	LatencyMS     int64  `json:"latency_ms"`
	AttemptsCount    int `json:"attempts_count"`
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
}

// summaryView mirrors the subset of Entry needed to build a Summary. Attempts
// is decoded as a raw array only to count its elements, avoiding decoding the
// (potentially large) per-attempt payloads.
type summaryView struct {
	Time         string            `json:"time"`
	ID           string            `json:"id"`
	ClientKey    string            `json:"client_key"`
	Endpoint     string            `json:"endpoint"`
	InboundModel string            `json:"inbound_model"`
	Provider     string            `json:"provider"`
	Model        string            `json:"model"`
	Success      bool              `json:"success"`
	Status       int               `json:"status"`
	LatencyMS    int64             `json:"latency_ms"`
	Attempts     []json.RawMessage `json:"attempts"`
	Response     json.RawMessage   `json:"response"`
}

// Tail returns up to limit of the most recent entries, newest first.
func (s *Store) Tail(limit int) ([]json.RawMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	matches, err := filepath.Glob(filepath.Join(s.dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches))) // newest dates first

	out := make([]json.RawMessage, 0, limit)
	for _, path := range matches {
		lines, err := readLines(path)
		if err != nil {
			continue
		}
		// Iterate this file newest-first.
		for i := len(lines) - 1; i >= 0; i-- {
			if len(lines[i]) == 0 {
				continue
			}
			out = append(out, json.RawMessage(lines[i]))
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// TailSummaries returns up to limit of the most recent entries as lightweight
// summaries (newest first), without the request/response/attempt bodies.
func (s *Store) TailSummaries(limit int) ([]Summary, error) {
	raw, err := s.Tail(limit)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(raw))
	for _, line := range raw {
		var v summaryView
		if err := json.Unmarshal(line, &v); err != nil {
			continue // skip malformed lines rather than fail the whole list
		}
		sm := Summary{
			Time:          v.Time,
			ID:            v.ID,
			ClientKey:     v.ClientKey,
			Endpoint:      v.Endpoint,
			InboundModel:  v.InboundModel,
			Provider:      v.Provider,
			Model:         v.Model,
			Success:       v.Success,
			Status:        v.Status,
			LatencyMS:     v.LatencyMS,
			AttemptsCount: len(v.Attempts),
		}
		if p, c, ok := computeUsage(v.Success, v.Response, v.Attempts); ok {
			sm.PromptTokens = p
			sm.CompletionTokens = c
		}
		out = append(out, sm)
	}
	return out, nil
}

// Get returns the full raw record for the entry with the given ID, scanning
// newest files first. It returns (nil, nil) when no entry matches.
func (s *Store) Get(id string) (json.RawMessage, error) {
	if id == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	matches, err := filepath.Glob(filepath.Join(s.dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches))) // newest dates first

	for _, path := range matches {
		lines, err := readLines(path)
		if err != nil {
			continue
		}
		for i := len(lines) - 1; i >= 0; i-- {
			if len(lines[i]) == 0 {
				continue
			}
			var idOnly struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(lines[i], &idOnly); err != nil {
				continue
			}
			if idOnly.ID == id {
				return json.RawMessage(lines[i]), nil
			}
		}
	}
	return nil, nil
}

func readLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		b := sc.Bytes()
		cp := make([]byte, len(b))
		copy(cp, b)
		lines = append(lines, cp)
	}
	return lines, sc.Err()
}
