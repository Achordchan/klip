package syncer

import (
	"klip/internal/model"
	"testing"
)

func TestUploadIntentRecoversPublishedSnapshot(t *testing.T) {
	srv := serveSnapshot(t, nil)
	defer srv.Close()
	st := signedIn(model.DefaultSettings())
	s := unitSyncer(t, srv, st, &unitHistory{})
	if err := s.writeUploadIntent(uploadIntent{Fingerprint: "exported", SHA: "receipt"}); err != nil {
		t.Fatal(err)
	}
	if err := s.recoverUploadIntent(ManifestInfo{Version: "v1", SnapshotSHA256: "receipt"}, ""); err != nil {
		t.Fatal(err)
	}
	if model.Str(st.SyncLastLocalFingerprint) != "exported" {
		t.Fatal("published upload not recovered")
	}
}
