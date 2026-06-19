// Package logstore persists request/response records as one JSON file per
// inbound request under logs/YYYY-MM/DD/<id>.json, indexed by a compact,
// append-only logs/index.tsv. A small in-memory cache of the most recent
// index entries backs the web UI list view; older pages and full records are
// read lazily from disk.
package logstore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	indexFileName = "index.tsv"
	// entryExt is the on-disk suffix for a per-request detail file: a
	// zstd-compressed JSON document.
	entryExt       = ".json.zst"
	legacyEntryExt = ".json" // uncompressed detail files written before compression
	// indexHeader documents the TSV columns. It begins with '#' so the parser
	// skips it as a comment. Adding columns in the future stays compatible:
	// readers take the leading fields they recognize and ignore the rest.
	indexHeader = "# File Path\tTime\tStatus\tEndpoint\tInbound model\tServed by\tAttempts\tIn tokens\tOut tokens\tLatency\tFinish reason\tRequest preview\n"

	// cacheWindow, cacheMin and cacheMax bound the in-memory index cache: keep
	// roughly the last cacheWindow of entries, but never fewer than cacheMin
	// nor more than cacheMax.
	cacheWindow = 5 * time.Minute
	cacheMin    = 100
	cacheMax    = 200

	// maxTSVLine caps how many bytes of a single TSV line are retained. A line
	// longer than this (e.g. corruption) is truncated to its first fields and
	// the remainder is discarded up to the next newline, so reads never OOM or
	// crash on a pathologically long line.
	maxTSVLine = 1 << 20 // 1 MiB
)

// Entry is the full per-request record persisted as logs/YYYY-MM/DD/<id>.json.
// Sensitive downstream API keys are never stored here; only masked keys (set by
// the caller) appear in Attempts.
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

// IndexEntry is one row of index.tsv: a lightweight projection used for the log
// list view. InTokens/OutTokens are pointers so an unknown count (empty TSV
// field) is distinguishable from a known zero.
type IndexEntry struct {
	Path         string `json:"path"` // YYYY-MM/DD/<id>, relative to the log dir, no .json
	Time         string `json:"time"`
	Status       int    `json:"status"`
	Endpoint     string `json:"endpoint"`
	InboundModel string `json:"inbound_model"`
	ServedBy     string `json:"served_by"` // provider/model
	Attempts     int    `json:"attempts"`
	InTokens     *int   `json:"in_tokens"`
	OutTokens    *int   `json:"out_tokens"`
	LatencyMS    int64  `json:"latency_ms"`
	FinishReason   string `json:"finish_reason"`   // finish reason of the served/last attempt
	RequestPreview string `json:"request_preview"` // first N chars of inbound request content
}

// Store is a thread-safe log writer/reader rooted at a directory.
type Store struct {
	dir     string
	tsvPath string
	mu      sync.RWMutex
	cache   []IndexEntry // chronological (oldest first)
}

// New creates the log directory if needed, ensures the index file has a header,
// and warms the in-memory cache from the tail of index.tsv.
func New(dir string) (*Store, error) {
	// Clean the directory so path-prefix checks in safeEntryPath compare apples
	// to apples (e.g. "./logs" becomes "logs", matching filepath.Join output).
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, tsvPath: filepath.Join(dir, indexFileName)}
	if err := s.ensureHeader(); err != nil {
		return nil, err
	}
	if err := s.warmCache(); err != nil {
		return nil, err
	}
	return s, nil
}

// ensureHeader writes the comment header when the index file is missing or
// empty so the format is self-describing on disk.
func (s *Store) ensureHeader() error {
	info, err := os.Stat(s.tsvPath)
	if err == nil && info.Size() > 0 {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(s.tsvPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(indexHeader)
	return err
}

// warmCache loads up to cacheMax of the most recent index entries into memory.
// It streams the index through a fixed ring so the whole (potentially large)
// file is never held in memory at once.
func (s *Store) warmCache() error {
	f, err := os.Open(s.tsvPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	ring := newLastN(cacheMax)
	if err := forEachIndexLine(f, func(e IndexEntry) bool {
		ring.add(e)
		return true
	}); err != nil {
		return err
	}
	s.mu.Lock()
	s.cache = ring.slice()
	s.mu.Unlock()
	return nil
}

// ServerTime returns the current server time as an RFC3339Nano UTC string. The
// web UI cursors by these server-issued timestamps rather than the client
// clock to avoid client-side time anomalies.
func (s *Store) ServerTime() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// Write persists one entry: it writes the per-request JSON file first (so the
// index never references a missing file), appends one index line, then updates
// the in-memory cache.
func (s *Store) Write(e Entry) error {
	t := parseEntryTime(e.Time)
	month := t.Format("2006-01")
	day := t.Format("02")
	relPath := month + "/" + day + "/" + e.ID

	jsonBody, err := json.Marshal(e)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	jsonDir := filepath.Join(s.dir, month, day)
	if err := os.MkdirAll(jsonDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(jsonDir, e.ID+entryExt), zstdCompress(jsonBody), 0o644); err != nil {
		return err
	}

	idx := s.indexEntryFor(e, relPath)
	if err := s.appendIndexLine(idx); err != nil {
		return err
	}

	s.cache = append(s.cache, idx)
	s.evict()
	return nil
}

// indexEntryFor builds the index projection for an entry, computing token
// counts once here so the list view never has to parse bodies.
func (s *Store) indexEntryFor(e Entry, relPath string) IndexEntry {
	served := ""
	if e.Provider != "" {
		served = e.Provider + "/" + e.Model
	}
	idx := IndexEntry{
		Path:         relPath,
		Time:         e.Time,
		Status:       e.Status,
		Endpoint:     e.Endpoint,
		InboundModel: e.InboundModel,
		ServedBy:     served,
		Attempts:     attemptsCount(e.Attempts),
		LatencyMS:    e.LatencyMS,
		FinishReason: finishReasonForEntry(e),
	}
	if p, c, ok := usageForEntry(e); ok {
		idx.InTokens = &p
		idx.OutTokens = &c
	}
	idx.RequestPreview = RequestContentPreview(e.Request, DefaultRequestPreviewLen)
	return idx
}

// appendIndexLine appends a single TSV row. Caller holds the write lock.
func (s *Store) appendIndexLine(e IndexEntry) error {
	f, err := os.OpenFile(s.tsvPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(formatIndexLine(e))
	return err
}

// evict trims the cache to the configured bounds. Caller holds the write lock.
func (s *Store) evict() {
	// Hard cap.
	if len(s.cache) > cacheMax {
		s.cache = s.cache[len(s.cache)-cacheMax:]
	}
	// Drop entries older than the window, but keep at least cacheMin.
	cutoff := time.Now().UTC().Add(-cacheWindow)
	for len(s.cache) > cacheMin {
		t := parseEntryTime(s.cache[0].Time)
		if t.After(cutoff) {
			break
		}
		s.cache = s.cache[1:]
	}
}

// Recent returns the cached entries, newest first (used on initial page load).
func (s *Store) Recent() []IndexEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return reversed(s.cache)
}

// Since returns cached entries strictly newer than ts, newest first. Auto
// refresh always asks for entries newer than the newest one the client holds,
// which is well within the cache window.
func (s *Store) Since(ts string) []IndexEntry {
	cursor := parseEntryTime(ts)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]IndexEntry, 0)
	for i := len(s.cache) - 1; i >= 0; i-- {
		t := parseEntryTime(s.cache[i].Time)
		if t.After(cursor) {
			out = append(out, s.cache[i])
		} else {
			break // cache is chronological; nothing older qualifies
		}
	}
	return out
}

// Before returns up to limit entries strictly older than ts, newest first, by
// streaming index.tsv. Memory use is bounded by limit regardless of file size.
func (s *Store) Before(ts string, limit int) ([]IndexEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	cursor := parseEntryTime(ts)

	s.mu.RLock()
	defer s.mu.RUnlock()

	f, err := os.Open(s.tsvPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	// Keep a sliding window of the last `limit` matching lines. The file is
	// chronological, so once we reach ts we can stop: every later line is >= ts.
	ring := make([]IndexEntry, 0, limit)
	err = forEachIndexLine(f, func(e IndexEntry) bool {
		t := parseEntryTime(e.Time)
		if !t.Before(cursor) {
			return false // reached the cursor; stop scanning
		}
		if len(ring) == limit {
			ring = ring[1:]
		}
		ring = append(ring, e)
		return true
	})
	if err != nil {
		return nil, err
	}
	return reversed(ring), nil
}

// ErrInvalidPath is returned by ReadEntry when the requested path is malformed
// or would escape the log directory.
var ErrInvalidPath = errors.New("invalid path")

// ReadEntry returns the raw JSON of the full record at the given index path
// (YYYY-MM/DD/<id>, without any extension). It reads the zstd-compressed file,
// falling back to a legacy uncompressed .json file if present. It returns
// (nil, nil) when no file exists and rejects paths that escape the log dir.
func (s *Store) ReadEntry(relPath string) (json.RawMessage, error) {
	base, ok := s.safeEntryPath(relPath)
	if !ok {
		return nil, ErrInvalidPath
	}
	if b, err := os.ReadFile(base + entryExt); err == nil {
		dec, derr := zstdDecompress(b)
		if derr != nil {
			return nil, derr
		}
		return json.RawMessage(dec), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if b, err := os.ReadFile(base + legacyEntryExt); err == nil {
		return json.RawMessage(b), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return nil, nil
}

// safeEntryPath resolves relPath to an absolute, extension-less file path under
// the log dir, rejecting traversal attempts. Callers append the desired suffix.
func (s *Store) safeEntryPath(relPath string) (string, bool) {
	relPath = strings.TrimSpace(relPath)
	if relPath == "" || strings.ContainsRune(relPath, '\x00') ||
		strings.Contains(relPath, "\\") || strings.HasPrefix(relPath, "/") {
		return "", false
	}
	relPath = strings.TrimSuffix(relPath, entryExt)
	relPath = strings.TrimSuffix(relPath, legacyEntryExt)
	// Reject any "", ".", or ".." segment so the path cannot escape the dir.
	for _, seg := range strings.Split(relPath, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	full := filepath.Join(s.dir, filepath.FromSlash(relPath))
	if full != filepath.Clean(s.dir) && !strings.HasPrefix(full, s.dir+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

// Cleanup removes detail files and index rows older than the retention window.
// Day directories whose calendar day falls entirely before the cutoff day are
// deleted, the index is rewritten to drop their rows, and the cache is
// refreshed. A non-positive retention disables cleanup (no-op).
func (s *Store) Cleanup(retention time.Duration) error {
	if retention <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().Add(-retention)
	cutoffDay := time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.removeOldDayDirs(cutoffDay); err != nil {
		return err
	}
	return s.pruneIndex(cutoffDay)
}

// removeOldDayDirs deletes logs/YYYY-MM/DD directories whose day is before
// cutoffDay, then removes any month directory left empty.
func (s *Store) removeOldDayDirs(cutoffDay time.Time) error {
	months, err := filepath.Glob(filepath.Join(s.dir, "[0-9][0-9][0-9][0-9]-[0-9][0-9]"))
	if err != nil {
		return err
	}
	for _, monthDir := range months {
		days, err := filepath.Glob(filepath.Join(monthDir, "[0-9][0-9]"))
		if err != nil {
			return err
		}
		for _, dayDir := range days {
			day, ok := dayFromDirs(filepath.Base(monthDir), filepath.Base(dayDir))
			if !ok {
				continue
			}
			if day.Before(cutoffDay) {
				if err := os.RemoveAll(dayDir); err != nil {
					return err
				}
			}
		}
		if entries, err := os.ReadDir(monthDir); err == nil && len(entries) == 0 {
			_ = os.Remove(monthDir)
		}
	}
	return nil
}

// pruneIndex rewrites index.tsv keeping only rows on or after cutoffDay, then
// refreshes the in-memory cache. Kept rows are written straight to a temp file
// as they are scanned and the cache is rebuilt from a fixed ring, so peak
// memory stays bounded even for a very large index.
func (s *Store) pruneIndex(cutoffDay time.Time) error {
	f, err := os.Open(s.tsvPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	tmp := s.tsvPath + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(out)

	cleanup := func() { out.Close(); os.Remove(tmp) }
	if _, err := bw.WriteString(indexHeader); err != nil {
		cleanup()
		return err
	}

	ring := newLastN(cacheMax)
	dropped := 0
	var writeErr error
	scanErr := forEachIndexLine(f, func(e IndexEntry) bool {
		if parseEntryTime(e.Time).Before(cutoffDay) {
			dropped++
			return true
		}
		if _, writeErr = bw.WriteString(formatIndexLine(e)); writeErr != nil {
			return false
		}
		ring.add(e)
		return true
	})
	if scanErr != nil {
		cleanup()
		return scanErr
	}
	if writeErr != nil {
		cleanup()
		return writeErr
	}
	if err := bw.Flush(); err != nil {
		cleanup()
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}

	if dropped == 0 {
		os.Remove(tmp) // nothing changed; keep the original
		return nil
	}
	if err := os.Rename(tmp, s.tsvPath); err != nil {
		return err
	}
	s.cache = ring.slice()
	return nil
}

// dayFromDirs parses a "YYYY-MM" month directory name and a "DD" day directory
// name into the UTC midnight of that calendar day.
func dayFromDirs(month, day string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", month+"-"+day)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// readAll loads every index entry in chronological order. Used only to warm the
// cache at startup.
func (s *Store) readAll() ([]IndexEntry, error) {
	f, err := os.Open(s.tsvPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []IndexEntry
	err = forEachIndexLine(f, func(e IndexEntry) bool {
		out = append(out, e)
		return true
	})
	return out, err
}

// formatIndexLine renders one IndexEntry as a TSV row terminated by a newline.
func formatIndexLine(e IndexEntry) string {
	var b strings.Builder
	b.WriteString(tsvField(e.Path))
	b.WriteByte('\t')
	b.WriteString(tsvField(e.Time))
	b.WriteByte('\t')
	b.WriteString(strconv.Itoa(e.Status))
	b.WriteByte('\t')
	b.WriteString(tsvField(e.Endpoint))
	b.WriteByte('\t')
	b.WriteString(tsvField(e.InboundModel))
	b.WriteByte('\t')
	b.WriteString(tsvField(e.ServedBy))
	b.WriteByte('\t')
	b.WriteString(strconv.Itoa(e.Attempts))
	b.WriteByte('\t')
	b.WriteString(intPtrField(e.InTokens))
	b.WriteByte('\t')
	b.WriteString(intPtrField(e.OutTokens))
	b.WriteByte('\t')
	b.WriteString(strconv.FormatInt(e.LatencyMS, 10))
	b.WriteByte('\t')
	b.WriteString(tsvField(e.FinishReason))
	b.WriteByte('\t')
	b.WriteString(tsvField(e.RequestPreview))
	b.WriteByte('\n')
	return b.String()
}

// tsvField makes a value safe to store in a tab-separated, newline-terminated
// record by replacing structural characters with spaces.
func tsvField(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n', '\r', '\x00':
			return ' '
		default:
			return r
		}
	}, s)
}

func intPtrField(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

// forEachIndexLine streams logical lines from the index file, parses each into
// an IndexEntry, and invokes fn. It tolerates corruption: overly long lines are
// truncated to their first fields, NUL blocks (from a crash mid-write) and the
// header are skipped, and unparseable lines are dropped rather than fatal. fn
// returns false to stop early.
func forEachIndexLine(r io.Reader, fn func(IndexEntry) bool) error {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		raw, readErr := readLogicalLine(br)
		if cleaned := cleanLine(raw); cleaned != nil {
			fields := strings.Split(string(cleaned), "\t")
			if e, ok := parseIndexLine(fields); ok {
				if !fn(e) {
					return nil
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// readLogicalLine reads bytes up to the next newline. It retains at most
// maxTSVLine bytes (discarding any excess up to the newline) so a corrupt,
// newline-free region cannot exhaust memory. The returned error is non-nil only
// at EOF or on a read error; any bytes read before EOF are still returned.
func readLogicalLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			return buf, err
		}
		if b == '\n' {
			return buf, nil
		}
		if len(buf) < maxTSVLine {
			buf = append(buf, b)
		}
	}
}

// cleanLine normalizes a raw line: it drops everything from the first NUL byte
// (a partially written/crash-allocated region), trims trailing CR, and returns
// nil for empty lines and the '#' comment header so they are skipped.
func cleanLine(raw []byte) []byte {
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		raw = raw[:i]
	}
	raw = bytes.TrimRight(raw, "\r")
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '#' {
		return nil
	}
	return raw
}

// parseIndexLine maps positional TSV fields to an IndexEntry, taking only the
// leading fields it recognizes. Missing trailing fields default to zero/unknown
// (tolerating older, shorter rows); extra trailing fields are ignored
// (tolerating future, wider rows). A row without a path or a parseable time is
// rejected.
func parseIndexLine(f []string) (IndexEntry, bool) {
	if len(f) < 2 {
		return IndexEntry{}, false
	}
	e := IndexEntry{Path: f[0], Time: f[1]}
	if e.Path == "" || e.Time == "" {
		return IndexEntry{}, false
	}
	if _, err := parseTime(e.Time); err != nil {
		return IndexEntry{}, false
	}
	if len(f) > 2 {
		e.Status = atoiOr(f[2], 0)
	}
	if len(f) > 3 {
		e.Endpoint = f[3]
	}
	if len(f) > 4 {
		e.InboundModel = f[4]
	}
	if len(f) > 5 {
		e.ServedBy = f[5]
	}
	if len(f) > 6 {
		e.Attempts = atoiOr(f[6], 0)
	}
	if len(f) > 7 {
		e.InTokens = atoiPtr(f[7])
	}
	if len(f) > 8 {
		e.OutTokens = atoiPtr(f[8])
	}
	if len(f) > 9 {
		e.LatencyMS = int64(atoiOr(f[9], 0))
	}
	if len(f) > 10 {
		e.FinishReason = f[10]
	}
	if len(f) > 11 {
		e.RequestPreview = f[11]
	}
	return e, true
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func atoiPtr(s string) *int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return &n
	}
	return nil
}

// parseEntryTime parses a stored timestamp, falling back to the current time so
// callers always get a usable value for path derivation and ordering.
func parseEntryTime(s string) time.Time {
	if t, err := parseTime(s); err == nil {
		return t
	}
	return time.Now().UTC()
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// lastN keeps at most n of the most recently added entries, in insertion
// order, using a fixed-size circular buffer so memory stays bounded regardless
// of how many entries are fed through it.
type lastN struct {
	buf   []IndexEntry
	total int
}

func newLastN(n int) *lastN { return &lastN{buf: make([]IndexEntry, n)} }

func (l *lastN) add(e IndexEntry) {
	l.buf[l.total%len(l.buf)] = e
	l.total++
}

// slice returns the retained entries in chronological (insertion) order.
func (l *lastN) slice() []IndexEntry {
	n := len(l.buf)
	if l.total <= n {
		out := make([]IndexEntry, l.total)
		copy(out, l.buf[:l.total])
		return out
	}
	out := make([]IndexEntry, n)
	start := l.total % n
	copy(out, l.buf[start:])
	copy(out[n-start:], l.buf[:start])
	return out
}

func reversed(in []IndexEntry) []IndexEntry {
	out := make([]IndexEntry, len(in))
	for i, e := range in {
		out[len(in)-1-i] = e
	}
	return out
}
