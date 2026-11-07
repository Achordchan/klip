package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fakeHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
}

func TestSelectionSurvivesDeletingTheOldDataFolder(t *testing.T) {
	fakeHome(t)
	def, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := SelectionPath()
	if err != nil {
		t.Fatal(err)
	}
	if rel, err := filepath.Rel(def.Root, sel); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("the choice %s is kept inside the data folder %s", sel, def.Root)
	}
	if err := os.MkdirAll(def.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	moved := t.TempDir()
	if err := SaveSelection(sel, moved); err != nil {
		t.Fatal(err)
	}
	// The user deletes the old folder, the backup.
	if err := os.RemoveAll(def.Root); err != nil {
		t.Fatal(err)
	}
	got, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != filepath.Clean(moved) {
		t.Fatalf("data folder %s, want the moved one %s", got.Root, moved)
	}
}

func TestLegacySelectionIsStillRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("same code; the paths differ only by the environment")
	}
	fakeHome(t)
	legacy, err := legacySelectionPath()
	if err != nil {
		t.Fatal(err)
	}
	moved := t.TempDir()
	if err := SaveSelection(legacy, moved); err != nil {
		t.Fatal(err)
	}
	got, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != filepath.Clean(moved) {
		t.Fatalf("data folder %s, want %s", got.Root, moved)
	}
	// It was written to the new place: deleting the old data folder, with
	// the old file in it, keeps it.
	def, _ := Default()
	if err := os.RemoveAll(def.Root); err != nil {
		t.Fatal(err)
	}
	if got, err := Current(); err != nil || got.Root != filepath.Clean(moved) {
		t.Fatalf("after deleting the old folder: %v, %v", got, err)
	}
}
