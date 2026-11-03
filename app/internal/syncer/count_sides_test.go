package syncer

import (
	"klip/internal/model"
	"testing"
)

func TestDownloadMergePreservesCountsWithinLocalSide(t *testing.T) {
	a := textItemAt("a", "same")
	a.CopyCount = 2
	a.ContentHash = model.Ptr("same")
	b := a
	b.ID = "b"
	b.CopyCount = 3
	got := MergeForDownload(nil, []model.Item{a, b}, nil)
	if len(got) != 1 || got[0].CopyCount != 5 {
		t.Fatalf("counts lost: %+v", got)
	}
}

func TestCountsFollowTransitiveIdentity(t *testing.T) {
	a := textItemAt("a", "same")
	a.CopyCount = 100
	a.ContentHash = nil
	b := textItemAt("b", "same")
	b.CopyCount = 1
	b.ContentHash = model.Ptr("H")
	local := a
	local.CopyCount = 2
	local.ContentHash = model.Ptr("H")
	got := MergeForDownload([]model.Item{a, b}, []model.Item{local}, nil)
	if len(got) != 1 || got[0].CopyCount != 101 {
		t.Fatalf("transitive group lost counts: %+v", got)
	}
}

func TestCountsUseCaseInsensitiveHashes(t *testing.T) {
	remote := textItemAt("a", "same")
	remote.ContentHash = model.Ptr("ABC")
	remote.CopyCount = 10
	local := textItemAt("b", "same")
	local.ContentHash = model.Ptr("abc")
	local.CopyCount = 1
	got := MergeForDownload([]model.Item{remote}, []model.Item{local}, nil)
	if len(got) != 1 || got[0].CopyCount != 10 {
		t.Fatalf("mixed case lost counts: %+v", got)
	}
}

func TestRollbackRetainsOnlyConcurrentCountDelta(t *testing.T) {
	before := textItemAt("a", "same")
	before.CopyCount = 2
	applied := before
	applied.CopyCount = 7
	current := applied
	current.CopyCount = 8
	got := rollbackItems([]model.Item{before}, []model.Item{applied}, []model.Item{current}, nil)
	if len(got) != 1 || got[0].CopyCount != 3 {
		t.Fatalf("merge contribution survived rollback: %+v", got)
	}
}
