package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"maiGoLLMRouter/provider"
)

// TestReloadDoesNotWaitForInFlightRequest verifies a slow downstream request
// holds up neither a config reload nor the requests that arrive after it.
func TestReloadDoesNotWaitForInFlightRequest(t *testing.T) {
	var once sync.Once
	arrived := make(chan struct{})
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(arrived) })
		<-release
		fmt.Fprint(w, chatStop)
	}))
	defer slow.Close()
	defer close(release) // runs before slow.Close, which waits for the handler
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, chatStop)
	}))
	defer fast.Close()

	content := fmt.Sprintf(`
[[provider]]
name = "slow"
kind = "openai"
base_url = %q
keys = ["s1"]

[[provider]]
name = "fast"
kind = "openai"
base_url = %q
keys = ["f1"]

[model."slow"]
targets = ["slow/m"]

[model."fast"]
targets = ["fast/m"]
`, slow.URL, fast.URL)
	r := New(loadCfg(t, content))
	next := loadCfg(t, content)

	go r.Execute(context.Background(), provider.OpChat, "slow", chatReq())
	<-arrived

	var res *Result
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Reload(next)
		res = r.Execute(context.Background(), provider.OpChat, "fast", chatReq())
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the reload, or the request after it, waited for the in-flight request")
	}
	if !res.Success {
		t.Fatalf("request after reload failed, attempts=%+v", res.Attempts)
	}
}

// TestReloadDiscardsVerdictLearnedUnderOldConfig verifies a request that
// started before a reload cannot teach the new configuration that a provider
// has no /responses route: the verdict describes the old base URL.
func TestReloadDiscardsVerdictLearnedUnderOldConfig(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var once sync.Once
	arrived := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/responses" {
			once.Do(func() { close(arrived) })
			<-release
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"Unknown path /responses","code":"unknown_url"}}`)
			return
		}
		fmt.Fprint(w, chatStop)
	}))
	defer srv.Close()

	content := fmt.Sprintf(`
[[provider]]
name = "openai"
kind = "openai"
base_url = %q
keys = ["n1"]

[model."m"]
targets = ["openai/real-model"]
`, srv.URL)
	r := New(loadCfg(t, content))
	next := loadCfg(t, content)

	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		r.Execute(context.Background(), provider.OpResponses, "m", responsesReq())
	}()
	<-arrived

	reloaded := make(chan struct{})
	go func() {
		defer close(reloaded)
		r.Reload(next)
	}()
	close(release) // the old request now learns "no /responses" and finishes
	<-oldDone
	<-reloaded

	mu.Lock()
	paths = nil
	mu.Unlock()
	if res := r.Execute(context.Background(), provider.OpResponses, "m", responsesReq()); !res.Success {
		t.Fatalf("expected success, attempts=%+v", res.Attempts)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 || paths[0] != "/responses" {
		t.Fatalf("paths after reload = %v, want a fresh /responses probe first", paths)
	}
}
