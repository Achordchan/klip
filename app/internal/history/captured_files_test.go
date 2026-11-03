package history

import (
	"klip/internal/model"
	"klip/internal/paths"
	"os"
	"path/filepath"
	"testing"
)

func TestRepeatedFileCaptureReplacesCachedBytes(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	s := NewService(l)
	for i, id := range []string{"old", "new"} {
		dir := filepath.Join(l.Files(), id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "0000_doc.txt"), []byte{byte('a' + i)}, 0600); err != nil {
			t.Fatal(err)
		}
		s.Add(model.Item{ID: id, Kind: model.KindFileList, ContentHash: model.Ptr("files:same"), FilePaths: []string{"/original/doc.txt"}, CopyCount: 1})
	}
	if s.Len() != 1 {
		t.Fatal("duplicate not merged")
	}
	b, err := os.ReadFile(filepath.Join(l.Files(), "old", "0000_doc.txt"))
	if err != nil || string(b) != "b" {
		t.Fatalf("stale cached bytes: %q %v", b, err)
	}
	if err := s.RemoveFiles([]string{filepath.Join(l.Files(), "old", "0000_doc.txt")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.Files(), "old", "0000_doc.txt")); err != nil {
		t.Fatal("implicit live cache deleted")
	}
}

func TestIncompleteReplacementKeepsOldCache(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	s := NewService(l)
	for _, id := range []string{"old", "new"} {
		if err := os.MkdirAll(filepath.Join(l.Files(), id), 0700); err != nil {
			t.Fatal(err)
		}
	}
	old := filepath.Join(l.Files(), "old", "0000_doc.txt")
	if err := os.WriteFile(old, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if s.replaceCapturedFiles(model.Item{ID: "old"}, model.Item{ID: "new", FilePaths: []string{"/original/doc.txt"}}) {
		t.Fatal("partial cache accepted")
	}
	if b, err := os.ReadFile(old); err != nil || string(b) != "old" {
		t.Fatal("old cache lost")
	}
}

func TestDuplicateImageRepairsMissingPayload(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	s := NewService(l)
	missing := filepath.Join(t.TempDir(), "missing.png")
	fresh := filepath.Join(t.TempDir(), "fresh.png")
	if err := os.WriteFile(fresh, []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{missing, fresh} {
		s.Add(model.Item{ID: []string{"old", "new"}[i], Kind: model.KindImage, ImageFilePath: &p, ContentHash: model.Ptr("same"), CopyCount: 1})
	}
	if s.Len() != 1 || model.Str(s.Items()[0].ImageFilePath) != fresh {
		t.Fatal("fresh image not retained")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh image deleted")
	}
}

func TestInterruptedCaptureReplacementRestoresRetainedID(t *testing.T) {
	root := t.TempDir()
	backup := filepath.Join(root, "retained.capture-backup")
	if err := os.Mkdir(backup, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "0000_doc.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	recoverCapturedFiles(root)
	if b, err := os.ReadFile(filepath.Join(root, "retained", "0000_doc.txt")); err != nil || string(b) != "old" {
		t.Fatal("interrupted replacement not recovered")
	}
}

func TestHistoryCleanupNeverDeletesExternalImage(t *testing.T) {
	s := NewService(paths.Layout{Root: t.TempDir()})
	external := filepath.Join(t.TempDir(), "original.png")
	if err := os.WriteFile(external, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	s.Add(model.Item{ID: "x", Kind: model.KindImage, ImageFilePath: &external})
	s.ClearAll()
	if _, err := os.Stat(external); err != nil {
		t.Fatal("external image deleted")
	}
}

func TestUnreadableCaptureEvidencePreservesBothCaches(t *testing.T) {
	for _, evidence := range []string{"missing", "truncated"} {
		t.Run(evidence, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"item", "item.capture-backup"} {
				if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, name, "payload"), []byte(name), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if evidence == "truncated" {
				if err := os.WriteFile(filepath.Join(root, "item.capture-backup.capture-time"), []byte(`"partial`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			recoverCapturedFiles(root)
			for _, name := range []string{"item", "item.capture-backup"} {
				b, err := os.ReadFile(filepath.Join(root, name, "payload"))
				if err != nil || string(b) != name {
					t.Fatalf("lost %s: %q %v", name, b, err)
				}
			}
		})
	}
}

func TestCaptureEvidencePublicationFailureLeavesCachesUntouched(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	s := NewService(l)
	s.deferCleanup = true
	for _, id := range []string{"old", "new"} {
		if err := os.MkdirAll(filepath.Join(l.Files(), id), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(l.Files(), id, "0000_doc.txt"), []byte(id), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(l.Files(), "old.capture-backup.capture-time"), 0700); err != nil {
		t.Fatal(err)
	}
	if s.replaceCapturedFiles(model.Item{ID: "old", LocalCaptureMutation: "old-marker"}, model.Item{ID: "new", LocalCaptureMutation: "new-marker", FilePaths: []string{"doc.txt"}}) {
		t.Fatal("replacement proceeded without durable evidence")
	}
	for _, id := range []string{"old", "new"} {
		b, err := os.ReadFile(filepath.Join(l.Files(), id, "0000_doc.txt"))
		if err != nil || string(b) != id {
			t.Fatalf("changed %s: %q %v", id, b, err)
		}
	}
}
