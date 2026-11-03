package syncer

import (
	"context"
	"encoding/json"
	"klip/internal/model"
	"testing"
)

func TestMetadataOnlyMergeRecoveryWithConcurrentCapture(t *testing.T) {
	original := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "shared")
	merged := original
	merged.CopyCount = 2
	merged.LocalSyncMutation = "transaction-unique-id"
	capture := textItemAt("16fd2706-8baf-433b-82eb-8c7fada847da", "new capture")
	for _, saved := range []bool{true, false} {
		t.Run(map[bool]string{true: "saved", false: "not-saved"}[saved], func(t *testing.T) {
			item := original
			if saved {
				item = merged
			} else {
				item.CopyCount = 2
			} // copying alone is not proof
			raw, err := json.Marshal([]model.Item{capture, item})
			if err != nil {
				t.Fatal(err)
			}
			var disk []model.Item
			if err := json.Unmarshal(raw, &disk); err != nil {
				t.Fatal(err)
			}
			st := signedIn(model.DefaultSettings())
			srv := serveSnapshot(t, nil)
			defer srv.Close()
			syn := unitSyncer(t, srv, st, &unitHistory{items: disk})
			p := pendingMergeState{Mutation: merged.LocalSyncMutation, Fingerprint: pendingUploadFingerprint, Version: "v1", Before: Fingerprint([]model.Item{original}), After: Fingerprint([]model.Item{merged}), Applied: Fingerprint([]model.Item{merged})}
			if err := syn.writePendingMerge(p); err != nil {
				t.Fatal(err)
			}
			if err := syn.recoverPendingMerge(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			if got := model.Str(st.SyncLastLocalFingerprint) == pendingUploadFingerprint; got != saved {
				t.Fatalf("recovered=%v saved=%v", got, saved)
			}
		})
	}
}
