package syncer

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"klip/internal/model"
)

// ParseManifestInfo reads the manifest the server sends. A text that is
// empty, not JSON, or without "latest" gives an empty info (which means that
// the server has nothing yet).
func ParseManifestInfo(manifestJSON string) ManifestInfo {
	info := ManifestInfo{RawJSON: manifestJSON}
	if strings.TrimSpace(manifestJSON) == "" {
		return info
	}
	var doc struct {
		Latest *struct {
			Version   json.RawMessage `json:"version"`
			UpdatedAt json.RawMessage `json:"updatedAt"`
			Snapshot  *struct {
				SHA256 json.RawMessage `json:"sha256"`
				Size   json.RawMessage `json:"size"`
			} `json:"snapshot"`
		} `json:"latest"`
	}
	if json.Unmarshal([]byte(manifestJSON), &doc) != nil || doc.Latest == nil {
		return info
	}
	info.Version = rawText(doc.Latest.Version)
	if text := rawText(doc.Latest.UpdatedAt); strings.TrimSpace(text) != "" {
		if t, err := model.ParseTime(text); err == nil {
			info.UpdatedAt = &t
		}
	}
	if s := doc.Latest.Snapshot; s != nil {
		info.SnapshotSHA256 = rawText(s.SHA256)
		var n int64
		if json.Unmarshal(s.Size, &n) == nil {
			info.SnapshotSize = &n
		} else {
			var text string
			if json.Unmarshal(s.Size, &text) == nil {
				if v, err := parseInt(text); err == nil {
					info.SnapshotSize = &v
				}
			}
		}
	}
	return info
}

// ParseManifestStrict reads a successful manifest answer. Unlike
// ParseManifestInfo, a body that is not a manifest is an error: a proxy's HTML
// page, a truncated body or an invalid time must not become "the server has
// no snapshot", which would upload over the remote one.
func ParseManifestStrict(manifestJSON string) (ManifestInfo, error) {
	// The whole structure is decoded here, not just checked and then read
	// again leniently: a snapshot that is not an object must fail too.
	var doc struct {
		Latest *struct {
			Version   json.RawMessage `json:"version"`
			UpdatedAt json.RawMessage `json:"updatedAt"`
			Snapshot  *struct {
				SHA256 json.RawMessage `json:"sha256"`
				Size   json.RawMessage `json:"size"`
			} `json:"snapshot"`
		} `json:"latest"`
	}
	if err := json.Unmarshal([]byte(manifestJSON), &doc); err != nil {
		return ManifestInfo{}, fmt.Errorf("manifest: %w", err)
	}
	if doc.Latest == nil {
		return ManifestInfo{}, fmt.Errorf("manifest: no latest snapshot")
	}
	at := strings.TrimSpace(rawText(doc.Latest.UpdatedAt))
	if at == "" {
		return ManifestInfo{}, fmt.Errorf("manifest: no updatedAt")
	}
	t, err := model.ParseTime(at)
	if err != nil {
		return ManifestInfo{}, fmt.Errorf("manifest: updatedAt: %w", err)
	}
	info := ManifestInfo{RawJSON: manifestJSON, Version: rawText(doc.Latest.Version), UpdatedAt: &t}
	if s := doc.Latest.Snapshot; s != nil {
		info.SnapshotSHA256 = rawText(s.SHA256)
		var n int64
		if json.Unmarshal(s.Size, &n) == nil {
			info.SnapshotSize = &n
		} else {
			var text string
			if json.Unmarshal(s.Size, &text) == nil {
				if v, err := parseInt(text); err == nil {
					info.SnapshotSize = &v
				}
			}
		}
	}
	if strings.TrimSpace(info.Version) == "" && strings.TrimSpace(info.SnapshotSHA256) == "" {
		return ManifestInfo{}, fmt.Errorf("manifest: missing snapshot identifier")
	}
	return info, nil
}

// rawText is TryReadString: a string as it is, any other value as its JSON
// text, null and missing as "".
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func parseInt(s string) (int64, error) {
	var n int64
	err := json.Unmarshal([]byte(strings.TrimSpace(s)), &n)
	return n, err
}

// Decide is DecideDirectionWithState. localLatest is the newest capture time
// in the history, nil when it is empty.
func Decide(manifest ManifestInfo, localFingerprint string, localCount int, localLatest *time.Time, s model.Settings) Decision {
	if manifest.UpdatedAt == nil {
		dir := None
		if localCount > 0 {
			dir = Upload
		}
		return Decision{dir, localFingerprint, manifest, "remote_missing"}
	}

	lastFp := strings.TrimSpace(model.Str(s.SyncLastLocalFingerprint))
	lastVersion := strings.TrimSpace(model.Str(s.SyncLastRemoteVersion))
	remoteVersion := strings.TrimSpace(manifest.Version)
	lastSha := strings.TrimSpace(model.Str(s.SyncLastRemoteSha256))
	remoteSha := strings.TrimSpace(manifest.SnapshotSHA256)
	remoteMatched := sameRemoteSnapshot(lastVersion, remoteVersion, lastSha, remoteSha)

	if lastFp == pendingUploadFingerprint && !remoteMatched {
		return Decision{Merge, localFingerprint, manifest, "pending_upload_remote_changed"}
	}
	if lastFp != "" && lastVersion != "" {
		switch {
		case isRemoteRollback(model.Str(s.SyncLastRemoteUpdatedAt), manifest.UpdatedAt):
			return Decision{Download, localFingerprint, manifest, "remote_rolled_back"}
		case lastFp == localFingerprint && remoteMatched:
			return Decision{None, localFingerprint, manifest, "state_matched"}
		case lastFp != localFingerprint && remoteMatched:
			return Decision{Upload, localFingerprint, manifest, "local_changed_only"}
		case lastFp == localFingerprint && !remoteMatched:
			return Decision{Download, localFingerprint, manifest, "remote_changed_only"}
		}
	} else {
		if localCount > 0 {
			return Decision{Merge, localFingerprint, manifest, "first_sync_merge"}
		}
		return Decision{Download, localFingerprint, manifest, "first_sync_remote_exists"}
	}

	return Decision{decideByTimestamp(localLatest, manifest.UpdatedAt), localFingerprint, manifest, "timestamp_fallback"}
}

func sameRemoteSnapshot(lastVersion, remoteVersion, lastSha, remoteSha string) bool {
	if lastVersion == remoteVersion {
		if lastSha == "" || remoteSha == "" {
			return true
		}
		return strings.EqualFold(lastSha, remoteSha)
	}
	// A download whose manifest could not be matched records the snapshot
	// hash as the version: it still names the same server file.
	if len(lastVersion) == 64 && lastSha != "" && remoteSha != "" &&
		strings.EqualFold(lastVersion, lastSha) && strings.EqualFold(lastSha, remoteSha) {
		return true
	}
	return false
}

// isRemoteRollback says that the server's snapshot is older (by more than two
// seconds) than the one this device last saw: someone restored a backup.
func isRemoteRollback(lastUpdatedAtText string, remoteUpdatedAt *time.Time) bool {
	if remoteUpdatedAt == nil || strings.TrimSpace(lastUpdatedAtText) == "" {
		return false
	}
	last, err := model.ParseTime(lastUpdatedAtText)
	if err != nil {
		return false
	}
	return remoteUpdatedAt.UTC().Before(last.UTC().Add(-2 * time.Second))
}

func decideByTimestamp(localLatest, remoteUpdatedAt *time.Time) Direction {
	if remoteUpdatedAt == nil {
		if localLatest == nil {
			return None
		}
		return Upload
	}
	if localLatest == nil {
		return Download
	}
	diff := localLatest.UTC().Sub(remoteUpdatedAt.UTC())
	if diff <= 2*time.Second && diff >= -2*time.Second {
		return None
	}
	if diff > 0 {
		return Upload
	}
	return Download
}
