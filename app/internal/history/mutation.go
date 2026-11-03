package history

import (
	"bytes"
	"encoding/hex"
	"klip/internal/paths"
	"os"
)

// A JSON whitespace suffix carries local transaction evidence without changing
// the legacy array schema. It is written in the same atomic rename as items,
// including an empty array. Legacy readers may discard it, never forge it.
var mutationPrefix = []byte("\n\t\n\t\n")

func withMutation(data []byte, mutation string) []byte {
	raw, err := hex.DecodeString(mutation)
	if err != nil || len(raw) != 16 {
		return data
	}
	out := append(append([]byte(nil), data...), mutationPrefix...)
	for _, b := range raw {
		for bit := 7; bit >= 0; bit-- {
			if b&(1<<bit) != 0 {
				out = append(out, '\t')
			} else {
				out = append(out, ' ')
			}
		}
	}
	return out
}

// SavedMutation reads evidence from the exact history file that survived a
// crash, not from current in-memory items or a separately committed sidecar.
func SavedMutation(l paths.Layout) string {
	data, err := os.ReadFile(l.History())
	if err != nil {
		return ""
	}
	const bits = 128
	if len(data) < len(mutationPrefix)+bits {
		return ""
	}
	start := len(data) - bits
	if !bytes.Equal(data[start-len(mutationPrefix):start], mutationPrefix) {
		return ""
	}
	raw := make([]byte, 16)
	for i, b := range data[start:] {
		if b != ' ' && b != '\t' {
			return ""
		}
		if b == '\t' {
			raw[i/8] |= 1 << uint(7-i%8)
		}
	}
	return hex.EncodeToString(raw)
}
