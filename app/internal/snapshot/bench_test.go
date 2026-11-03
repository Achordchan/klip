package snapshot

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"klip/internal/model"
	"klip/internal/paths"
)

// TestExportSpeed gives the time to export and read a long history of text.
func TestExportSpeed(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	items := make([]model.Item, 5000)
	now := time.Now()
	for i := range items {
		items[i] = model.Item{ID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i), Kind: model.KindText,
			CapturedAt: model.Time{Time: now}, Text: model.Ptr(fmt.Sprintf("entry %d 剪贴板", i)), CopyCount: 1}
	}
	snap := filepath.Join(t.TempDir(), "s.sqlite")
	t0 := time.Now()
	res, err := Export(ctx, l, snap, items, model.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}
	t1 := time.Now()
	got, applied, err := Read(ctx, l, snap, nil)
	if err != nil || len(got.Items) != len(items) {
		t.Fatalf("%v %d", err, len(got.Items))
	}
	applied.Commit()
	t.Logf("5000 items: export %v (%d KB), read %v", t1.Sub(t0).Round(time.Millisecond), res.Size/1024, time.Since(t1).Round(time.Millisecond))
}
