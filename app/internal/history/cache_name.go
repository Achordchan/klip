package history

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

func CaptureCacheName(index int, name string) string {
	prefix := fmt.Sprintf("%04d_", index)
	limit := 255 - len(prefix)
	if len(name) > limit {
		ext := filepath.Ext(name)
		if len(ext) > 32 {
			ext = ""
		}
		stem := strings.TrimSuffix(name, ext)
		n := limit - len(ext)
		for n > 0 && !utf8.RuneStart(stem[n]) {
			n--
		}
		name = stem[:n] + ext
	}
	return prefix + name
}
