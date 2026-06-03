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
