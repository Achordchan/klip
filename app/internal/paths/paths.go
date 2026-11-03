// Package paths says where Klip keeps its data. On Windows it is the folder
// Maccy 1.0.10 and older used, so that an upgrade finds its history.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Layout is the data folder and what is in it.
type Layout struct{ Root string }

// Default returns the data folder of this platform.
func Default() (Layout, error) {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return Layout{}, err
			}
			base = filepath.Join(home, "AppData", "Local")
		}
		return Layout{filepath.Join(base, "maccy")}, nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return Layout{}, err
		}
		return Layout{filepath.Join(home, "Library", "Application Support", "Klip")}, nil
	}
	if base := os.Getenv("XDG_DATA_HOME"); base != "" {
		return Layout{filepath.Join(base, "klip")}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Layout{}, err
	}
	return Layout{filepath.Join(home, ".local", "share", "klip")}, nil
}

func (l Layout) Settings() string { return filepath.Join(l.Root, "settings.json") }
func (l Layout) History() string  { return filepath.Join(l.Root, "history.json") }
func (l Layout) Images() string   { return filepath.Join(l.Root, "images") }
func (l Layout) Files() string    { return filepath.Join(l.Root, "files") }
func (l Layout) Blobs() string    { return filepath.Join(l.Root, "blobs") }

// BlobPath is where a blob with this SHA-256 lives locally:
// blobs/aa/bb/<sha>.blob, the hash in lower case. Anything that is not a
// SHA-256 has no path: a hash in a crafted snapshot must not become one.
func (l Layout) BlobPath(sha256 string) string {
	n := strings.ToLower(strings.TrimSpace(sha256))
	if !isSHA256(n) {
		return ""
	}
	return filepath.Join(l.Blobs(), n[:2], n[2:4], n+".blob")
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
