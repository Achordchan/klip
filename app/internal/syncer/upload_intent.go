package syncer

import (
	"encoding/json"
	"klip/internal/durable"
	"klip/internal/model"
	"os"
	"path/filepath"
	"strings"
)

type uploadIntent struct {
	Account, Fingerprint, SHA string
	Baseline                  *syncBaseline
}

func (s *Syncer) uploadIntentPath() string {
	return filepath.Join(s.cfg.Layout.Root, "sync", "upload-intent.json")
}
func (s *Syncer) writeUploadIntent(v uploadIntent) error {
	if v.Baseline == nil {
		b := syncBaselineOf(s.settings())
		v.Baseline = &b
	}
	path := s.uploadIntentPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "upload-intent-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return durable.Rename(f.Name(), path)
}
func (s *Syncer) recoverUploadIntent(m ManifestInfo, account string) error {
	b, err := os.ReadFile(s.uploadIntentPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var v uploadIntent
	if err = json.Unmarshal(b, &v); err != nil {
		return err
	}
	if !strings.EqualFold(v.Account, account) {
		return nil
	}
	if v.Baseline != nil && syncBaselineOf(s.settings()) != *v.Baseline {
		if err := s.cfg.Settings.Update(func(*model.Settings) {}); err != nil {
			return err
		}
		return os.Remove(s.uploadIntentPath())
	}
	if strings.EqualFold(v.SHA, m.SnapshotSHA256) && v.SHA != "" {
		if err = s.PersistSyncState(v.Fingerprint, m, account); err != nil {
			return err
		}
	}
	if !strings.EqualFold(v.SHA, m.SnapshotSHA256) && (v.Baseline == nil || !sameRemoteSnapshot(v.Baseline.Version, m.Version, v.Baseline.SHA256, m.SnapshotSHA256)) {
		if err := s.PersistSyncState(pendingUploadFingerprint, ManifestInfo{Version: v.SHA, SnapshotSHA256: v.SHA}, account); err != nil {
			return err
		}
	}
	return os.Remove(s.uploadIntentPath())
}
