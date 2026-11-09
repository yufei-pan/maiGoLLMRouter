package webui_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"maiGoLLMRouter/inflight"
	"maiGoLLMRouter/logstore"
	"maiGoLLMRouter/webui"
)

// A huge limit on /ui/logs used to crash the process with an out-of-memory
// error. It is served as the store's cap of 500 instead.
func TestLogsLimitCapped(t *testing.T) {
	logs, err := logstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	base := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	for i := range 600 {
		e := logstore.Entry{
			Time:         base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
			ID:           fmt.Sprintf("req-%04d", i),
			Endpoint:     "/v1/chat/completions",
			InboundModel: "gpt-4o",
			Success:      true,
			Status:       200,
		}
		if err := logs.Write(e); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	mux := http.NewServeMux()
	webui.Register(mux, logs, inflight.New())

	before := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	req := httptest.NewRequest(http.MethodGet, "/ui/logs?before="+url.QueryEscape(before)+"&limit=100000000000", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body)
	}
	var body struct {
		Entries []logstore.IndexEntry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Entries) != 500 {
		t.Fatalf("want 500 entries, got %d", len(body.Entries))
	}
}
