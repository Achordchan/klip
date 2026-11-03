package syncer

import (
	"context"
	"testing"
	"time"

	"klip/internal/model"
)

func TestPendingFingerprintSurvivesRevertingHistory(t *testing.T) {
	before := []model.Item{textItemAt("local", "local")}
	st := model.DefaultSettings()
	st.SyncLastLocalFingerprint = model.Ptr(pendingUploadFingerprint)
	st.SyncLastRemoteVersion = model.Ptr("v1")
	now := time.Now()
	d := Decide(ManifestInfo{Version: "v1", UpdatedAt: &now}, Fingerprint(before), len(before), &now, st)
	if d.Direction != Upload {
		t.Fatalf("reverted history must upload: %+v", d)
	}
}

func TestRecoveryDoesNotReplaySupersededJournal(t *testing.T) {
	srv := serveSnapshot(t, nil)
	defer srv.Close()
	st := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, st, &unitHistory{})
	if err := syn.writePendingMerge(pendingMergeState{Fingerprint: "old", Version: "v1", Committed: true}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after saving the newer checkpoint but before removing
	// the journal, followed by an independent local edit.
	if err := syn.PersistSyncState("uploaded", ManifestInfo{Version: "v2"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := syn.recoverPendingMerge(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if model.Str(st.SyncLastLocalFingerprint) != "uploaded" || model.Str(st.SyncLastRemoteVersion) != "v2" {
		t.Fatalf("new checkpoint overwritten: %+v", syncBaselineOf(*st))
	}
}

func TestFailedSupersedingCheckpointKeepsJournal(t *testing.T) {
	srv := serveSnapshot(t, nil)
	defer srv.Close()
	st := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, st, &unitHistory{})
	if err := syn.writePendingMerge(pendingMergeState{Fingerprint: "old", Version: "v1", Committed: true}); err != nil {
		t.Fatal(err)
	}
	st.SyncLastLocalFingerprint = model.Ptr("new-memory-only")
	fail := true
	syn.cfg.Settings = failingSettings(st, &fail)
	if err := syn.recoverPendingMerge(context.Background(), ""); err == nil {
		t.Fatal("unsaved checkpoint retired journal")
	}
	if _, ok := syn.readPendingMerge(); !ok {
		t.Fatal("recovery evidence deleted")
	}
}
