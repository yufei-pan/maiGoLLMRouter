package logstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testBase = time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

// writeEntries writes n entries with strictly increasing timestamps starting at
// the given base, returning them in write (chronological) order.
func writeEntriesAt(t *testing.T, s *Store, n int, base time.Time) []Entry {
	t.Helper()
	entries := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		e := Entry{
			Time:         base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
			ID:           fmt.Sprintf("req-%04d", i),
			ClientKey:    "****mask",
			Endpoint:     "/v1/chat/completions",
			InboundModel: "gpt-4o",
			Provider:     "openai",
			Model:        "gpt-4o",
			Success:      true,
			Status:       200,
			LatencyMS:    int64(100 + i),
			Attempts:     []map[string]any{{"provider": "openai"}, {"provider": "google"}},
			Request:      json.RawMessage(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
			Response:     json.RawMessage(`{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`),
		}
		if err := s.Write(e); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		entries = append(entries, e)
	}
	return entries
}

func TestWriteCreatesJSONFileAndIndex(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	writeEntriesAt(t, s, 3, testBase)

	// Per-entry detail file exists at logs/YYYY-MM/DD/<id>.json.zst and is
	// actually zstd-compressed (magic bytes 0x28 0xB5 0x2F 0xFD).
	jsonPath := filepath.Join(dir, "2026-06", "04", "req-0000.json.zst")
	raw0, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("expected detail file %s: %v", jsonPath, err)
	}
	if len(raw0) < 4 || raw0[0] != 0x28 || raw0[1] != 0xB5 || raw0[2] != 0x2F || raw0[3] != 0xFD {
		t.Fatalf("detail file is not zstd-compressed: % x", raw0[:min(4, len(raw0))])
	}

	// Index file exists with a header comment and one data line per entry.
	raw, err := os.ReadFile(filepath.Join(dir, indexFileName))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 4 { // header + 3 entries
		t.Fatalf("want 4 index lines, got %d: %q", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "#") {
		t.Errorf("first line should be the comment header, got %q", lines[0])
	}
	cols := strings.Split(lines[1], "\t")
	if len(cols) != 12 {
		t.Fatalf("want 12 TSV columns, got %d: %q", len(cols), cols)
	}
	if cols[0] != "2026-06/04/req-0000" {
		t.Errorf("col0 path = %q", cols[0])
	}
	if cols[7] != "11" || cols[8] != "7" {
		t.Errorf("want in/out tokens 11/7, got %q/%q", cols[7], cols[8])
	}
	if cols[11] != "hi" {
		t.Errorf("request preview = %q, want hi", cols[11])
	}
}

func TestRecentNewestFirst(t *testing.T) {
	s, _ := New(t.TempDir())
	writeEntriesAt(t, s, 3, testBase)
	got := s.Recent()
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	if got[0].Path != "2026-06/04/req-0002" || got[2].Path != "2026-06/04/req-0000" {
		t.Fatalf("not newest-first: %s ... %s", got[0].Path, got[2].Path)
	}
	if got[0].InTokens == nil || *got[0].InTokens != 11 {
		t.Errorf("want in_tokens 11, got %v", got[0].InTokens)
	}
	if got[0].RequestPreview != "hi" {
		t.Errorf("want request_preview hi, got %q", got[0].RequestPreview)
	}
}

func TestSinceReturnsOnlyNewer(t *testing.T) {
	s, _ := New(t.TempDir())
	es := writeEntriesAt(t, s, 5, testBase)
	// Everything strictly newer than entry index 2.
	got := s.Since(es[2].Time)
	if len(got) != 2 {
		t.Fatalf("want 2 newer entries, got %d", len(got))
	}
	if got[0].Path != "2026-06/04/req-0004" || got[1].Path != "2026-06/04/req-0003" {
		t.Fatalf("unexpected since order: %s, %s", got[0].Path, got[1].Path)
	}
	// Newest cursor yields nothing.
	if got := s.Since(es[4].Time); len(got) != 0 {
		t.Fatalf("want 0 for newest cursor, got %d", len(got))
	}
}

func TestBeforeStreamsOlderPage(t *testing.T) {
	s, _ := New(t.TempDir())
	es := writeEntriesAt(t, s, 10, testBase)

	got, err := s.Before(es[9].Time, 3)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	// Closest-older first: req-0008, req-0007, req-0006.
	want := []string{"2026-06/04/req-0008", "2026-06/04/req-0007", "2026-06/04/req-0006"}
	for i, w := range want {
		if got[i].Path != w {
			t.Errorf("before[%d] = %s, want %s", i, got[i].Path, w)
		}
	}

	// Oldest cursor: nothing older.
	if got, _ := s.Before(es[0].Time, 5); len(got) != 0 {
		t.Fatalf("want 0 older than oldest, got %d", len(got))
	}
}

func TestReadEntryFullRecord(t *testing.T) {
	s, _ := New(t.TempDir())
	writeEntriesAt(t, s, 1, testBase)
	raw, err := s.ReadEntry("2026-06/04/req-0000")
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	if raw == nil {
		t.Fatal("want entry, got nil")
	}
	var got Entry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "req-0000" || len(got.Request) == 0 || len(got.Response) == 0 {
		t.Fatalf("full record missing fields: %+v", got)
	}

	// Trailing .json is accepted too.
	if raw2, err := s.ReadEntry("2026-06/04/req-0000.json"); err != nil || raw2 == nil {
		t.Fatalf("want entry with .json suffix, got %v %v", raw2, err)
	}

	// Missing entry returns (nil, nil).
	if raw, err := s.ReadEntry("2026-06/04/req-9999"); err != nil || raw != nil {
		t.Fatalf("want nil for missing, got %v %v", raw, err)
	}
}

func TestReadEntryLegacyUncompressed(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	// Simulate a pre-compression detail file written as plain .json.
	legacyDir := filepath.Join(dir, "2026-06", "04")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"id":"req-legacy","success":true}`
	if err := os.WriteFile(filepath.Join(legacyDir, "req-legacy.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := s.ReadEntry("2026-06/04/req-legacy")
	if err != nil || raw == nil {
		t.Fatalf("want legacy entry, got %v %v", raw, err)
	}
	if string(raw) != body {
		t.Fatalf("legacy body mismatch: %s", raw)
	}
}

func TestCleanupRemovesOld(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	writeEntriesAt(t, s, 2, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) // old
	writeEntriesAt(t, s, 2, time.Now().UTC())                            // recent

	if err := s.Cleanup(60 * 24 * time.Hour); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// The old month directory and its files are gone.
	if _, err := os.Stat(filepath.Join(dir, "2026-01")); !os.IsNotExist(err) {
		t.Errorf("old month dir should have been removed, stat err = %v", err)
	}
	// The index no longer references old entries.
	all, err := s.readAll()
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 index rows after cleanup, got %d", len(all))
	}
	for _, e := range all {
		if strings.HasPrefix(e.Path, "2026-01") {
			t.Errorf("old index row survived cleanup: %s", e.Path)
		}
	}
	// The cache reflects the pruned index.
	if got := len(s.Recent()); got != 2 {
		t.Errorf("want cache of 2 after cleanup, got %d", got)
	}
}

func TestCleanupDisabledIsNoop(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	writeEntriesAt(t, s, 3, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err := s.Cleanup(0); err != nil {
		t.Fatalf("cleanup(0): %v", err)
	}
	if got := len(s.Recent()); got != 3 {
		t.Errorf("disabled cleanup must not prune; got %d", got)
	}
}

func TestReadEntryWithDotSlashDir(t *testing.T) {
	// A "./"-prefixed (relative, uncleaned) dir must still resolve valid paths;
	// filepath.Join normalizes away the "./", so safeEntryPath has to compare
	// against the cleaned dir. Run inside a temp cwd so "./logs" is contained.
	t.Chdir(t.TempDir())
	s, err := New("./logs")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	writeEntriesAt(t, s, 1, testBase)
	raw, err := s.ReadEntry("2026-06/04/req-0000")
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	if raw == nil {
		t.Fatal("want entry, got nil")
	}
}

func TestReadEntryRejectsTraversal(t *testing.T) {
	s, _ := New(t.TempDir())
	for _, bad := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"2026-06/../../secret",
		"..",
		"",
		"a\x00b",
		"a\\b",
	} {
		if _, err := s.ReadEntry(bad); err == nil {
			t.Errorf("expected error for %q, got nil", bad)
		}
	}
}

func TestCacheHardCap(t *testing.T) {
	s, _ := New(t.TempDir())
	// Recent timestamps so the window rule does not evict; only the hard cap
	// should apply.
	writeEntriesAt(t, s, cacheMax+50, time.Now().UTC())
	if got := len(s.Recent()); got != cacheMax {
		t.Fatalf("want cache capped at %d, got %d", cacheMax, got)
	}
}

func TestCacheWindowKeepsMinimum(t *testing.T) {
	s, _ := New(t.TempDir())
	// All timestamps are well older than the cache window, so the window rule
	// would drop them all, but the minimum keeps cacheMin.
	old := time.Now().UTC().Add(-2 * time.Hour)
	writeEntriesAt(t, s, cacheMin+80, old)
	if got := len(s.Recent()); got != cacheMin {
		t.Fatalf("want cache trimmed to min %d, got %d", cacheMin, got)
	}
}

func TestWarmCacheFromDisk(t *testing.T) {
	dir := t.TempDir()
	s1, _ := New(dir)
	writeEntriesAt(t, s1, 5, testBase)

	s2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := len(s2.Recent()); got != 5 {
		t.Fatalf("want 5 entries warmed from disk, got %d", got)
	}
	if s2.Recent()[0].Path != "2026-06/04/req-0004" {
		t.Fatalf("warm cache not newest-first: %s", s2.Recent()[0].Path)
	}
}

// --- robustness of the TSV reader/parser ---

func collect(t *testing.T, content string) []IndexEntry {
	t.Helper()
	var out []IndexEntry
	if err := forEachIndexLine(bytes.NewReader([]byte(content)), func(e IndexEntry) bool {
		out = append(out, e)
		return true
	}); err != nil {
		t.Fatalf("forEachIndexLine: %v", err)
	}
	return out
}

func TestTSVReaderTolerance(t *testing.T) {
	ts := "2026-06-04T12:00:0"
	var b strings.Builder
	b.WriteString(indexHeader) // header, must be skipped
	// A: full row.
	b.WriteString("p/A\t" + ts + "0Z\t200\t/v1\tgpt\topenai/gpt\t2\t11\t7\t100\n")
	// A block of NULs from a crash-allocated-but-unwritten region.
	b.WriteString("\x00\x00\x00\x00\x00\x00\x00\x00\n")
	// B: future, wider row with extra trailing fields (ignored).
	b.WriteString("p/B\t" + ts + "1Z\t200\t/v1\tgpt\topenai/gpt\t1\t5\t3\t90\textra1\textra2\n")
	// An empty line (skipped).
	b.WriteString("\n")
	// C: older, shorter row missing trailing fields (defaults applied).
	b.WriteString("p/C\t" + ts + "2Z\t500\n")
	// A line missing a parseable time (skipped).
	b.WriteString("p/bad\tnot-a-time\t200\n")
	// D: truncated final line with no trailing newline.
	b.WriteString("p/D\t" + ts + "3Z\t204")

	got := collect(t, b.String())
	if len(got) != 4 {
		t.Fatalf("want 4 valid entries (A,B,C,D), got %d: %+v", len(got), got)
	}
	paths := []string{got[0].Path, got[1].Path, got[2].Path, got[3].Path}
	if want := []string{"p/A", "p/B", "p/C", "p/D"}; !equalStrings(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	// B kept its recognized fields despite trailing extras.
	if got[1].LatencyMS != 90 || got[1].InTokens == nil || *got[1].InTokens != 5 {
		t.Errorf("B extras not handled: %+v", got[1])
	}
	// C defaulted the missing fields.
	if got[2].Status != 500 || got[2].Attempts != 0 || got[2].InTokens != nil || got[2].LatencyMS != 0 {
		t.Errorf("C defaults wrong: %+v", got[2])
	}
	// D parsed from a truncated final line.
	if got[3].Status != 204 {
		t.Errorf("D status = %d, want 204", got[3].Status)
	}
}

func TestTSVReaderOverlongLine(t *testing.T) {
	ts := "2026-06-04T12:00:00Z"
	// A valid row, then a tab and an enormous field with no newline until the
	// end. The reader must cap the line and still parse the leading fields.
	huge := strings.Repeat("x", maxTSVLine+1024)
	content := "p/A\t" + ts + "\t200\t/v1\tgpt\topenai/gpt\t1\t10\t5\t100\t" + huge + "\n" +
		"p/B\t2026-06-04T12:00:01Z\t201\n"

	got := collect(t, content)
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if got[0].Path != "p/A" || got[0].Status != 200 || got[0].LatencyMS != 100 {
		t.Errorf("overlong row's leading fields lost: %+v", got[0])
	}
	if got[1].Path != "p/B" || got[1].Status != 201 {
		t.Errorf("row after overlong line not recovered: %+v", got[1])
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
