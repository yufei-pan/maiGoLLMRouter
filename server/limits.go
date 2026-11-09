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
