package update

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteJSONFailureKeepsExistingDestination(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "manifest.json")
	if err := os.Mkdir(dest, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dest, "original")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(dest, map[string]string{"new": "value"}); err == nil {
		t.Fatal("invalid replacement succeeded")
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "keep" {
		t.Fatal("existing destination lost")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatal("temporary file leaked")
	}
}
func TestRetireRecoveryJournal(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(recoveryPath(root), []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeRecoveryJournal(root, func(time.Duration) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(recoveryPath(root)); !os.IsNotExist(err) {
		t.Fatal("journal remains")
	}
}
