package snapshot

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// importedFileName reserves the index prefix within the common 255-byte
// component limit. A UTF-8 byte bound also bounds Windows UTF-16 units.
func importedFileName(index int, original string) string {
	prefix := fmt.Sprintf("%04d_", index)
	name := sanitizeFileName(original)
	budget := 255 - len(prefix)
	if len(name) <= budget {
		return prefix + name
	}
	ext := filepath.Ext(name)
	// Pathological extensions must be bounded too; ordinary extensions remain
	// intact so cached documents still open with their registered application.
	if len(ext) > budget/2 {
		ext = truncateUTF8(ext, budget/2)
	}
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	stem = strings.TrimRight(truncateUTF8(stem, budget-len(ext)), ". ")
	if stem == "" {
		stem = "file"
	}
	return prefix + stem + ext
}

func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}
