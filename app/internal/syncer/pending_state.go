package syncer

import "klip/internal/model"

// syncBaseline identifies the saved checkpoint preceding a journal mutation.
// Keeping it in the journal avoids adding fields to shared settings files.
type syncBaseline struct {
	Fingerprint string `json:"fingerprint"`
	Version     string `json:"version"`
	UpdatedAt   string `json:"updatedAt"`
	SHA256      string `json:"sha256"`
}

func syncBaselineOf(s model.Settings) syncBaseline {
	return syncBaseline{
		model.Str(s.SyncLastLocalFingerprint), model.Str(s.SyncLastRemoteVersion),
		model.Str(s.SyncLastRemoteUpdatedAt), model.Str(s.SyncLastRemoteSha256),
	}
}
