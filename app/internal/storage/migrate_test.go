package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationCopiesAndRemapsWithoutDeletingSource(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	selection := filepath.Join(t.TempDir(), "location.json")
	if err := os.Mkdir(filepath.Join(src, "images"), 0700); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(src, "images", "a.png")
	if err := os.WriteFile(image, []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "original.txt")
	items := []map[string]any{{"ImageFilePath": image, "FilePaths": []string{image, external}, "FutureField": "preserved"}}
	b, _ := json.Marshal(items)
	b = append(b, []byte("\n\t \t")...)
	os.WriteFile(filepath.Join(src, "history.json"), b, 0600)
	if err := Migrate(src, dst, selection, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "history.json"))
	var decoded []map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(dst)
	if decoded[0]["ImageFilePath"] != filepath.Join(canonical, "images", "a.png") || decoded[0]["FutureField"] != "preserved" {
		t.Fatal(decoded)
	}
	if decoded[0]["FilePaths"].([]any)[1] != external {
		t.Fatal("external path rewritten")
	}
	if _, err := os.Stat(image); err != nil {
		t.Fatal("source removed")
	}
	if _, err := os.Stat(selection); err != nil {
		t.Fatal(err)
	}
}
func TestRejectsOccupiedNestedAndLinkedTargets(t *testing.T) {
	src := t.TempDir()
	nested := filepath.Join(src, "nested")
	os.Mkdir(nested, 0700)
	if _, err := Validate(src, nested); err == nil {
		t.Fatal("nested target accepted")
	}
	dst := t.TempDir()
	os.WriteFile(filepath.Join(dst, "keep"), []byte("user"), 0600)
	if _, err := Validate(src, dst); err == nil {
		t.Fatal("occupied target accepted")
	}
}
func TestSelectionFailureKeepsOldLocationAndCleansCopies(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(src, "payload"), []byte("keep"), 0600)
	selection := t.TempDir() // replacing a directory must fail
	if err := Migrate(src, dst, selection, nil); err == nil {
		t.Fatal("expected selection failure")
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Fatal("partial files remain")
	}
	if _, err := os.Stat(filepath.Join(src, "payload")); err != nil {
		t.Fatal("source lost")
	}
}

func TestInvalidHistoryNeverChangesSelection(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	selection := filepath.Join(t.TempDir(), "location.json")
	os.WriteFile(filepath.Join(src, "history.json"), []byte("broken"), 0600)
	if err := Migrate(src, dst, selection, nil); err == nil {
		t.Fatal("invalid history accepted")
	}
	if _, err := os.Stat(selection); !os.IsNotExist(err) {
		t.Fatal("selection changed")
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Fatal("failed copies retained")
	}
}

func TestRecoveryRecordsAreMatchedByTheirPlace(t *testing.T) {
	for rel, want := range map[string]bool{
		"sync/payload-journal.json":            true,
		"files/12.capture-backup/a.txt":        true,
		"files/12.capture-backup":              true,
		"files/12.capture-backup.capture-time": true,
		"files/12/payload-journal.pdf":         false,
		"files/12/payload-replacement.txt":     false,
		"files/12/report.capture-backup":       false,
		"images/payload-journal.png":           false,
		"sync/state.json":                      false,
	} {
		if got := recoveryRecord(filepath.FromSlash(rel)); got != want {
			t.Errorf("%s: %v, want %v", rel, got, want)
		}
	}
}

func TestUserFileNamedLikeAJournalDoesNotBlockMigration(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	selection := filepath.Join(t.TempDir(), "location.json")
	dir := filepath.Join(src, "files", "12")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "payload-journal.pdf"), []byte("user"), 0600)
	os.WriteFile(filepath.Join(src, "history.json"), []byte("[]"), 0600)
	if err := Migrate(src, dst, selection, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "files", "12", "payload-journal.pdf")); err != nil {
		t.Fatal(err)
	}
}
