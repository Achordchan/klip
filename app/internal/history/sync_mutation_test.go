package history

import (
	"context"
	"klip/internal/model"
	"klip/internal/paths"
	"os"
	"path/filepath"
	"testing"
)

func TestSyncMutationAndDeferredCleanup(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	s := NewService(l)
	if err := os.MkdirAll(l.Images(), 0700); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(l.Images(), "kept.png")
	if err := os.WriteFile(payload, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	item := model.Item{ID: "one", Kind: model.KindImage, ImageFilePath: &payload}
	if err := s.Mutate(context.Background(), func([]model.Item) ([]model.Item, error) { return []model.Item{item}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveFiles([]string{payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(payload); err != nil {
		t.Fatal("live payload deleted")
	}
	st := model.DefaultSettings()
	st.MaxItems = 0
	st.ExcludePinnedFromLimits = false
	items, removed := s.ApplyLimitsAndSnapshot(st)
	if len(items) != 0 || len(removed) != 1 {
		t.Fatalf("items=%v removed=%v", items, removed)
	}
	if _, err := os.Stat(payload); err != nil {
		t.Fatal("payload deleted before persistence")
	}
	if err := s.RemoveFiles([]string{payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(payload); !os.IsNotExist(err) {
		t.Fatal("unreferenced payload retained")
	}
}

func TestClearedHistoryPersistsMutationAcrossRestart(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	s := NewService(l)
	marker := "0123456789abcdef0123456789abcdef"
	if err := s.MutateWithMarker(context.Background(), marker, func([]model.Item) ([]model.Item, error) {
		return []model.Item{{ID: "one", Kind: model.KindText, Text: model.Ptr("hello")}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	s.ClearAll()
	p := NewPersister(s, l)
	if err := p.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if SavedMutation(l) != marker {
		t.Fatal("clear erased transaction evidence")
	}
	fresh := NewService(l)
	next := NewPersister(fresh, l)
	if err := next.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := next.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fresh.Len() != 0 || SavedMutation(l) != marker {
		t.Fatal("restart lost empty-history marker")
	}
}
