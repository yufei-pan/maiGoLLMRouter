package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

// maxModelName is the longest inbound model name, in bytes. The longest name
// in two months of logs was 57; a name is kept in the UI cache, index.tsv and
// the blackout map, so one without a bound could be 32 MB.
const maxModelName = 256

var invalidModelMessage = fmt.Sprintf("model must be at most %d bytes with no control characters", maxModelName)

// modelRefusal is "" for a name the router may route, log and show, else the
// reason it is refused. A control character (C0, DEL or C1) could forge a line
// in a log or a view that prints the name.
func modelRefusal(model string) string {
	if len(model) > maxModelName || strings.ContainsFunc(model, unicode.IsControl) {
		return invalidModelMessage
	}
	return ""
}

// writeCodedError is writeError's shape plus a machine-readable "code", for
// refusals a client may want to tell apart.
func writeCodedError(w http.ResponseWriter, status int, msg, typ, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
}

// maxBodyValues bounds the JSON values in one request body. Measured 2026-11-09: the
// largest of 154,614 real requests (Oct–Nov) held 3,452 values, p99.9 1,542; 100,000 is
// ~29x that. Without a bound, dialect conversion of a 32 MiB body of tiny messages took
// 13.6 GiB (Gemini) in Task 6's measurement.
const maxBodyValues = 100_000

var tooManyValuesMessage = fmt.Sprintf("request body has more than %d JSON values", maxBodyValues)

// countExceeds reports whether v holds more than limit JSON values. Every value counts once:
// each object, array, string, number, bool and null, the top-level value included; object
// keys don't count. It stops walking as soon as the count passes limit.
func countExceeds(v any, limit int) bool {
	return countValues(v, limit) > limit
}

// countValues counts v's JSON values, stopping once the count passes limit, so
// it returns at most limit+1. It recurses as deep as v nests, which
// encoding/json caps at 10,000 levels.
func countValues(v any, limit int) int {
	n := 0
	var walk func(any)
	walk = func(v any) {
		n++
		switch t := v.(type) {
		case map[string]any:
			for _, e := range t {
				if n > limit {
					return
				}
				walk(e)
			}
		case []any:
			for _, e := range t {
				if n > limit {
					return
				}
				walk(e)
			}
		}
	}
	walk(v)
	return n
}
