package provider

import (
	"crypto/rand"
	"encoding/hex"
)

// randomID returns a short hex identifier used to synthesize OpenAI-style
// response IDs for translated dialects.
func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(b[:])
}
