// Package inflight tracks active API requests in memory for the web UI.
package inflight

import (
	"sort"
	"sync"
	"time"

	"maiGoLLMRouter/router"
)

// Meta is the initial metadata for a newly registered in-flight request.
type Meta struct {
	ID             string
	StartedAt      string
	Endpoint       string
	InboundModel   string
	ClientKey      string
	RequestPreview string
	InTokens       int // rough estimate for live UI preview
}

// Entry describes one request currently being routed.
type Entry struct {
	ID             string `json:"id"`
	StartedAt      string `json:"started_at"`
	Endpoint       string `json:"endpoint"`
	InboundModel   string `json:"inbound_model"`
	ClientKey      string `json:"client_key,omitempty"`
	RequestPreview string `json:"request_preview,omitempty"`
	InTokens       int    `json:"in_tokens,omitempty"`
	Attempts       int    `json:"attempts"`
	CurrentTarget  string `json:"current_target,omitempty"`
	LastOutcome    string `json:"last_outcome,omitempty"`

	mu sync.Mutex
}

// OnAttempt records a downstream attempt for live progress in the UI.
func (e *Entry) OnAttempt(a router.Attempt) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Attempts++
	if a.Provider != "" {
		e.CurrentTarget = a.Provider + "/" + a.Model
	} else if a.Model != "" {
		e.CurrentTarget = a.Model
	}
	e.LastOutcome = a.Outcome
}

func (e *Entry) snapshot() Entry {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Entry{
		ID:             e.ID,
		StartedAt:      e.StartedAt,
		Endpoint:       e.Endpoint,
		InboundModel:   e.InboundModel,
		ClientKey:      e.ClientKey,
		RequestPreview: e.RequestPreview,
		InTokens:       e.InTokens,
		Attempts:       e.Attempts,
		CurrentTarget:  e.CurrentTarget,
		LastOutcome:    e.LastOutcome,
	}
}

// Registry holds in-flight requests keyed by ID.
type Registry struct {
	mu   sync.RWMutex
	byID map[string]*Entry
}

// New creates an empty in-flight registry.
func New() *Registry {
	return &Registry{byID: make(map[string]*Entry)}
}

// Register adds an entry and returns the live object for attempt updates.
func (r *Registry) Register(meta Meta) *Entry {
	live := &Entry{
		ID:             meta.ID,
		StartedAt:      meta.StartedAt,
		Endpoint:       meta.Endpoint,
		InboundModel:   meta.InboundModel,
		ClientKey:      meta.ClientKey,
		RequestPreview: meta.RequestPreview,
		InTokens:       meta.InTokens,
	}
	if live.StartedAt == "" {
		live.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	r.mu.Lock()
	r.byID[meta.ID] = live
	r.mu.Unlock()
	return live
}

// Unregister removes a request when it completes.
func (r *Registry) Unregister(id string) {
	r.mu.Lock()
	delete(r.byID, id)
	r.mu.Unlock()
}

// Snapshot returns a copy of active requests, newest first.
func (r *Registry) Snapshot() []Entry {
	r.mu.RLock()
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}
	r.mu.RUnlock()

	out := make([]Entry, 0, len(ids))
	for _, id := range ids {
		r.mu.RLock()
		e := r.byID[id]
		r.mu.RUnlock()
		if e == nil {
			continue
		}
		out = append(out, e.snapshot())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt > out[j].StartedAt
	})
	return out
}

// ServerTime returns the current server time for UI cursors.
func (r *Registry) ServerTime() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
