package syncer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"klip/internal/durable"
	"klip/internal/history"
	"klip/internal/model"
	"klip/internal/snapshot"
	"klip/internal/syncclient"
)

// Syncer runs the sync operations. One operation runs at a time for a data
// folder, as with the static gate of SyncService: two Syncers of the same
// folder share their locks (rootLocks).
type Syncer struct {
	cfg     Config
	session *syncclient.Session
	*rootLocks
	// changed is told of a change of the history that the sync's own
	// suppression may have hidden (mutateHistory); NewAuto sets it.
	changed    func()
	unrepaired atomic.Pointer[string]
}

// rootLocks are the locks of a data folder. gate serializes the operations
// that talk to the server or change the folder; workflow is held for a whole
// Run, so that two runs cannot decide on the same manifest and apply it twice
// (and the pending-merge state of the folder is one run's at a time).
type rootLocks struct {
	gate     sync.Mutex
	workflow sync.Mutex
	// gen counts the workflows begun (under workflow): a restore's state is
	// recorded only if no other one began since (PersistAfterDownload).
	gen             uint64
	rollbackPending bool
}

// beginWorkflow takes the workflow lock for a whole operation and counts it.
func (s *Syncer) beginWorkflow() {
	s.workflow.Lock()
	s.gen++
}

var (
	locksMu sync.Mutex
	locks   = map[string]*rootLocks{}
)

// locksFor returns the locks of the data folder root, the same for every
// Syncer of it.
func locksFor(root string) *rootLocks {
	key := filepath.Clean(root)
	if abs, err := filepath.Abs(key); err == nil {
		key = abs
	}
	locksMu.Lock()
	defer locksMu.Unlock()
	l, ok := locks[key]
	if !ok {
		l = &rootLocks{}
		locks[key] = l
	}
	return l
}

// New returns a Syncer. cfg.Client, Settings, History, Layout and BaseURL are
// required; Flush too, for a download or a merge (ErrNoFlush).
func New(cfg Config) *Syncer {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Syncer{
		cfg:       cfg,
		rootLocks: locksFor(cfg.Layout.Root),
		session: &syncclient.Session{
			Client: cfg.Client, Settings: cfg.Settings, Now: cfg.Now, BaseURL: cfg.BaseURL,
		},
	}
}

func (s *Syncer) settings() model.Settings { return s.cfg.Settings.Get() }

// flush saves the history (Config.Flush).
func (s *Syncer) flush(ctx context.Context) error {
	if s.cfg.Flush == nil {
		return ErrNoFlush
	}
	err := s.cfg.Flush(ctx)
	if err == nil {
		s.rollbackPending = false
	}
	return err
}

// ---- connection and tokens

// connection returns the access token to use, after the checks of
// GetConnectionAsync: the address is the official one, a token that expires
// within a minute is renewed, and one that cannot be used is "not logged in".
func (s *Syncer) connection(ctx context.Context) (string, error) {
	cur := s.settings()
	base := strings.TrimRight(s.cfg.BaseURL, "/")
	if !strings.EqualFold(strings.TrimRight(strings.TrimSpace(cur.NasAgentBaseUrl), "/"), base) {
		s.cfg.Settings.Update(func(m *model.Settings) { m.NasAgentBaseUrl = s.cfg.BaseURL })
		cur = s.settings()
	}
	nowMs := s.cfg.Now().UnixMilli()
	if cur.AuthExpiresAtUnixMs <= nowMs+60_000 {
		// A temporary failure must reach the caller as it is: the tokens are
		// still there, so "not logged in" would be wrong.
		if applied, err := s.session.Refresh(ctx); err != nil {
			return "", err
		} else if !applied {
			if model.Str(s.settings().AuthAccessToken) == "" {
				return "", ErrNotLoggedIn
			}
			return "", ErrSessionChanged
		}
	}
	cur = s.settings()
	token := strings.TrimSpace(model.Str(cur.AuthAccessToken))
	if token == "" || cur.AuthExpiresAtUnixMs <= nowMs+30_000 {
		return "", ErrNotLoggedIn
	}
	return token, nil
}

func (s *Syncer) bearer() string {
	return strings.TrimSpace(model.Str(s.settings().AuthAccessToken))
}

// currentAccount is the account of the stored access token, "" when the
// token does not say (an opaque one).
func (s *Syncer) currentAccount() string { return syncclient.AccountID(s.bearer()) }

// accountChanged says a known account is no longer the signed-in one: the
// user signed out or signed into another account during the workflow.
func (s *Syncer) accountChanged(account string) bool {
	if strings.TrimSpace(account) == "" {
		return false
	}
	cur := s.currentAccount()
	if cur == "" {
		// Signed out (or the token is gone): the session the workflow started
		// in is not there any more.
		return true
	}
	return !strings.EqualFold(cur, account)
}

// checkToken says the access token belongs to account. The token itself is
// checked, not the settings: a sign-in between reading the settings and
// sending the request cannot swap the credentials of a workflow.
func (s *Syncer) checkToken(token, account string) error {
	if strings.TrimSpace(account) == "" {
		return nil
	}
	if id := syncclient.AccountID(token); id != "" && !strings.EqualFold(id, account) {
		return ErrSessionChanged
	}
	return nil
}

// ensureAccount refuses when the workflow's account changed and the token
// about to be used belongs to another one.
func (s *Syncer) ensureAccount(token, account string) error {
	if s.accountChanged(account) {
		return ErrSessionChanged
	}
	return s.checkToken(token, account)
}

// tokenFor is checkToken for the stored token: the token the caller gets is
// the one it will send, so it cannot be replaced by another account's.
func (s *Syncer) tokenFor(account string) (string, error) {
	token := s.bearer()
	if err := s.checkToken(token, account); err != nil {
		return "", err
	}
	return token, nil
}

// refreshBound renews the token only for the account that started the
// workflow. If the settings now hold another account, its credentials are
// not used for the old account's data (a retry would publish a snapshot into
// the wrong account).
func (s *Syncer) refreshBound(ctx context.Context, account string) (bool, error) {
	if s.accountChanged(account) {
		return false, ErrSessionChanged
	}
	ok, err := s.session.Refresh(ctx)
	if err != nil || !ok {
		return ok, err
	}
	if s.accountChanged(account) {
		return false, ErrSessionChanged
	}
	return true, nil
}

func isAuthError(err error) bool { return syncclient.IsStatus(err, 401) }

// isSubscriptionExpired is IsSubscriptionExpired: a 403 whose body says so.
func isSubscriptionExpired(err error) bool {
	var ae *syncclient.APIError
	if !errors.As(err, &ae) || ae.StatusCode != 403 {
		return false
	}
	body := strings.ToLower(ae.Body)
	return strings.Contains(body, "订阅已过期") || (strings.Contains(body, "subscription") && strings.Contains(body, "expired"))
}

// refreshAndRetry is the pattern of the C# calls: when the server refuses the
// token, renew it once and try again. A subscription that has expired is its
// own error.
func (s *Syncer) refreshAndRetry(ctx context.Context, token, account string, call func(token string) error) (string, error) {
	if strings.TrimSpace(account) == "" {
		account = syncclient.AccountID(token)
	}
	if err := s.checkToken(token, account); err != nil {
		return token, err
	}
	if s.accountChanged(account) {
		return token, ErrSessionChanged
	}
	err := call(token)
	if err == nil {
		return token, nil
	}
	if isSubscriptionExpired(err) {
		return token, ErrSubscriptionExpired
	}
	if !isAuthError(err) {
		return token, err
	}
	ok, rerr := s.refreshBound(ctx, account)
	if rerr != nil {
		return token, rerr
	}
	if !ok {
		return token, err
	}
	if token, err = s.tokenFor(account); err != nil {
		return token, err
	}
	return token, call(token)
}

// ---- manifest

// ManifestJSON fetches the server's manifest text; "" and a 404 error mean
// that the server has no snapshot yet.
func (s *Syncer) ManifestJSON(ctx context.Context) (string, error) {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.manifestJSON(ctx)
}

func (s *Syncer) manifestJSON(ctx context.Context) (string, error) {
	out, _, err := s.manifestJSONOf(ctx)
	return out, err
}

// manifestJSONOf is manifestJSON with the account of the token the manifest
// was asked with ("" for an opaque token): the manifest is that account's,
// whoever is signed in by the time it arrives.
func (s *Syncer) manifestJSONOf(ctx context.Context) (string, string, error) {
	started := s.cfg.Now()
	token, err := s.connection(ctx)
	if err != nil {
		return "", "", err
	}
	var out, account string
	_, err = s.refreshAndRetry(ctx, token, syncclient.AccountID(token), func(t string) error {
		var e error
		out, e = s.cfg.Client.GetManifestJSON(ctx, t)
		account = syncclient.AccountID(t)
		return e
	})
	if err == nil {
		s.timing("manifest", "fetch", started, "ok")
	}
	return out, account, err
}

// ManifestInfo fetches and parses the manifest. A server without a snapshot
// answers 404, which is returned as the *syncclient.APIError it is, as the C#
// client does ("云端还没有快照").
func (s *Syncer) ManifestInfo(ctx context.Context) (ManifestInfo, error) {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.manifestInfoLocked(ctx)
}

// manifestInfoLocked is ManifestInfo for a caller that already holds the
// gate (upload checks whether the remote snapshot is still there).
func (s *Syncer) manifestInfoLocked(ctx context.Context) (ManifestInfo, error) {
	text, err := s.manifestJSON(ctx)
	if err != nil {
		return ManifestInfo{}, err
	}
	return ParseManifestStrict(text)
}

// remoteManifest is ManifestInfo for a sync that has to decide: a server
// without a snapshot is an empty manifest, which the decision turns into an
// upload ("remote_missing"). The C# client fails here with a 404 instead, so
// a new account's first sync never uploads by itself; that is the one
// difference of Run from RunAutoSyncAsync.
//
// It also returns the account the manifest is of (manifestJSONOf).
func (s *Syncer) remoteManifest(ctx context.Context) (ManifestInfo, string, error) {
	s.gate.Lock()
	text, account, err := s.manifestJSONOf(ctx)
	s.gate.Unlock()
	if syncclient.IsStatus(err, 404) {
		return ManifestInfo{}, account, nil
	}
	if err != nil {
		return ManifestInfo{}, account, err
	}
	m, err := ParseManifestStrict(text)
	return m, account, err
}

// ---- state

// Fingerprint is the fingerprint of the history now.
func (s *Syncer) Fingerprint() string { return Fingerprint(s.cfg.History.Items()) }

// Decide says what a sync has to do given the server's manifest.
func (s *Syncer) Decide(manifest ManifestInfo) Decision {
	return s.DecideItems(manifest, s.cfg.History.Items())
}

// DecideItems decides on the items the caller captured, so the fingerprint
// and the merge baseline describe the same state.
func (s *Syncer) DecideItems(manifest ManifestInfo, items []model.Item) Decision {
	var latest *time.Time
	for _, it := range items {
		if latest == nil || it.CapturedAt.After(*latest) {
			t := it.CapturedAt.Time
			latest = &t
		}
	}
	return Decide(manifest, Fingerprint(items), len(items), latest, s.settings())
}

// PersistSyncState remembers that this history and that snapshot were the
// same at this moment.
//
// Its error says the state was not saved: a record that waits for it (the
// pending merge) must then be kept.
//
// A state that is not written because another session is signed in by then
// is ErrSessionChanged, not a success: the record it would have replaced is
// still needed.
func (s *Syncer) PersistSyncState(localFingerprint string, m ManifestInfo, account string) error {
	if s.holdsState() {
		return errors.New("sync: deletion repair is not durable")
	}
	skipped := false
	err := s.cfg.Settings.Update(func(st *model.Settings) {
		if account != "" {
			// Check the account inside the same update that writes the state:
			// a check before the callback cannot stop a sign-in or sign-out
			// in between.
			cur := syncclient.AccountID(model.Str(st.AuthAccessToken))
			if cur == "" || !strings.EqualFold(cur, account) {
				skipped = true
				return
			}
		}
		st.SyncLastLocalFingerprint = nonEmpty(localFingerprint)
		st.SyncLastRemoteVersion = nonEmpty(m.Version)
		st.SyncLastRemoteUpdatedAt = nil
		if m.UpdatedAt != nil {
			v := model.FormatO(*m.UpdatedAt)
			st.SyncLastRemoteUpdatedAt = &v
		}
		st.SyncLastRemoteSha256 = nonEmpty(m.SnapshotSHA256)
	})
	if skipped {
		return ErrSessionChanged
	}
	return err
}

// persistPendingMerge records the state before a merge, so that the upload
// that follows is seen as a local change.
//
// The record of the merge (pending-merge.json) goes only once the state is
// saved; until then it is what a restart recovers from.
func (s *Syncer) persistPendingMerge(preMergeFingerprint string, basis ManifestInfo, account string) error {
	if err := s.PersistSyncState(pendingUploadFingerprint, basis, account); err != nil {
		return err
	}
	s.clearPendingMerge()
	return nil
}

func basisUpdatedAt(m ManifestInfo) string {
	if m.UpdatedAt != nil {
		return model.FormatO(*m.UpdatedAt)
	}
	return ""
}

// pendingUploadFingerprint is outside the hexadecimal fingerprint namespace, so
// reverting to any earlier history still requires uploading the local change.
const pendingUploadFingerprint = "pending-upload"

// pendingMergeState is a merge that was applied but whose pending upload state
// was not recorded yet. It is written before the mutation is seen (under the
// history's lock), so that no save of the history, however it is triggered,
// can come before it: a crash can resume the upload instead of merging the
// same items again.
type pendingMergeState struct {
	OldPortable       *model.Settings `json:"oldPortable,omitempty"`
	InstalledPortable *model.Settings `json:"installedPortable,omitempty"`
	Mutation          string          `json:"mutation,omitempty"`
	Baseline          *syncBaseline   `json:"baseline,omitempty"`
	Fingerprint       string          `json:"fingerprint"`
	Version           string          `json:"version,omitempty"`
	UpdatedAt         string          `json:"updatedAt,omitempty"`
	SHA256            string          `json:"sha256,omitempty"`
	Size              *int64          `json:"size,omitempty"`
	Before            string          `json:"before"`
	After             string          `json:"after,omitempty"`
	// Applied is the history as the mutation made it, before the limits
	// (After is after them): the record is written before the mutation is
	// seen, and a save of the history can come between the two.
	Applied   string `json:"applied,omitempty"`
	Account   string `json:"account,omitempty"`
	Committed bool   `json:"committed,omitempty"`
	// Added tells a saved mutation apart when a capture was saved with it
	// (so the history is neither After nor Applied): the ids it brought from
	// the server, which nothing here makes (markOf). A mutation that brought
	// none leaves no such proof: only the exact fingerprints, or Committed,
	// tell it then. A count or a field it changed is no proof, as the user
	// can make the same change.
	Added []string `json:"added,omitempty"`
}

// maxMarks bounds the ids a record keeps: a few are proof enough.
const maxMarks = 32

// markOf is the ids a mutation from before to applied brought (Added).
func markOf(before, applied []model.Item) []string {
	was := make(map[string]bool, len(before))
	for _, it := range before {
		was[strings.ToLower(it.ID)] = true
	}
	var added []string
	for _, it := range applied {
		if !was[strings.ToLower(it.ID)] && len(added) < maxMarks {
			added = append(added, it.ID)
		}
	}
	return added
}

// markSeen says that items hold an item p brought.
func (p pendingMergeState) markSeen(items []model.Item) bool {
	if p.Mutation != "" {
		for _, it := range items {
			if it.LocalSyncMutation == p.Mutation {
				return true
			}
		}
		return false
	}
	if len(p.Added) == 0 {
		return false
	}
	have := make(map[string]bool, len(items))
	for _, it := range items {
		have[strings.ToLower(it.ID)] = true
	}
	for _, id := range p.Added {
		if have[strings.ToLower(id)] {
			return true
		}
	}
	return false
}

func (s *Syncer) pendingMergePath() string {
	return filepath.Join(s.cfg.Layout.Root, "sync", "pending-merge.json")
}

func (s *Syncer) writePendingMerge(p pendingMergeState) error {
	if old, ok := s.readPendingMerge(); ok && old.Mutation == p.Mutation && p.OldPortable == nil {
		p.OldPortable, p.InstalledPortable = old.OldPortable, old.InstalledPortable
	}
	if p.Baseline == nil {
		b := syncBaselineOf(s.settings())
		p.Baseline = &b
	}
	dir := filepath.Dir(s.pendingMergePath())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "pending-merge.json.tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := durable.Rename(tmp, s.pendingMergePath()); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Syncer) clearPendingMerge() { _ = os.Remove(s.pendingMergePath()) }

// markPendingMergeCommitted records that Flush returned successfully: from
// then on the merged history is on disk even if captures change it later.
//
// A marker that cannot be written does not stop the workflow: the upload
// that follows publishes the merged history and retires the record
// (persistAfterUpload), which is what the marker is for; stopping here would
// leave the record to a recovery that a later capture can defeat.
func (s *Syncer) markPendingMergeCommitted() {
	p, ok := s.readPendingMerge()
	if !ok {
		return
	}
	p.Committed = true
	_ = s.writePendingMerge(p)
}

func (s *Syncer) readPendingMerge() (pendingMergeState, bool) {
	b, err := os.ReadFile(s.pendingMergePath())
	if err != nil {
		return pendingMergeState{}, false
	}
	var p pendingMergeState
	if json.Unmarshal(b, &p) != nil {
		return pendingMergeState{}, false
	}
	return p, true
}

// recoverPendingMerge finishes the state of a merge whose history was saved
// before the pending state: if the merged items are in the history, record the
// pending upload (so the next decision uploads instead of merging again).
//
// The record goes once its state is saved; when the settings cannot be saved
// it stays, and the error stops the run: deciding on the old state would
// merge the same items again.
func (s *Syncer) recoverPendingMerge(ctx context.Context, account string) error {
	if s.rollbackPending {
		if err := s.cfg.Settings.Update(func(*model.Settings) {}); err != nil {
			return err
		}
		if err := s.flush(ctx); err != nil {
			return err
		}
		s.rollbackPending = false
		s.clearPendingMerge()
		return nil
	}
	p, ok := s.readPendingMerge()
	if !ok {
		return nil
	}
	if p.Account == "" {
		// An opaque token did not say whose the record is: it is the
		// workflow's, not whoever signs in later.
		p.Account = account
	}
	if p.Account != "" && s.accountChanged(p.Account) {
		s.clearPendingMerge()
		return nil
	}
	if p.Baseline != nil {
		current := syncBaselineOf(s.settings())
		installed := syncBaseline{p.Fingerprint, p.Version, p.UpdatedAt, p.SHA256}
		if current != *p.Baseline && current != installed {
			// Update can change memory even when its disk write fails. Flush
			// that checkpoint again before retiring its recovery evidence.
			if err := s.cfg.Settings.Update(func(*model.Settings) {}); err != nil {
				return err
			}
			s.clearPendingMerge()
			return nil
		}
	}
	items := s.cfg.History.Items()
	current := Fingerprint(items)
	if !p.Committed && (p.After == "" || current != p.After) && (p.Applied == "" || current != p.Applied) && !p.markSeen(items) && (p.Mutation == "" || history.SavedMutation(s.cfg.Layout) != p.Mutation) {
		// Only the marker written after a successful Flush, the exact
		// merged history, or the mark of the merge in the history proves the
		// merge reached the disk. A different fingerprint alone can be an
		// unflushed capture. Restore only portable fields still equal to the import.
		if p.OldPortable != nil && p.InstalledPortable != nil {
			if err := s.cfg.Settings.Update(func(st *model.Settings) {
				if p.Account != "" && !strings.EqualFold(syncclient.AccountID(model.Str(st.AuthAccessToken)), p.Account) {
					return
				}
				st.RestorePortable(*p.InstalledPortable, *p.OldPortable)
			}); err != nil {
				return err
			}
		}
		// Persist the current rollback before discarding evidence.
		if err := s.flush(ctx); err != nil {
			return err
		}
		s.clearPendingMerge()
		return nil
	}
	if p.OldPortable != nil && p.InstalledPortable != nil {
		if err := s.cfg.Settings.Update(func(st *model.Settings) {
			if p.Account != "" && !strings.EqualFold(syncclient.AccountID(model.Str(st.AuthAccessToken)), p.Account) {
				return
			}
			st.RestorePortable(*p.OldPortable, *p.InstalledPortable)
		}); err != nil {
			return err
		}
	}
	m := ManifestInfo{Version: p.Version, SnapshotSHA256: p.SHA256, SnapshotSize: p.Size}
	if p.UpdatedAt != "" {
		if at, err := model.ParseTime(p.UpdatedAt); err == nil {
			m.UpdatedAt = &at
		}
	}
	if err := s.flush(ctx); err != nil {
		return err
	}
	if err := s.PersistSyncState(p.Fingerprint, m, p.Account); err != nil {
		return fmt.Errorf("recover the pending merge: %w", err)
	}
	s.clearPendingMerge()
	_ = s.bindFor(p.Account)
	return nil
}

func nonEmpty(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return &v
}

// ---- upload

// Upload exports the history and sends it, with the blobs the server lacks.
// It sends nothing when the snapshot is the one the server already has.
func (s *Syncer) Upload(ctx context.Context, progress func(Progress), reason string) (*syncclient.UploadResult, error) {
	s.beginWorkflow()
	defer s.workflow.Unlock()
	account := s.currentAccount()
	if err := s.checkBound(account); err != nil {
		return nil, err
	}
	if err := s.bindFor(account); err != nil {
		return nil, err
	}
	up, _, err := s.uploadWithFingerprint(ctx, progress, reason, account)
	if err != nil {
		return nil, err
	}
	_ = s.bindAccount(account)
	return up, nil
}

// uploadWithFingerprint is Upload, and also says which history went into the
// snapshot: a change made while the upload ran must not be remembered as
// uploaded.
func (s *Syncer) uploadWithFingerprint(ctx context.Context, progress func(Progress), reason, account string) (*syncclient.UploadResult, string, error) {
	s.gate.Lock()
	defer s.gate.Unlock()
	return s.upload(ctx, progress, reason, account)
}

func (s *Syncer) upload(ctx context.Context, progress func(Progress), reason, account string) (res *syncclient.UploadResult, fp string, err error) {
	started := s.cfg.Now()
	report(progress, "prepare_local", "准备本地数据", 30)
	token, err := s.connection(ctx)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(account) == "" {
		account = syncclient.AccountID(token)
	}
	if err := s.checkBound(account); err != nil {
		return nil, "", err
	}
	if err := s.bindFor(account); err != nil {
		return nil, "", err
	}
	if err := s.checkToken(token, account); err != nil {
		return nil, "", err
	}
	if s.accountChanged(account) {
		return nil, "", ErrSessionChanged
	}

	path, err := s.tempSnapshotPath()
	if err != nil {
		return nil, "", err
	}
	defer tryDelete(path)

	items := s.cfg.History.Items()
	defer func() {
		if err == nil && !s.accountChanged(account) {
			s.repaired()
			if s.cfg.Uploaded != nil {
				s.cfg.Uploaded(items)
			}
		}
	}()

	fp = Fingerprint(items)
	// The history is read under the account that started the upload: a sign-in
	// while it is being read must not ship the new account's items with this
	// one's token.
	if err := s.ensureAccount(token, account); err != nil {
		return nil, fp, err
	}
	st := s.settings()
	s.timing("upload", "snapshot_source_ready", started, fmt.Sprintf("%s;items=%d", reason, len(items)))

	report(progress, "export_snapshot", "导出快照", 52)
	exp, err := snapshot.Export(ctx, s.cfg.Layout, path, items, st)
	if err != nil {
		return nil, fp, err
	}
	s.timing("upload", "export_snapshot", started, fmt.Sprintf("%s;size=%d;sha256=%s;blobs=%d", reason, exp.Size, exp.SHA256, len(exp.Blobs)))

	if last := strings.TrimSpace(model.Str(st.SyncLastRemoteSha256)); last != "" && strings.EqualFold(last, exp.SHA256) {
		// The shortcut is safe only while the server still has that snapshot:
		// after a storage reset or a deletion the same export has to be
		// uploaded again.
		if m, err := s.manifestInfoLocked(ctx); err == nil &&
			strings.EqualFold(strings.TrimSpace(m.SnapshotSHA256), exp.SHA256) {
			s.timing("upload", "skip_upload_same_sha", started, reason)
			size := exp.Size
			return &syncclient.UploadResult{Version: strings.TrimSpace(m.Version), SHA256: exp.SHA256, Size: &size}, fp, nil
		}
	}

	if err := s.ensureAccount(token, account); err != nil {
		return nil, fp, err
	}
	if len(exp.Blobs) > 0 {
		report(progress, "upload_blobs", "上传二进制对象", 68)
		if token, err = s.uploadMissingBlobsWithRefresh(ctx, token, exp, started, reason, account); err != nil {
			return nil, fp, err
		}
	}

	if err := s.flush(ctx); err != nil {
		return nil, fp, err
	}
	if err := s.writeUploadIntent(uploadIntent{Account: account, Fingerprint: fp, SHA: exp.SHA256}); err != nil {
		return nil, fp, err
	}
	report(progress, "upload_snapshot", "上传快照", 84)
	call := func(t string) error {
		var e error
		res, e = s.cfg.Client.UploadSnapshot(ctx, t, path)
		return e
	}
	if err := s.ensureAccount(token, account); err != nil {
		return nil, fp, err
	}
	err = call(token)
	if err != nil {
		if isSubscriptionExpired(err) {
			return nil, fp, ErrSubscriptionExpired
		}
		if !isAuthError(err) {
			return nil, fp, err
		}
		ok, rerr := s.refreshBound(ctx, account)
		if rerr != nil {
			return nil, fp, rerr
		}
		if !ok {
			return nil, fp, err
		}
		if token, err = s.tokenFor(account); err != nil {
			return nil, fp, err
		}
		if len(exp.Blobs) > 0 {
			if token, err = s.uploadMissingBlobsWithRefresh(ctx, token, exp, started, reason+";after_refresh", account); err != nil {
				return nil, fp, err
			}
		}
		if err = call(token); err != nil {
			return nil, fp, err
		}
		s.timing("upload", "upload_snapshot_after_refresh", started, reason)
		return res, fp, nil
	}
	s.timing("upload", "upload_snapshot", started, reason)
	return res, fp, nil
}

func (s *Syncer) uploadMissingBlobsWithRefresh(ctx context.Context, token string, exp snapshot.ExportResult, started time.Time, reason, account string) (string, error) {
	if err := s.ensureAccount(token, account); err != nil {
		return token, err
	}
	err := s.uploadMissingBlobs(ctx, token, exp, started, reason, account)
	if err == nil || !isAuthError(err) {
		return token, err
	}
	ok, rerr := s.refreshBound(ctx, account)
	if rerr != nil {
		return token, rerr
	}
	if !ok {
		return token, err
	}
	if token, err = s.tokenFor(account); err != nil {
		return token, err
	}
	return token, s.uploadMissingBlobs(ctx, token, exp, started, reason+";blob_refresh", account)
}

func (s *Syncer) uploadMissingBlobs(ctx context.Context, token string, exp snapshot.ExportResult, started time.Time, reason, account string) error {
	seen := map[string]bool{}
	var unique []snapshot.Blob
	for _, b := range exp.Blobs {
		k := strings.ToLower(b.SHA256)
		if !seen[k] {
			seen[k] = true
			unique = append(unique, b)
		}
	}
	if len(unique) == 0 {
		return nil
	}
	hashes := make([]string, len(unique))
	for i, b := range unique {
		hashes[i] = b.SHA256
	}
	missingList, err := s.cfg.Client.CheckMissingBlobs(ctx, token, hashes)
	if err != nil {
		return err
	}
	missing := map[string]bool{}
	for _, h := range missingList {
		missing[strings.ToLower(h)] = true
	}
	s.timing("upload", "check_missing_blobs", started, fmt.Sprintf("%s;total=%d;missing=%d", reason, len(unique), len(missing)))
	for _, b := range unique {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !missing[strings.ToLower(b.SHA256)] {
			continue
		}
		if err := s.ensureAccount(token, account); err != nil {
			return err
		}
		if err := s.cfg.Client.UploadBlob(ctx, token, b.SHA256, b.SourcePath); err != nil {
			return err
		}
	}
	s.timing("upload", "upload_blobs", started, fmt.Sprintf("%s;uploaded=%d", reason, len(missing)))
	return nil
}

// ---- download

// DownloadResult says which snapshot a download applied, and with which
// history: the caller remembers that, not the state of the history later,
// which may already hold a capture that was never uploaded.
type DownloadResult struct {
	// Fingerprint is of the items the download applied.
	Fingerprint string
	// SnapshotSHA256 is of the snapshot file, as the server hashes it.
	SnapshotSHA256 string
	// Merged says the history changed while the snapshot was downloaded and
	// the new items were merged in: the result has to be uploaded.
	Merged bool
	// Account is the account the download started in, so that a later step
	// (PersistAfterDownload) can tell whether the session changed.
	Account string
	// PreMergeFingerprint is the history before a download that had to merge
	// a concurrent capture: it is the baseline of the pending upload.
	PreMergeFingerprint string
	// gen is the workflow the download ran in (rootLocks.gen).
	gen uint64
}

// DownloadAndApply replaces the history and the portable settings with the
// server's snapshot, unless the history changed since expectFingerprint, in
// which case the new captures are merged instead of being lost.
//
// Its state is recorded by PersistAfterDownload, which refuses when another
// sync ran in between; Restore does both in one workflow.
func (s *Syncer) DownloadAndApply(ctx context.Context, progress func(Progress), reason, expectFingerprint string, baseline []model.Item) (DownloadResult, error) {
	s.beginWorkflow()
	defer s.workflow.Unlock()
	if err := s.recoverPendingMerge(ctx, s.currentAccount()); err != nil {
		return DownloadResult{}, err
	}
	return s.downloadAndApplyWorkflow(ctx, progress, reason, expectFingerprint, baseline)
}

// downloadAndApplyWorkflow is DownloadAndApply in a workflow already begun.
func (s *Syncer) downloadAndApplyWorkflow(ctx context.Context, progress func(Progress), reason, expectFingerprint string, baseline []model.Item) (DownloadResult, error) {
	account := s.currentAccount()
	if err := s.checkBound(account); err != nil {
		return DownloadResult{}, err
	}
	if err := s.bindFor(account); err != nil {
		return DownloadResult{}, err
	}
	res, err := s.downloadAndApply(ctx, progress, reason, expectFingerprint, baseline, account)
	if err != nil {
		return DownloadResult{}, err
	}
	_ = s.bindAccount(account)
	res.gen = s.gen
	return res, nil
}

func (s *Syncer) downloadAndApply(ctx context.Context, progress func(Progress), reason, expectFingerprint string, baseline []model.Item, account string) (DownloadResult, error) {
	if s.cfg.Flush == nil {
		return DownloadResult{}, ErrNoFlush
	}
	s.gate.Lock()
	defer s.gate.Unlock()
	started := s.cfg.Now()
	report(progress, "prepare_local", "准备本地数据", 30)
	s.recoverPayloadDirs()
	token, err := s.connection(ctx)
	if err != nil {
		return DownloadResult{}, err
	}
	if strings.TrimSpace(account) == "" {
		account = syncclient.AccountID(token)
	}
	if err := s.checkBound(account); err != nil {
		return DownloadResult{}, err
	}
	if err := s.bindFor(account); err != nil {
		return DownloadResult{}, err
	}
	if err := s.checkToken(token, account); err != nil {
		return DownloadResult{}, err
	}
	if s.accountChanged(account) {
		return DownloadResult{}, ErrSessionChanged
	}
	path, err := s.tempSnapshotPath()
	if err != nil {
		return DownloadResult{}, err
	}
	defer tryDelete(path)

	report(progress, "download_snapshot", "下载快照", 52)
	if token, err = s.downloadSnapshot(ctx, token, path, account); err != nil {
		return DownloadResult{}, err
	}
	s.timing("download", "download_snapshot", started, fmt.Sprintf("%s;size=%d", reason, fileSize(path)))

	sum, err := snapshot.FileSHA256(path)
	if err != nil {
		return DownloadResult{}, err
	}

	report(progress, "import_snapshot", "导入快照", 76)
	// The payloads are kept apart from the data folders until the history is
	// replaced: a failed replacement must leave the old history's files.
	importRoot := filepath.Join(s.cfg.Layout.Root, ".download-"+guidN())
	if err := os.MkdirAll(importRoot, 0o700); err != nil {
		return DownloadResult{}, err
	}
	defer os.RemoveAll(importRoot)
	data, err := snapshot.ReadDetached(ctx, s.cfg.Layout, importRoot, path, s.ensureBlob(ctx, &token, account))
	if err != nil {
		return DownloadResult{}, err
	}

	if s.accountChanged(account) {
		return DownloadResult{}, ErrSessionChanged
	}
	oldSettings := s.settings()
	undo := &settingsUndo{old: oldSettings, account: account, identities: data.Items}
	var before, applied []model.Item
	var moves *snapshot.AppliedPayloads
	merged := false
	preMerge, appliedFP := "", ""
	mutation := guidN()
	var added []string
	err = s.mutateHistoryWithMarker(ctx, mutation, func(current []model.Item) ([]model.Item, error) {
		if s.accountChanged(account) {
			return nil, ErrSessionChanged
		}
		before = append([]model.Item(nil), current...)
		next := s.withoutDeleted(data.Items)
		if expectFingerprint != "" && Fingerprint(current) != expectFingerprint {
			// A capture arrived while the snapshot was downloading: keep it
			// and let the upload that follows publish both sides.
			preMerge = Fingerprint(current)
			next = MergeForDownload(data.Items, current, baseline)
			merged = true
		}
		out, ap, err := snapshot.ApplyPayloads(s.cfg.Layout, importRoot, next, before)
		if err != nil {
			return nil, err
		}
		if s.accountChanged(account) {
			// The session changed while the payloads were moving: undo them
			// before the history commit, so the new session cannot get this
			// account's items.
			ap.Rollback()
			return nil, ErrSessionChanged
		}
		// The fingerprint of what this mutation applied, taken while the
		// history is locked: a later deletion is a local change, not part of
		// the downloaded baseline.
		for i := range out {
			out[i].LocalSyncMutation = mutation
		}
		appliedFP = Fingerprint(out)
		added = markOf(before, out)
		fingerprint := appliedFP
		if merged {
			fingerprint = pendingUploadFingerprint
		}
		oldPortable := oldSettings.Portable()
		intended := oldPortable
		if data.Settings != nil {
			intended = data.Settings.Portable()
		}
		if merged {
			EnsureSettingsCanHoldItems(&intended, out)
		}
		if err := s.writePendingMerge(pendingMergeState{
			OldPortable: &oldPortable, InstalledPortable: &intended,
			Mutation:    mutation,
			Fingerprint: fingerprint,
			Version:     sum,
			SHA256:      sum,
			Before:      Fingerprint(before),
			After:       appliedFP,
			Applied:     appliedFP,
			Account:     account,
			Added:       added,
		}); err != nil {
			ap.Rollback()
			return nil, err
		}
		moves, applied = ap, cloneItems(out)
		return out, nil
	})
	if err != nil {
		s.clearPendingMerge() // nothing was applied
		if moves != nil {
			moves.Rollback()
		}
		return DownloadResult{}, err
	}
	mutatedFP := appliedFP
	if s.accountChanged(account) {
		return DownloadResult{}, s.abandonForSession(ctx, applied, moves, undo)
	}
	// The settings change only after the history was replaced: a failure
	// above leaves both as they were. A snapshot without settings keeps the
	// local ones, and a download-time merge still has to fit.
	settings := oldSettings.Portable()
	if data.Settings != nil {
		settings = data.Settings.Portable()
	}
	if merged {
		// The merge added items to a history that may be at its limits: do
		// not let ApplyLimits prune them before the upload.
		EnsureSettingsCanHoldItems(&settings, applied)
	}
	// The limits the history now needs must be saved with it: settings left
	// with the old ones would prune it at the next start.
	limitPruned, serr := s.applySettingsFor(account, settings, undo)
	if errors.Is(serr, ErrSessionChanged) {
		return DownloadResult{}, s.abandonForSession(ctx, applied, moves, undo)
	}
	res := DownloadResult{
		Fingerprint:         appliedFP,
		SnapshotSHA256:      sum,
		Merged:              merged,
		Account:             account,
		PreMergeFingerprint: preMerge,
	}
	// Write the pending state before the history is saved: a crash after the
	// save but before Run records it must resume the upload (merged) or keep
	// the downloaded baseline (ordinary download).
	appliedFP = Fingerprint(withoutIDs(applied, limitPruned))
	res.Fingerprint = appliedFP
	after := appliedFP
	fingerprint := appliedFP
	if merged {
		fingerprint = pendingUploadFingerprint
	}
	werr := serr
	if werr == nil {
		werr = s.writePendingMerge(pendingMergeState{
			Mutation:    mutation,
			Fingerprint: fingerprint,
			Version:     sum,
			SHA256:      sum,
			Before:      Fingerprint(before),
			After:       after,
			Applied:     mutatedFP,
			Account:     account,
			Added:       added,
		})
	}
	if werr != nil {
		rctx, rcancel := cleanupContext()
		rerr := s.rollbackMutation(rctx, before, applied, undo, limitPruned)
		if rerr == nil {
			rerr = s.flush(rctx)
		}
		rcancel()
		if rerr == nil && moves != nil {
			moves.RollbackKeeping(s.payloadKeeper())
		}
		if rerr != nil {
			// The record stays: the applied history may be on disk (an
			// autosave), and it is what a restart recovers from.
			return DownloadResult{}, fmt.Errorf("%w (history rollback failed: %v)", werr, rerr)
		}
		s.clearPendingMerge() // the history is back as it was, and saved
		return DownloadResult{}, werr
	}
	// What the limits pruned is unreferenced too, including payloads this
	// import just installed.
	cleanup := append(append([]model.Item(nil), before...), applied...)
	removed := s.unreferencedFiles(cleanup, s.cfg.History.Items())
	s.timing("download", "import_snapshot", started, reason)

	report(progress, "persist_local", "保存本地状态", 90)
	if err := s.flush(ctx); err != nil {
		// The record is kept until the undo is saved: an autosave may have
		// saved the applied history already.
		// The history was not saved: put the old one, its settings and its
		// files back. Captures made meanwhile are kept.
		if s.accountChanged(account) {
			if derr := s.discardMutation(ctx, applied, moves, undo); derr != nil {
				return DownloadResult{}, fmt.Errorf("%w (session cleanup failed: %v)", err, derr)
			}
			if derr := s.persistCleanup(); derr != nil {
				return DownloadResult{}, fmt.Errorf("%w (session cleanup not saved: %v)", err, derr)
			}
			s.clearPendingMerge()
			if moves != nil {
				moves.Rollback()
			}
			return DownloadResult{}, ErrSessionChanged
		}
		rctx, rcancel := cleanupContext()
		rerr := s.rollbackMutation(rctx, before, applied, undo, limitPruned)
		if rerr == nil {
			// Save the restored history before deleting the payloads it may
			// still be referenced by (an autosave might have saved it).
			rerr = s.flush(rctx)
		}
		rcancel()
		if rerr == nil && moves != nil {
			moves.RollbackKeeping(s.payloadKeeper())
		}
		if rerr != nil {
			return DownloadResult{}, fmt.Errorf("%w (history rollback failed: %v)", err, rerr)
		}
		s.clearPendingMerge() // the history is back as it was, and saved
		return DownloadResult{}, err
	}
	s.markPendingMergeCommitted()
	if moves != nil {
		moves.MarkCommitted()
	}
	if s.accountChanged(account) {
		// The session changed while the new history was being saved: take
		// this account's items out again and save that, so the new session
		// cannot publish them later.
		return DownloadResult{}, s.abandonForSession(ctx, applied, moves, undo)
	}
	if moves != nil {
		moves.Commit()
	}
	_ = s.cfg.History.RemoveFiles(removed)
	s.timing("download", "persist_local", started, reason)
	return res, nil
}

func (s *Syncer) downloadSnapshot(ctx context.Context, token, path, account string) (string, error) {
	return s.refreshAndRetry(ctx, token, account, func(t string) error {
		return s.cfg.Client.DownloadSnapshot(ctx, t, path)
	})
}

// ensureBlob returns the function snapshot.Read calls for a blob that is not
// stored locally. It reads *token each time, as the C# closure does, so a
// renewed token is used. A blob that cannot be fetched is "not there": the
// import then fails with "blob missing".
func (s *Syncer) ensureBlob(ctx context.Context, token *string, account string) snapshot.EnsureBlob {
	if strings.TrimSpace(account) == "" {
		account = syncclient.AccountID(*token)
	}
	return func(sha string) (string, error) {
		local := s.cfg.Layout.BlobPath(sha)
		if local == "" {
			return "", nil
		}
		if isFile(local) {
			return local, nil
		}
		err := s.cfg.Client.DownloadBlob(ctx, *token, sha, local)
		if err != nil && isAuthError(err) {
			ok, rerr := s.refreshBound(ctx, account)
			if rerr != nil {
				return "", rerr
			}
			if !ok {
				return "", nil
			}
			tok, terr := s.tokenFor(account)
			if terr != nil {
				return "", terr
			}
			*token = tok
			err = s.cfg.Client.DownloadBlob(ctx, *token, sha, local)
		}
		if err != nil || !isFile(local) {
			return "", nil
		}
		return local, nil
	}
}

// ---- merge

// MergeAndApply merges the server's snapshot into the local history: it
// backs the local history up first, then keeps the items of both sides.
// basis is the manifest the decision was made on and preMergeFingerprint the
// fingerprint of the history before the merge; they are remembered so that
// the upload that follows counts as a local change.
func (s *Syncer) MergeAndApply(ctx context.Context, basis ManifestInfo, preMergeFingerprint string, progress func(Progress), reason string) error {
	s.beginWorkflow()
	defer s.workflow.Unlock()
	account := s.currentAccount()
	if err := s.checkBound(account); err != nil {
		return err
	}
	if err := s.bindFor(account); err != nil {
		return err
	}
	if err := s.recoverPendingMerge(ctx, account); err != nil {
		return err
	}
	if err := s.mergeAndApply(ctx, basis, preMergeFingerprint, progress, reason, account); err != nil {
		return err
	}
	_ = s.bindAccount(account)
	return nil
}

func (s *Syncer) mergeAndApply(ctx context.Context, basis ManifestInfo, preMergeFingerprint string, progress func(Progress), reason, account string) error {
	if s.cfg.Flush == nil {
		return ErrNoFlush
	}
	s.gate.Lock()
	defer s.gate.Unlock()
	started := s.cfg.Now()
	report(progress, "prepare_local", "prepare local", 18)
	s.recoverPayloadDirs()
	token, err := s.connection(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(account) == "" {
		account = syncclient.AccountID(token)
	}
	if err := s.checkBound(account); err != nil {
		return err
	}
	if err := s.bindFor(account); err != nil {
		return err
	}
	if err := s.checkToken(token, account); err != nil {
		return err
	}
	if s.accountChanged(account) {
		return ErrSessionChanged
	}
	local := s.cfg.History.Items()
	localSettings := s.settings()
	undo := &settingsUndo{old: localSettings, account: account}

	report(progress, "backup_snapshot", "backup local snapshot", 28)
	backup, err := s.preMergeBackupPath()
	if err != nil {
		return err
	}
	bexp, err := snapshot.Export(ctx, s.cfg.Layout, backup, local, localSettings)
	if err != nil {
		return err
	}
	if err := s.ensureLocalBackupBlobs(ctx, bexp); err != nil {
		// The local history is about to be replaced: a backup that cannot be
		// read back is not a backup, so stop before anything is changed.
		return fmt.Errorf("pre-merge backup: %w", err)
	}
	if err := durable.SyncTree(backup); err != nil {
		return err
	}
	if err := durable.SyncParent(backup); err != nil {
		return err
	}
	s.timing("merge", "backup_snapshot", started, fmt.Sprintf("%s;path=%s;items=%d;size=%d", reason, backup, len(local), bexp.Size))

	path, err := s.tempSnapshotPath()
	if err != nil {
		return err
	}
	defer tryDelete(path)

	report(progress, "download_snapshot", "download remote snapshot", 44)
	if token, err = s.downloadSnapshot(ctx, token, path, account); err != nil {
		return err
	}
	if sum, err := snapshot.FileSHA256(path); err == nil {
		// The snapshot may be newer than the manifest the decision was made
		// on: the pending state must name the file that was actually merged.
		basis = snapshotState(DownloadResult{SnapshotSHA256: sum}, basis)
		if basis.Version == "" {
			basis.Version = sum
		}
	}
	s.timing("merge", "download_snapshot", started, fmt.Sprintf("%s;size=%d", reason, fileSize(path)))

	report(progress, "read_snapshot", "read remote snapshot", 62)
	// The remote payloads go to a folder of their own: the merge may keep the
	// local item of a file-list item, whose files must stay where they are
	// until that is decided.
	importRoot := filepath.Join(s.cfg.Layout.Root, ".merge-"+guidN())
	if err := os.MkdirAll(importRoot, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(importRoot)
	remote, err := snapshot.ReadDetached(ctx, s.cfg.Layout, importRoot, path, s.ensureBlob(ctx, &token, account))
	if err != nil {
		return err
	}
	s.timing("merge", "read_snapshot", started, fmt.Sprintf("%s;remoteItems=%d;images=%d;files=%d", reason, len(remote.Items), remote.ImageCount, remote.FileCount))

	report(progress, "merge_snapshot", "merge snapshots", 78)
	if s.accountChanged(account) {
		return ErrSessionChanged
	}
	var before, merged []model.Item
	var moves *snapshot.AppliedPayloads
	mutation := guidN()
	var added []string
	// The merge runs on the history as it is now, and the payloads it keeps
	// are moved into place under the same lock: a capture made while the
	// download ran is merged, not overwritten.
	err = s.mutateHistoryWithMarker(ctx, mutation, func(current []model.Item) ([]model.Item, error) {
		if s.accountChanged(account) {
			return nil, ErrSessionChanged
		}
		before = append([]model.Item(nil), current...)
		undo.identities = remote.Items
		// What the user deleted while the snapshot was downloading is left out
		// of it, or the merge would bring it back.
		merge := MergeItems
		if !localSettings.MergeDuplicates || (remote.Settings != nil && !remote.Settings.MergeDuplicates) {
			merge = MergeItemsKeepingDuplicates
		}
		merged = merge(s.withoutDeleted(withoutDeletedSince(remote.Items, local, current)), s.withoutDeleted(current))
		if model.Str(localSettings.SyncLastLocalFingerprint) == pendingUploadFingerprint {
			if !localSettings.MergeDuplicates || (remote.Settings != nil && !remote.Settings.MergeDuplicates) {
				counts := map[string]int32{}
				for _, side := range [][]model.Item{remote.Items, current} {
					for _, it := range side {
						key := strings.ToLower(it.ID)
						counts[key] = max(counts[key], it.CopyCount)
					}
				}
				for i := range merged {
					merged[i].CopyCount = max(1, counts[strings.ToLower(merged[i].ID)])
				}

			} else {
				recountMergedSides(merged, s.withoutDeleted(withoutDeletedSince(remote.Items, local, current)), s.withoutDeleted(current))
			}
		}
		applied, ap, err := snapshot.ApplyPayloads(s.cfg.Layout, importRoot, merged, before)
		if err != nil {
			return nil, err
		}
		if s.accountChanged(account) {
			// The session changed while the payloads were moving: undo them
			// before the history commit.
			ap.Rollback()
			return nil, ErrSessionChanged
		}
		for i := range applied {
			applied[i].LocalSyncMutation = mutation
		}
		fp := Fingerprint(applied)
		added = markOf(before, applied)
		oldPortable := localSettings.Portable()
		intended := oldPortable
		if remote.Settings != nil {
			intended = remote.Settings.Portable()
		}
		EnsureSettingsCanHoldItems(&intended, applied)
		if err := s.writePendingMerge(pendingMergeState{
			OldPortable: &oldPortable, InstalledPortable: &intended,
			Mutation:    mutation,
			Fingerprint: pendingUploadFingerprint,
			Version:     basis.Version,
			UpdatedAt:   basisUpdatedAt(basis),
			SHA256:      basis.SnapshotSHA256,
			Size:        basis.SnapshotSize,
			Before:      Fingerprint(before),
			After:       fp,
			Applied:     fp,
			Account:     account,
			Added:       added,
		}); err != nil {
			ap.Rollback()
			return nil, err
		}
		moves, merged = ap, cloneItems(applied)
		return merged, nil
	})
	if err != nil {
		s.clearPendingMerge() // nothing was applied
		if moves != nil {
			moves.Rollback()
		}
		return err
	}
	appliedFP := Fingerprint(merged)
	if s.accountChanged(account) {
		return s.abandonForSession(ctx, merged, moves, undo)
	}
	mergedSettings := localSettings.Portable()
	if remote.Settings != nil {
		mergedSettings = remote.Settings.Portable()
	}
	EnsureSettingsCanHoldItems(&mergedSettings, merged)

	// The limits the merged history needs are saved with it (see the
	// download).
	limitPruned, serr := s.applySettingsFor(account, mergedSettings, undo)
	if errors.Is(serr, ErrSessionChanged) {
		return s.abandonForSession(ctx, merged, moves, undo)
	}
	s.timing("merge", "merge_snapshot", started, fmt.Sprintf("%s;localItems=%d;remoteItems=%d;mergedItems=%d", reason, len(local), len(remote.Items), len(merged)))

	cleanup := append(append([]model.Item(nil), before...), merged...)
	removed := s.unreferencedFiles(cleanup, s.cfg.History.Items())
	var updatedAt string
	if basis.UpdatedAt != nil {
		updatedAt = model.FormatO(*basis.UpdatedAt)
	}
	after := s.Fingerprint()
	werr := serr
	if werr == nil {
		werr = s.writePendingMerge(pendingMergeState{
			Mutation:    mutation,
			Fingerprint: pendingUploadFingerprint,
			Version:     basis.Version,
			UpdatedAt:   updatedAt,
			SHA256:      basis.SnapshotSHA256,
			Size:        basis.SnapshotSize,
			Before:      Fingerprint(before),
			After:       after,
			Applied:     appliedFP,
			Account:     account,
			Added:       added,
		})
	}
	if werr != nil {
		rctx, rcancel := cleanupContext()
		rerr := s.rollbackMutation(rctx, before, merged, undo, limitPruned)
		if rerr == nil {
			rerr = s.flush(rctx)
		}
		rcancel()
		if rerr == nil && moves != nil {
			moves.RollbackKeeping(s.payloadKeeper())
		}
		if rerr != nil {
			// The record stays: the merged history may be on disk (an
			// autosave), and it is what a restart recovers from.
			return fmt.Errorf("%w (history rollback failed: %v)", werr, rerr)
		}
		s.clearPendingMerge() // the history is back as it was, and saved
		return werr
	}
	report(progress, "persist_local", "persist local", 90)
	if err := s.flush(ctx); err != nil {
		// The record is kept until the undo is saved: an autosave may have
		// saved the applied history already.
		// The history was not saved: put the old one, its settings and its
		// files back. Captures made meanwhile are kept.
		if s.accountChanged(account) {
			if derr := s.discardMutation(ctx, merged, moves, undo); derr != nil {
				return fmt.Errorf("%w (session cleanup failed: %v)", err, derr)
			}
			if derr := s.persistCleanup(); derr != nil {
				return fmt.Errorf("%w (session cleanup not saved: %v)", err, derr)
			}
			s.clearPendingMerge()
			if moves != nil {
				moves.Rollback()
			}
			return ErrSessionChanged
		}
		rctx, rcancel := cleanupContext()
		rerr := s.rollbackMutation(rctx, before, merged, undo, limitPruned)
		if rerr == nil {
			rerr = s.flush(rctx)
		}
		rcancel()
		if rerr == nil && moves != nil {
			moves.RollbackKeeping(s.payloadKeeper())
		}
		if rerr != nil {
			return fmt.Errorf("%w (history rollback failed: %v)", err, rerr)
		}
		s.clearPendingMerge() // the history is back as it was, and saved
		return err
	}
	s.markPendingMergeCommitted()
	if moves != nil {
		moves.MarkCommitted()
	}
	if s.accountChanged(account) {
		// The session changed while the merged history was being saved: take
		// this account's items out again (see the download).
		return s.abandonForSession(ctx, merged, moves, undo)
	}
	// The merged history is on disk: record the pending upload before the
	// payload cleanup and the upload that follows. A state that cannot be
	// saved leaves the record, from which the next run recovers, and stops
	// this one before the upload.
	perr := s.persistPendingMerge(preMergeFingerprint, basis, account)
	if moves != nil {
		moves.Commit()
	}
	_ = s.cfg.History.RemoveFiles(removed)
	s.timing("merge", "persist_local", started, reason)
	return perr
}

// ensureLocalBackupBlobs copies the files the backup refers to into the blob
// store, so that the backup can be read back later. It fails when a copy
// fails: the merge must not go on with a backup that refers to nothing.
func (s *Syncer) ensureLocalBackupBlobs(ctx context.Context, exp snapshot.ExportResult) error {
	for _, b := range exp.Blobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := s.cfg.Layout.BlobPath(b.SHA256)
		if target == "" {
			return fmt.Errorf("blob %s: no local path", b.SHA256)
		}
		if isFile(target) {
			// A blob that is there but does not hash to its name is not a
			// backup: repair it from the source, which Export hashed.
			if sum, err := snapshot.FileSHA256(target); err == nil && strings.EqualFold(sum, b.SHA256) {
				if err := durable.SyncTree(target); err != nil {
					return err
				}
				if err := durable.SyncParent(target); err != nil {
					return err
				}
				continue
			}
		}
		if a, _ := filepath.Abs(b.SourcePath); strings.EqualFold(a, mustAbs(target)) {
			return fmt.Errorf("blob %s: the only copy is corrupt", b.SHA256)
		}
		if !isFile(b.SourcePath) {
			return fmt.Errorf("blob %s: the payload is gone", b.SHA256)
		}
		if isFile(target) {
			if err := os.Remove(target); err != nil {
				return fmt.Errorf("remove the corrupt blob %s: %w", b.SHA256, err)
			}
		}
		if err := copyNew(b.SourcePath, target); err != nil {
			return fmt.Errorf("blob %s: %w", b.SHA256, err)
		}
		if sum, err := snapshot.FileSHA256(target); err != nil || !strings.EqualFold(sum, b.SHA256) {
			_ = os.Remove(target)
			return fmt.Errorf("blob %s: the copied bytes do not match", b.SHA256)
		}
	}
	return nil
}

// copyNew copies src to dst, which must not exist yet. A file that cannot be
// written or closed is removed, so that a later backup cannot mistake a
// partial file for a complete blob.
func copyNew(src, dst string) (err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if err == nil {
		err = durable.SyncParent(dst)
	}
	return err
}

// ---- small helpers

func (s *Syncer) suppress(on bool) {
	if s.cfg.SuppressAutoSync != nil {
		s.cfg.SuppressAutoSync(on)
	}
}

// applySettingsFor writes the portable settings a download or a merge brings,
// and applies their limits to the history, for the session of account. The
// account is checked inside the settings update, as PersistSyncState does: a
// session signed in meanwhile gets nothing written (ErrSessionChanged), and
// the limits are not applied to its history. It returns what the limits
// pruned; another error says the settings were not saved.
//
// undo learns what was written.
func (s *Syncer) applySettingsFor(account string, settings model.Settings, undo *settingsUndo) (map[string]bool, error) {
	if p, ok := s.readPendingMerge(); ok {
		old, installed := undo.old.Portable(), settings.Portable()
		p.OldPortable, p.InstalledPortable = &old, &installed
		if err := s.writePendingMerge(p); err != nil {
			return nil, err
		}
	}
	skipped := false
	err := s.cfg.Settings.Update(func(m *model.Settings) {
		if account != "" {
			cur := syncclient.AccountID(model.Str(m.AuthAccessToken))
			if cur == "" || !strings.EqualFold(cur, account) {
				skipped = true
				return
			}
		}
		m.CopyPortable(settings)
	})
	if !skipped {
		installed := settings
		undo.installed = &installed
	}
	if skipped || s.accountChanged(account) {
		return nil, ErrSessionChanged
	}
	_, removed := s.cfg.History.ApplyLimitsAndSnapshot(s.settings())
	return removedSet(removed), err
}

// abandonForSession undoes a download or a merge whose session ended before
// it was recorded: its items, payloads and settings go, and the cleanup is
// saved. It returns ErrSessionChanged, with what failed.
func (s *Syncer) abandonForSession(ctx context.Context, applied []model.Item, moves *snapshot.AppliedPayloads, undo *settingsUndo) error {
	if err := s.discardMutation(ctx, applied, moves, undo); err != nil {
		return fmt.Errorf("%w (session cleanup failed: %v)", ErrSessionChanged, err)
	}
	if err := s.persistCleanup(); err != nil {
		return fmt.Errorf("%w (session cleanup not saved: %v)", ErrSessionChanged, err)
	}
	s.clearPendingMerge() // what it recorded is gone
	if moves != nil {
		moves.Rollback()
	}
	return ErrSessionChanged
}

// mutateHistory applies f to the history with the sync's own change
// suppressed. Suppression covers only the mutation: a capture made while the
// result is being saved is the user's, not the sync's, and schedules a sync.
//
// A capture can take the history right after the mutation, while the
// suppression is still on, and its notice is then dropped. So once the
// suppression is off, a history that is not what the mutation made is told
// as a change (changed): the capture is not left unsynced.
func (s *Syncer) mutateHistory(ctx context.Context, f func([]model.Item) ([]model.Item, error)) error {
	return s.mutateHistoryWithMarker(ctx, "", f)
}

func (s *Syncer) mutateHistoryWithMarker(ctx context.Context, marker string, f func([]model.Item) ([]model.Item, error)) error {
	s.suppress(true)
	applied := ""
	mutate := s.cfg.History.Mutate
	if h, ok := s.cfg.History.(interface {
		MutateWithMarker(context.Context, string, func([]model.Item) ([]model.Item, error)) error
	}); ok && marker != "" {
		mutate = func(ctx context.Context, f func([]model.Item) ([]model.Item, error)) error {
			return h.MutateWithMarker(ctx, marker, f)
		}
	}
	err := mutate(ctx, func(items []model.Item) ([]model.Item, error) {
		out, err := f(items)
		if err == nil {
			applied = Fingerprint(out)
		}
		return out, err
	})
	s.suppress(false)
	if err == nil && s.changed != nil && Fingerprint(s.cfg.History.Items()) != applied {
		s.changed()
	}
	return err
}

// recoverPayloadDirs finishes or undoes a cache-folder replacement that a
// crash interrupted. history.json is the source of truth: a folder whose
// files the history still names is current (its backup is deleted); a folder
// whose history names files that are missing is restored from its backup.
func (s *Syncer) recoverPayloadDirs() {
	snapshot.RecoverPayloads(s.cfg.Layout, s.cfg.History.Items())
	dir := s.cfg.Layout.Files()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	backups := map[string]string{}
	for _, e := range ents {
		name := e.Name()
		if i := strings.Index(name, ".old-"); i > 0 {
			backups[name[:i]] = filepath.Join(dir, name)
		}
	}
	if len(backups) == 0 {
		return
	}
	items := s.cfg.History.Items()
	for target, backup := range backups {
		targetPath := filepath.Join(dir, target)
		used, broken := false, false
		for _, it := range items {
			for _, p := range it.FilePaths {
				if filepath.Dir(filepath.Clean(p)) != targetPath {
					continue
				}
				used = true
				if !isFile(p) {
					broken = true
				}
			}
		}
		if used && !broken {
			_ = os.RemoveAll(backup)
			continue
		}
		_ = os.RemoveAll(targetPath)
		_ = os.Rename(backup, targetPath)
	}
}

// payloadFiles is where an item keeps its image or file contents.
func payloadFiles(it model.Item) []string {
	switch it.Kind {
	case model.KindImage:
		if p := strings.TrimSpace(model.Str(it.ImageFilePath)); p != "" {
			return []string{p}
		}
	case model.KindFileList:
		var out []string
		for _, p := range it.FilePaths {
			if strings.TrimSpace(p) != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return nil
}

// unreferencedFiles is the files of old that next does not refer to: the
// payloads a history mutation made orphans, to be removed only after the new
// history is on disk. Only files in Klip's own folders are listed: the
// FilePaths of an item migrated from C# point at the user's documents, and
// those are never ours to delete.
func (s *Syncer) unreferencedFiles(old, next []model.Item) []string {
	keep := map[string]bool{}
	for _, it := range next {
		for _, p := range append(payloadFiles(it), s.fileCacheFiles(it)...) {
			keep[filepath.Clean(p)] = true
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, it := range old {
		for _, p := range append(payloadFiles(it), s.fileCacheFiles(it)...) {
			c := filepath.Clean(p)
			if keep[c] || seen[c] || !s.ownPayload(c) {
				continue
			}
			seen[c] = true
			out = append(out, p)
		}
	}
	return out
}

// fileCacheFiles is the copy Klip keeps of a file-list item. A history
// migrated from C# names the user's documents in FilePaths, so the cache is
// found from the item id instead.
func (s *Syncer) fileCacheFiles(it model.Item) []string {
	if it.Kind != model.KindFileList || strings.TrimSpace(it.ID) == "" {
		return nil
	}
	// The active cache is the one FilePaths names, when it is one of ours;
	// the canonical folder is only for migrated items whose paths are the
	// user's documents. Returning every folder with the prefix would keep
	// the obsolete copies "referenced" forever.
	dir := ""
	if len(it.FilePaths) > 0 {
		if d := filepath.Dir(filepath.Clean(it.FilePaths[0])); s.ownPayload(d) {
			dir = d
		}
	}
	if dir == "" {
		dir = filepath.Join(s.cfg.Layout.Files(), it.IDN())
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// ownPayload says the file is inside the folders Klip keeps for images and
// files. A path from a migrated history that points at the user's documents
// is not one.
// payloadKeeper says whether a restored history still refers to a payload.
func (s *Syncer) payloadKeeper() func(string) bool {
	var refs []string
	for _, it := range s.cfg.History.Items() {
		if p := strings.TrimSpace(model.Str(it.ImageFilePath)); p != "" {
			refs = append(refs, p)
		}
		refs = append(refs, it.FilePaths...)
	}
	return func(target string) bool {
		for _, p := range refs {
			rel, err := filepath.Rel(target, p)
			if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			return true
		}
		return false
	}
}

func (s *Syncer) ownPayload(p string) bool {
	for _, base := range []string{s.cfg.Layout.Images(), s.cfg.Layout.Files()} {
		rel, err := filepath.Rel(base, p)
		if err != nil || filepath.IsAbs(rel) || rel == ".." {
			continue
		}
		if !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// findIdentity is the item of items that MergeItems folded into target.
func findIdentity(items []model.Item, target model.Item) (model.Item, bool) {
	id := strings.ToLower(strings.TrimSpace(target.ID))
	for _, it := range items {
		if id != "" && id != emptyGUID && strings.ToLower(it.ID) == id {
			return it, true
		}
	}
	for _, it := range items {
		if sameMergeIdentity(it, target) {
			return it, true
		}
	}
	return model.Item{}, false
}

// sameMergeIdentity says two items became one in MergeItems: same kind and
// the same id, or the same content hash (an older item may have no hash).
func sameMergeIdentity(a, b model.Item) bool {
	if a.Kind != b.Kind {
		return false
	}
	ha := strings.TrimSpace(model.Str(a.ContentHash))
	hb := strings.TrimSpace(model.Str(b.ContentHash))
	if ha != "" && hb != "" && strings.EqualFold(ha, hb) {
		return true
	}
	ia := strings.ToLower(strings.TrimSpace(a.ID))
	ib := strings.ToLower(strings.TrimSpace(b.ID))
	return ia != "" && ia != emptyGUID && ia == ib
}

// itemMetadataChanged says the user edited the metadata of an item since the
// baseline.
func itemMetadataChanged(cur, base model.Item) bool {
	return !sameNote(cur.Note, base.Note) || cur.Pinned != base.Pinned || cur.CopyCount != base.CopyCount
}

// withoutDeletedSince leaves out of remote what was deleted from the history
// between then (when a merge began, or the baseline of the last sync) and now: an item of the same id, and one
// of the same content when no item of that content is left now, which the
// merge would make one with the deleted item. It is applied before
// MergeItems folds equal content, as in MergeForDownload.
func withoutDeletedSince(remote, then, now []model.Item) []model.Item {
	present := make(map[string]bool, len(now))
	kept := make(map[string]bool, len(now))
	for _, it := range now {
		present[strings.ToLower(strings.TrimSpace(it.ID))] = true
		if k := strings.ToLower(mergeKey(it)); k != "" {
			kept[k] = true
		}
	}
	goneIDs, goneKeys := map[string]bool{}, map[string]bool{}
	for _, it := range then {
		id := strings.ToLower(strings.TrimSpace(it.ID))
		if present[id] {
			continue
		}
		if id != "" && id != emptyGUID {
			goneIDs[id] = true
		}
		if k := strings.ToLower(mergeKey(it)); k != "" && !kept[k] {
			goneKeys[k] = true
		}
	}
	if len(goneIDs) == 0 && len(goneKeys) == 0 {
		return remote
	}
	out := make([]model.Item, 0, len(remote))
	for _, it := range remote {
		if goneIDs[strings.ToLower(strings.TrimSpace(it.ID))] || goneKeys[strings.ToLower(mergeKey(it))] {
			continue
		}
		out = append(out, it)
	}
	return out
}

// MergeForDownload merges a downloaded history with the local one when a
// capture arrived during the download. Unlike a first merge, the two sides
// share a synchronized baseline, so the copy count of an item both sides have
// is the larger one, not the sum.
func MergeForDownload(remote, local, baseline []model.Item) []model.Item {
	localByID := make(map[string]model.Item, len(local))
	localIDs := make(map[string]bool, len(local))
	for _, it := range local {
		id := strings.ToLower(it.ID)
		localIDs[id] = true
		localByID[id] = it
	}
	baseByID := make(map[string]model.Item, len(baseline))
	baseIDs := make(map[string]bool, len(baseline))
	for _, it := range baseline {
		id := strings.ToLower(strings.TrimSpace(it.ID))
		if id == "" || id == emptyGUID {
			continue
		}
		baseIDs[id] = true
		baseByID[id] = it
	}
	remoteByID := make(map[string]model.Item, len(remote))
	remoteIDs := make(map[string]bool, len(remote))
	for _, it := range remote {
		id := strings.ToLower(it.ID)
		remoteIDs[id] = true
		remoteByID[id] = it
	}

	// Deletions are applied to the inputs, before MergeItems folds equal
	// content: a deleted id must not take a hash duplicate with it.
	deletedRemotely := map[string]bool{}
	for id := range baseIDs {
		if !localIDs[id] {
			continue // deleted here: withoutDeletedSince
		}
		if !remoteIDs[id] && !itemMetadataChanged(localByID[id], baseByID[id]) {
			deletedRemotely[id] = true
		}
	}
	// What was deleted here goes from the remote side by id, and by content
	// when no copy of it is left here: another device may hold it under
	// another id (withoutDeletedSince).
	filteredRemote := withoutDeletedSince(remote, baseline, local)
	filteredLocal := local[:0:0]
	for _, it := range local {
		if !deletedRemotely[strings.ToLower(it.ID)] {
			filteredLocal = append(filteredLocal, it)
		}
	}

	merged := MergeItems(filteredRemote, filteredLocal)
	localByID = make(map[string]model.Item, len(filteredLocal))
	for _, it := range filteredLocal {
		localByID[strings.ToLower(it.ID)] = it
	}
	remoteByID = make(map[string]model.Item, len(filteredRemote))
	for _, it := range filteredRemote {
		remoteByID[strings.ToLower(it.ID)] = it
	}
	recountMergedSides(merged, filteredRemote, filteredLocal)

	// A field the user changed during the download keeps the local value; an
	// unchanged field takes the downloaded value (which may be a remote edit).
	for i := range merged {
		l, lok := findConnectedIdentity(filteredLocal, merged[i], filteredRemote, filteredLocal)
		b, bok := findConnectedIdentity(baseline, merged[i], filteredRemote, filteredLocal)
		r, rok := findConnectedIdentity(filteredRemote, merged[i], filteredRemote, filteredLocal)
		if !lok || !bok {
			continue
		}
		if sameNote(l.Note, b.Note) {
			if rok {
				merged[i].Note = r.Note
			}
		} else {
			merged[i].Note = l.Note
		}
		if l.Pinned == b.Pinned {
			if rok {
				merged[i].Pinned = r.Pinned
			}
		} else {
			merged[i].Pinned = l.Pinned
		}
	}
	components := identityComponents(filteredRemote, filteredLocal)
	for i := range merged {
		for _, l := range filteredLocal {
			b, ok := baseByID[strings.ToLower(l.ID)]
			if !ok {
				continue
			}
			if sameNote(l.Note, b.Note) && l.Pinned == b.Pinned {
				continue
			}
			if components[strings.ToLower(l.ID)] != components[strings.ToLower(merged[i].ID)] {
				continue
			}
			if !sameNote(l.Note, b.Note) {
				merged[i].Note = l.Note
			}
			if l.Pinned != b.Pinned {
				merged[i].Pinned = l.Pinned
			}
		}
	}
	return merged
}

// removedSet is the ids the limits removed under their lock.
func removedSet(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[strings.ToLower(id)] = true
	}
	return out
}

// rollbackItems is the history after a failed mutation: the items before it,
// with the note, pin and copy count edits made meanwhile, plus the captures
// that were added. Items that only the failed mutation added are dropped.
func rollbackItems(before, applied, current []model.Item, limitPruned map[string]bool) []model.Item {
	// A deleted representative deletes every original in its identity group.
	keptBefore := make([]model.Item, 0, len(before))
	for _, old := range before {
		ap, ok := findIdentity(applied, old)
		if ok && !limitPruned[strings.ToLower(ap.ID)] {
			if _, present := findIdentity(current, ap); !present {
				continue
			}
		}
		keptBefore = append(keptBefore, old)
	}
	before = keptBefore
	// Resolve retained remote IDs back to their original local identity.
	applied = cloneItems(applied)
	current = cloneItems(current)
	for _, old := range before {
		if ap, ok := findIdentity(applied, old); ok && !strings.EqualFold(ap.ID, old.ID) {
			if limitPruned[strings.ToLower(ap.ID)] {
				if limitPruned == nil {
					limitPruned = map[string]bool{}
				}
				limitPruned[strings.ToLower(old.ID)] = true
			}
			for i := range applied {
				if applied[i].ID == ap.ID {
					applied[i].ID = old.ID
				}
			}
			for i := range current {
				if current[i].ID == ap.ID {
					current[i].ID = old.ID
				}
			}
		}
	}
	currentByID := make(map[string]model.Item, len(current))
	for _, it := range current {
		currentByID[strings.ToLower(it.ID)] = it
	}
	appliedIDs := make(map[string]bool, len(applied))
	for _, it := range applied {
		appliedIDs[strings.ToLower(it.ID)] = true
	}
	appliedByID := make(map[string]model.Item, len(applied))
	for _, it := range applied {
		appliedByID[strings.ToLower(it.ID)] = it
	}
	beforeIDs := make(map[string]bool, len(before))
	out := make([]model.Item, 0, len(before)+len(current))
	for _, it := range before {
		id := strings.ToLower(it.ID)
		beforeIDs[id] = true
		_, inApplied := appliedByID[id]
		cur, inCurrent := currentByID[id]
		if inApplied && !inCurrent && !limitPruned[id] {
			// The user deleted this item while the failed operation was
			// being flushed: keep the deletion. A limit pruning is not a
			// user deletion and must be undone with the replacement.
			continue
		}
		if inCurrent {
			// Only a field changed after the mutation was applied is the
			// user's edit; the rest goes back to what it was before.
			if ap, applied := appliedByID[id]; applied {
				if !sameNote(cur.Note, ap.Note) {
					it.Note = cur.Note
				}
				if cur.Pinned != ap.Pinned {
					it.Pinned = cur.Pinned
				}
				if cur.CopyCount != ap.CopyCount {
					it.CopyCount = int32(max(int64(1), min(int64(2147483647), int64(it.CopyCount)+int64(cur.CopyCount)-int64(ap.CopyCount))))
				}
				if cur.CapturedAt.After(ap.CapturedAt.Time) {
					it.CapturedAt = cur.CapturedAt
					if cur.FirstCapturedAt != nil {
						it.FirstCapturedAt = cur.FirstCapturedAt
					}
				}
			}
		}
		out = append(out, it)
	}
	for _, it := range current {
		id := strings.ToLower(it.ID)
		if ap, applied := appliedByID[id]; applied {
			// A recapture of a just-imported item updates the same id: keep
			// the user's content instead of dropping it with the import. An
			// item that existed before is restored above, with its edits.
			if !beforeIDs[id] && !sameCaptureState(it, ap) {
				out = append(out, it)
			}
			continue
		}
		if !beforeIDs[id] {
			out = append(out, it)
		}
	}
	// Sort only: MergeItems would fold duplicate items of a history that does
	// not merge them.
	sortItems(out)
	return out
}

// sameCaptureState says a current item is still the imported item, not a
// recapture that happened while the flush was failing.
// withoutIDs drops the items the limits removed from the applied snapshot.
func withoutIDs(items []model.Item, removed map[string]bool) []model.Item {
	if len(removed) == 0 {
		return items
	}
	out := make([]model.Item, 0, len(items))
	for _, it := range items {
		if !removed[strings.ToLower(it.ID)] {
			out = append(out, it)
		}
	}
	return out
}

// cloneItems detaches a snapshot of the applied items: the user's edits to
// the live history must not change the baseline rollback compares against.
func cloneItems(items []model.Item) []model.Item {
	out := make([]model.Item, len(items))
	for i, it := range items {
		out[i] = it
		if len(it.FilePaths) > 0 {
			out[i].FilePaths = append([]string(nil), it.FilePaths...)
		}
	}
	return out
}

func sameCaptureState(a, b model.Item) bool {
	if !a.CapturedAt.Equal(b.CapturedAt.Time) || a.CopyCount != b.CopyCount || a.Pinned != b.Pinned ||
		!sameNote(a.Note, b.Note) || model.Str(a.Text) != model.Str(b.Text) ||
		model.Str(a.ContentHash) != model.Str(b.ContentHash) ||
		model.Str(a.ImageFilePath) != model.Str(b.ImageFilePath) || len(a.FilePaths) != len(b.FilePaths) {
		return false
	}
	for i := range a.FilePaths {
		if a.FilePaths[i] != b.FilePaths[i] {
			return false
		}
	}
	return true
}

func sameNote(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// sortItems is the order MergeItems gives: newest first, then by id.
func sortItems(items []model.Item) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if !a.CapturedAt.Equal(b.CapturedAt.Time) {
			return a.CapturedAt.After(b.CapturedAt.Time)
		}
		return strings.ToLower(a.ID) < strings.ToLower(b.ID)
	})
}

// cleanupContext bounds the rollback of a failed operation; it is not the
// cancelled context that may have failed the save.
func cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// persistCleanup writes a session-change cleanup on a context of its own, so
// a cancelled sync still saves the removal of the old account's items. It
// retries, because the window it closes is the history surviving a restart.
func (s *Syncer) persistCleanup() error {
	var err error
	for i := 0; i < 3; i++ {
		ctx, cancel := cleanupContext()
		err = s.flush(ctx)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

// itemsNotIn is the items whose id is not in exclude.
func itemsNotIn(exclude, items []model.Item) []model.Item {
	have := make(map[string]bool, len(exclude))
	for _, it := range exclude {
		have[strings.ToLower(it.ID)] = true
	}
	var out []model.Item
	for _, it := range items {
		if !have[strings.ToLower(it.ID)] {
			out = append(out, it)
		}
	}
	return out
}

// rollbackMutation puts before back after a failed save, but keeps the items
// captured while the operation was in flight: they were neither downloaded nor
// part of the mutation, so losing them would be data loss. The previous
// portable settings are restored too.
func (s *Syncer) rollbackMutation(ctx context.Context, before, applied []model.Item, undo *settingsUndo, limitPruned map[string]bool) error {
	if err := s.mutateHistoryWithMarker(ctx, guidN(), func(current []model.Item) ([]model.Item, error) {
		if undo != nil && s.accountChanged(undo.account) {
			return itemsNotIn(applied, current), nil
		}
		normalized := cloneItems(before)
		if undo != nil {
			for i, it := range normalized {
				if ap, ok := findConnectedIdentity(applied, it, undo.identities, before, applied); ok && it.ContentHash == nil {
					normalized[i].ContentHash = ap.ContentHash
				}
			}
		}
		restored := rollbackItems(normalized, applied, current, limitPruned)
		for i := range restored {
			restored[i].LocalSyncMutation = ""
			for _, old := range before {
				if restored[i].ID == old.ID {
					restored[i].ContentHash = old.ContentHash
					break
				}
			}
		}
		return restored, nil
	}); err != nil {
		return err
	}
	s.rollbackPending = true
	serr := s.restoreSettings(undo)
	s.cfg.History.ApplyLimits(s.settings())
	if serr != nil {
		return fmt.Errorf("settings: %w", serr)
	}
	return nil
}

// discardMutation removes the items an operation had applied when the
// session changed during its commit: the new session must not inherit them.
// Unlike rollbackMutation it does not restore before, so a history the new
// session already replaced (or cleared) is left alone.
func (s *Syncer) discardMutation(ctx context.Context, applied []model.Item, _ *snapshot.AppliedPayloads, undo *settingsUndo) error {
	rctx, cancel := cleanupContext()
	defer cancel()
	if err := s.mutateHistory(rctx, func(current []model.Item) ([]model.Item, error) {
		return itemsNotIn(applied, current), nil
	}); err != nil {
		// The history still refers to the moved payloads: keep them.
		return err
	}
	if err := s.restoreSettings(undo); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	return nil
}

// settingsUndo is what a download or a merge did to the portable settings:
// what they were before it (old), and what it wrote (installed; nil while it
// wrote nothing).
type settingsUndo struct {
	identities []model.Item
	account    string
	old        model.Settings
	installed  *model.Settings
}

// restoreSettings undoes what the operation wrote to the settings, and only
// that: a setting that is no longer what it wrote (changed by the user, or by
// a session signed in meanwhile) keeps its value, and nothing is written when
// the operation wrote nothing.
func (s *Syncer) restoreSettings(u *settingsUndo) error {
	if u == nil || u.installed == nil {
		return nil
	}
	installed, old := u.installed.Portable(), u.old.Portable()
	return s.cfg.Settings.Update(func(m *model.Settings) {
		if u.account != "" && !strings.EqualFold(syncclient.AccountID(model.Str(m.AuthAccessToken)), u.account) {
			return
		}
		m.RestorePortable(installed, old)
	})
}

// retainedItems is the applied items that the history still holds: the limits
// may have pruned some, and a capture made later is not part of what this
// sync applied.
func retainedItems(applied, current []model.Item) []model.Item {
	have := make(map[string]bool, len(current))
	for _, it := range current {
		have[strings.ToLower(it.ID)] = true
	}
	out := make([]model.Item, 0, len(applied))
	for _, it := range applied {
		if have[strings.ToLower(it.ID)] {
			out = append(out, it)
		}
	}
	return out
}

func report(f func(Progress), code, msg string, pct int) {
	if f != nil {
		f(Progress{code, msg, pct})
	}
}

func (s *Syncer) tempSnapshotPath() (string, error) {
	dir := filepath.Join(s.cfg.Layout.Root, "sync", "tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(dir, 0o700)
	return filepath.Join(dir, "snapshot-"+guidN()+".sqlite"), nil
}

func (s *Syncer) preMergeBackupPath() (string, error) {
	dir := filepath.Join(s.cfg.Layout.Root, "sync", "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(dir, 0o700)
	// ".000" is the milliseconds of the layout, not literal text. The guid
	// keeps two backups of the same millisecond apart; Export deletes a file
	// that is already there.
	name := "pre-merge-" + s.cfg.Now().Format("20060102-150405.000") + "-" + guidN()[:8] + ".sqlite"
	return filepath.Join(dir, name), nil
}

func guidN() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func tryDelete(p string) {
	if p == "" {
		return
	}
	for i := 0; i < 6; i++ {
		if err := os.Remove(p); err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(120 * time.Millisecond)
	}
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func mustAbs(p string) string {
	a, _ := filepath.Abs(p)
	return a
}

// timing appends a line to sync_timing.log, as WriteTimingLog does.
func (s *Syncer) timing(op, stage string, started time.Time, details string) {
	line := fmt.Sprintf("%s | %s | %s | elapsedMs=%d", s.cfg.Now().Format(model.OLayout), op, stage, s.cfg.Now().Sub(started).Milliseconds())
	if strings.TrimSpace(details) != "" {
		line += " | " + strings.TrimSpace(details)
	}
	f, err := os.OpenFile(filepath.Join(s.cfg.Layout.Root, "sync_timing.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// ---- account binding

// accountFile is where the sync state's account is kept, next to the backups.
// It is not part of settings.json: the C# client has no such property, and a
// Klip 1.0.10 device must keep reading the file.
func (s *Syncer) accountFile() string {
	return filepath.Join(s.cfg.Layout.Root, "sync", "account")
}

// boundAccount is the account the local sync state belongs to, "" when no
// sync has recorded one yet.
func (s *Syncer) boundAccount() string {
	b, err := os.ReadFile(s.accountFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// bindAccount records the account of the state that was just saved.
func (s *Syncer) bindAccount(account string) error {
	if strings.TrimSpace(account) == "" {
		return nil
	}
	dir := filepath.Join(s.cfg.Layout.Root, "sync")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Through a file beside it: a crash in the middle must not leave an
	// empty file (bound to whoever signs in next) or a cut one.
	f, err := os.CreateTemp(dir, "account.tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.WriteString(account); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := durable.Rename(tmp, s.accountFile()); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// bindFor records the account when no state has been bound yet: the local
// history is about to be used, so from now on it belongs to this account even
// if the operation does not finish.
func (s *Syncer) bindFor(account string) error {
	if _, err := os.ReadFile(s.accountFile()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.TrimSpace(account) == "" || s.boundAccount() != "" {
		return nil
	}
	return s.bindAccount(account)
}

// checkBound refuses to sync a state that belongs to another account. After a
// sign-out or an account change the app has to clear the sync state (and this
// file) before the new account syncs; until then nothing is uploaded with the
// new account's credentials.
func (s *Syncer) checkBound(account string) error {
	if _, err := os.ReadFile(s.accountFile()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.TrimSpace(account) == "" {
		return nil
	}
	if bound := s.boundAccount(); bound != "" && !strings.EqualFold(bound, account) {
		return ErrSessionChanged
	}
	return nil
}
