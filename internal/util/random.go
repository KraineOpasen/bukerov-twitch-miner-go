package util

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
)

// RandomHex generates a random hex string of the specified byte length.
// The returned string will be 2*bytes characters long.
func RandomHex(bytes int) string {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should never fail on a healthy system; if it does the
		// caller silently gets a non-random (all-zero) value — e.g. a fixed
		// DeviceID or transaction ID. Surface it loudly instead of hiding it.
		slog.Error("crypto/rand.Read failed; returning non-random hex", "bytes", bytes, "error", err)
		return strings.Repeat("0", bytes*2)
	}
	return hex.EncodeToString(b)
}

// DeviceID generates a random 32-character device ID (16 bytes as hex)
func DeviceID() string {
	return RandomHex(16)
}
