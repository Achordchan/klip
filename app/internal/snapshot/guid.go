package snapshot

import (
	"crypto/rand"
	"encoding/hex"
)

// newGUIDN returns 32 random hex digits, the shape of Guid.ToString("N").
func newGUIDN() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
