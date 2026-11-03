package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestImportedFileNameReservesPrefix(t *testing.T) {
	for _, name := range []string{strings.Repeat("a", 251) + ".txt", strings.Repeat("文", 83) + ".txt", "short.txt", strings.Repeat("x", 260) + "." + strings.Repeat("z", 260)} {
		got := importedFileName(12345, name)
		if len(got) > 255 || !utf8.ValidString(got) {
			t.Fatalf("invalid filename: %q", got)
		}
		if strings.HasSuffix(name, ".txt") && !strings.HasSuffix(got, ".txt") {
			t.Fatal("extension lost")
		}
		if err := os.WriteFile(filepath.Join(t.TempDir(), got), []byte("payload"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestImageExtensionBudget(t *testing.T) {
	ext := filepath.Ext(sanitizeFileName("image." + strings.Repeat("界", 100)))
	ext = truncateUTF8(ext, 32)
	name := "1780000000000_0123456789abcdef0123456789abcdef" + ext
	if len(name) > 255 || !utf8.ValidString(name) {
		t.Fatal("invalid generated name")
	}
	if err := os.WriteFile(filepath.Join(t.TempDir(), name), []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
}
