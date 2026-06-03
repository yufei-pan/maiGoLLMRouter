package server

import (
	"crypto/rand"
	"encoding/hex"
)

func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}

// maskKey hides all but the last 4 characters of a key for safe logging.
func maskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return "****"
	}
	return "****" + key[len(key)-4:]
}
