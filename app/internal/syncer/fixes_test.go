package syncer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"klip/internal/model"
	"klip/internal/paths"
	"klip/internal/snapshot"
	"klip/internal/syncclient"
)

// unitHistory is the History of a Syncer that does not touch a disk.
type unitHistory struct {
	items    []model.Item
	replaced int
}

func (h *unitHistory) Items() []model.Item { return append([]model.Item(nil), h.items...) }

func (h *unitHistory) ReplaceAll(_ context.Context, items []model.Item) error {
	h.items = append([]model.Item(nil), items...)
	h.replaced++
	return nil
}

func (h *unitHistory) RemoveFiles(paths []string) error {
	for _, p := range paths {
		_ = os.Remove(p)
	}
	return nil
}

func (h *unitHistory) Mutate(_ context.Context, f func([]model.Item) ([]model.Item, error)) error {
	next, err := f(append([]model.Item(nil), h.items...))
	if err != nil {
		return err
	}
	h.items = append([]model.Item(nil), next...)
	h.replaced++
	return nil
}

func (h *unitHistory) ApplyLimits(model.Settings) {}

func idsOf(items []model.Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, strings.ToLower(it.ID))
	}
	return out
}

func removedIDs(before, after []string) []string {
	have := make(map[string]bool, len(after))
	for _, id := range after {
		have[id] = true
	}
	var out []string
	for _, id := range before {
		if !have[id] {
			out = append(out, id)
		}
	}
	return out
}

func (h *unitHistory) ApplyLimitsAndSnapshot(s model.Settings) ([]model.Item, []string) {
	before := idsOf(h.Items())
	h.ApplyLimits(s)
	after := h.Items()
	return after, removedIDs(before, idsOf(after))
}

// changingHistory changes after the first read, as a capture does while an
// upload is running.
type changingHistory struct {
	first []model.Item
	later []model.Item
	reads int
}

func (h *changingHistory) Items() []model.Item {
	h.reads++
	if h.reads == 1 {
		return append([]model.Item(nil), h.first...)
	}
	return append([]model.Item(nil), h.later...)
}

func (h *changingHistory) Mutate(ctx context.Context, f func([]model.Item) ([]model.Item, error)) error {
	next, err := f(h.Items())
	if err != nil {
		return err
	}
	return h.ReplaceAll(ctx, next)
}

func (h *changingHistory) RemoveFiles([]string) error                     { return nil }
func (h *changingHistory) ReplaceAll(context.Context, []model.Item) error { return nil }

func (h *changingHistory) ApplyLimitsAndSnapshot(s model.Settings) ([]model.Item, []string) {
	before := idsOf(h.Items())
	h.ApplyLimits(s)
	after := h.Items()
	return after, removedIDs(before, idsOf(after))
}

func (h *changingHistory) ApplyLimits(model.Settings) {}

func signedIn(s model.Settings) *model.Settings {
	s.AuthAccessToken = model.Ptr("token")
	s.AuthRefreshToken = model.Ptr("refresh")
	s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	return &s
}

func unitSyncer(t *testing.T, srv *httptest.Server, s *model.Settings, h History) *Syncer {
	t.Helper()
	return New(Config{
		Layout: paths.Layout{Root: t.TempDir()},
		Settings: syncclient.Settings{
			Get:    func() model.Settings { return *s },
			Update: func(f func(*model.Settings)) error { f(s); return nil },
		},
		History: h,
		Client:  syncclient.New(srv.URL),
		BaseURL: srv.URL,
		Now:     time.Now,
		Flush:   func(context.Context) error { return nil },
	})
}

func textItemAt(id, text string) model.Item {
	return model.Item{
		ID: id, Kind: model.KindText, CapturedAt: model.Time{Time: time.Now()},
		ApproxBytes: int64(len(text)) * 2, Text: model.Ptr(text), CopyCount: 1,
	}
}

// The manifest is only the state after an upload when it still describes
// that upload. Another device may have uploaded in between.
func TestPersistAfterUploadKeepsTheUploadedVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"schema":1,"latest":{"version":"other-device","updatedAt":"2026-05-30T13:00:00+00:00","snapshot":{"sha256":"their-sha","size":11}}}`)
	}))
	defer srv.Close()

	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, &unitHistory{items: []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "hello")}})
	up := &syncclient.UploadResult{Version: "mine", SHA256: "my-sha"}
	if err := syn.persistAfterUpload(context.Background(), up, &ManifestInfo{Version: "basis"}, "fp-sent", ""); err != nil {
		t.Fatal(err)
	}

	if got := model.Str(s.SyncLastRemoteVersion); got != "mine" {
		t.Errorf("version %q: the manifest of another device was recorded", got)
	}
	if got := strings.ToLower(model.Str(s.SyncLastRemoteSha256)); got != "my-sha" {
		t.Errorf("sha %q", got)
	}
	if got := model.Str(s.SyncLastLocalFingerprint); got != "fp-sent" {
		t.Errorf("fingerprint %q", got)
	}
	if got := model.Str(s.SyncLastRemoteUpdatedAt); got == "2026-05-30T13:00:00.0000000+00:00" {
		t.Error("the updatedAt of another device was recorded")
	}
}

func TestPersistAfterUploadUsesTheManifestOfItsOwnUpload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"schema":1,"latest":{"version":"mine","updatedAt":"2026-05-30T13:00:00+00:00","snapshot":{"sha256":"my-sha","size":11}}}`)
	}))
	defer srv.Close()

	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, &unitHistory{})
	if err := syn.persistAfterUpload(context.Background(), &syncclient.UploadResult{Version: "mine", SHA256: "my-sha"}, nil, "fp", ""); err != nil {
		t.Fatal(err)
	}

	if got := model.Str(s.SyncLastRemoteVersion); got != "mine" {
		t.Errorf("version %q", got)
	}
	at, err := model.ParseTime(model.Str(s.SyncLastRemoteUpdatedAt))
	if err != nil || at.UTC().Format(time.RFC3339) != "2026-05-30T13:00:00Z" {
		t.Errorf("updatedAt %q %v", model.Str(s.SyncLastRemoteUpdatedAt), err)
	}
}

func TestUploadIsInManifest(t *testing.T) {
	cases := []struct {
		name string
		up   *syncclient.UploadResult
		m    ManifestInfo
		want bool
	}{
		{"version", &syncclient.UploadResult{Version: "v2"}, ManifestInfo{Version: "v2"}, true},
		{"same version, other sha", &syncclient.UploadResult{Version: "v2", SHA256: "abc"}, ManifestInfo{Version: "v2", SnapshotSHA256: "def"}, false},
		{"another version", &syncclient.UploadResult{Version: "v2"}, ManifestInfo{Version: "v3"}, false},
		{"sha", &syncclient.UploadResult{SHA256: "ABC"}, ManifestInfo{SnapshotSHA256: "abc"}, true},
		{"another sha", &syncclient.UploadResult{SHA256: "abc"}, ManifestInfo{SnapshotSHA256: "def"}, false},
		{"nothing to compare", &syncclient.UploadResult{}, ManifestInfo{Version: "v2"}, false},
		{"no upload", nil, ManifestInfo{Version: "v2"}, false},
	}
	for _, c := range cases {
		if got := uploadIsInManifest(c.up, c.m); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// The state after an upload is the fingerprint of the items that went into
// the snapshot, not of a history that changed while the upload ran.
func TestUploadRemembersTheItemsItSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/sync/snapshot":
			io.WriteString(w, `{"version":"v1","sha256":"sha-1","size":10}`)
		case r.URL.Path == "/sync/manifest":
			io.WriteString(w, `{"schema":1,"latest":{"version":"v1","updatedAt":"2026-05-30T12:00:00+00:00","snapshot":{"sha256":"sha-1","size":10}}}`)
		case r.URL.Path == "/sync/blobs/check":
			io.WriteString(w, `{"missing":[]}`)
		}
	}))
	defer srv.Close()

	sent := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "sent")
	captured := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "captured while uploading")
	h := &changingHistory{first: []model.Item{sent}, later: []model.Item{captured, sent}}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)

	if _, err := syn.UploadAfterLocalMutation(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := model.Str(s.SyncLastLocalFingerprint)
	if got != Fingerprint([]model.Item{sent}) {
		t.Errorf("fingerprint is not the one of the exported items: %s", got)
	}
	if got == Fingerprint([]model.Item{captured, sent}) {
		t.Error("the change made during the upload was remembered as uploaded")
	}
}

// A backup that refers to a blob which could not be stored is not a backup:
// the merge stops before it replaces the history.
func TestMergeStopsWhenTheBackupBlobCannotBeStored(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	if err := os.WriteFile(l.Blobs(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.Images(), 0o755); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(l.Images(), "1780000000000_aaaa.png")
	if err := os.WriteFile(img, []byte("image bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &unitHistory{items: []model.Item{{
		ID: "7c9e6679-7425-40de-944b-e07fc1f90ae7", Kind: model.KindImage,
		CapturedAt: model.Time{Time: time.Now()}, ApproxBytes: 11, ImageFilePath: &img, CopyCount: 1,
	}}}
	s := signedIn(model.DefaultSettings())
	syn := New(Config{
		Flush:  func(context.Context) error { return nil },
		Layout: l,
		Settings: syncclient.Settings{
			Get:    func() model.Settings { return *s },
			Update: func(f func(*model.Settings)) error { f(s); return nil },
		},
		History: h,
		Client:  syncclient.New("http://127.0.0.1:1"),
		BaseURL: "http://127.0.0.1:1",
		Now:     time.Now,
	})

	err := syn.MergeAndApply(context.Background(), ManifestInfo{}, "", nil, "test")
	if err == nil || !strings.Contains(err.Error(), "pre-merge backup") {
		t.Fatalf("a backup that cannot be read back: %v", err)
	}
	if h.replaced != 0 {
		t.Error("the merge replaced the history without a usable backup")
	}
}

// Two backups of the same second, or millisecond, are two files: Go's layout
// needs ".000" for the milliseconds, not "-000".
func TestPreMergeBackupNamesAreUnique(t *testing.T) {
	now := time.Date(2026, 5, 30, 12, 0, 0, 500_000_000, time.UTC)
	syn := New(Config{Flush: func(context.Context) error { return nil }, Layout: paths.Layout{Root: t.TempDir()}, Now: func() time.Time { return now }})

	first, err := syn.preMergeBackupPath()
	if err != nil {
		t.Fatal(err)
	}
	second, err := syn.preMergeBackupPath()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two backups of one moment: %s", first)
	}
	want := regexp.MustCompile(`^pre-merge-20260530-120000\.500-[0-9a-f]{8}\.sqlite$`)
	for _, p := range []string{first, second} {
		if !want.MatchString(filepath.Base(p)) {
			t.Errorf("name %q", filepath.Base(p))
		}
	}
}

// snapshotFile exports a snapshot of items, as a server would have it.
func snapshotFile(t *testing.T, items []model.Item) []byte {
	t.Helper()
	l := paths.Layout{Root: t.TempDir()}
	path := filepath.Join(l.Root, "snapshot.sqlite")
	if _, err := snapshot.Export(context.Background(), l, path, items, model.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func serveSnapshot(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/snapshot" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(data)
	}))
}

func serveManifest(t *testing.T, version, sha, updatedAt string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"schema":1,"latest":{"version":"`+version+`","updatedAt":"`+updatedAt+
			`","snapshot":{"sha256":"`+sha+`","size":1}}}`)
	}))
}

// mergeHistory has a capture arrive after the local history was read for the
// backup, but before the merge runs.
type mergeHistory struct {
	local    []model.Item
	captured model.Item
	stored   []model.Item
}

func (h *mergeHistory) Items() []model.Item { return append([]model.Item(nil), h.local...) }

func (h *mergeHistory) Mutate(_ context.Context, f func([]model.Item) ([]model.Item, error)) error {
	next, err := f(append([]model.Item{h.captured}, h.local...))
	if err != nil {
		return err
	}
	h.stored = next
	return nil
}

func (h *mergeHistory) RemoveFiles([]string) error                     { return nil }
func (h *mergeHistory) ReplaceAll(context.Context, []model.Item) error { return nil }

func (h *mergeHistory) ApplyLimitsAndSnapshot(s model.Settings) ([]model.Item, []string) {
	before := idsOf(h.Items())
	h.ApplyLimits(s)
	after := h.Items()
	return after, removedIDs(before, idsOf(after))
}

func (h *mergeHistory) ApplyLimits(model.Settings) {}

// A capture made while the remote snapshot is downloading is part of the
// merge: the history is read again, under its lock, when the merge applies.
func TestMergeAppliesTheHistoryAsItIs(t *testing.T) {
	remote := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "remote")
	local := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")
	captured := textItemAt("16fd2706-8baf-433b-82eb-8c7fada847da", "captured during the download")
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{remote}))
	defer srv.Close()
	h := &mergeHistory{local: []model.Item{local}, captured: captured}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)

	if err := syn.MergeAndApply(context.Background(), ManifestInfo{}, "", nil, "test"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range h.stored {
		got = append(got, model.Str(it.Text))
	}
	if len(got) != 3 {
		t.Fatalf("merged %v: the capture made during the merge was lost", got)
	}
	found := false
	for _, text := range got {
		if text == "captured during the download" {
			found = true
		}
	}
	if !found {
		t.Errorf("merged %v", got)
	}
}

// captureAfterMutationHistory takes a capture just after the download
// replaced the history, before the sync remembers its state.
type captureAfterMutationHistory struct {
	unitHistory
	extra model.Item
}

func (h *captureAfterMutationHistory) Mutate(ctx context.Context, f func([]model.Item) ([]model.Item, error)) error {
	if err := h.unitHistory.Mutate(ctx, f); err != nil {
		return err
	}
	h.items = append([]model.Item{h.extra}, h.items...)
	return nil
}

// The fingerprint after a download is of what the download applied, not of a
// capture that arrived while it was saving.
func TestDownloadRemembersTheHistoryItApplied(t *testing.T) {
	remote := []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")}
	data := snapshotFile(t, remote)
	srv := serveSnapshot(t, data)
	defer srv.Close()
	h := &captureAfterMutationHistory{extra: textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "captured after saving")}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)

	dr, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := Fingerprint(remote); dr.Fingerprint != want {
		t.Errorf("fingerprint %s, want %s: a capture made during the save was recorded as downloaded", dr.Fingerprint, want)
	}
	if dr.Fingerprint == Fingerprint(h.Items()) {
		t.Error("the fingerprint is of the live history")
	}
	tmp := filepath.Join(t.TempDir(), "downloaded.sqlite")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	wantSHA, err := snapshot.FileSHA256(tmp)
	if err != nil || dr.SnapshotSHA256 != wantSHA {
		t.Errorf("sha %q, want %q (%v)", dr.SnapshotSHA256, wantSHA, err)
	}
}

// A manual restore remembers the manifest only when it still describes the
// snapshot that was restored; otherwise only the hash, so the next sync looks
// again.
func TestRestoreStateIsBoundToTheDownloadedSnapshot(t *testing.T) {
	other := serveManifest(t, "v2", "bbb", "2026-05-30T13:00:00+00:00")
	defer other.Close()
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, other, s, &unitHistory{})
	if err := syn.PersistAfterDownload(context.Background(), DownloadResult{Fingerprint: "fp", SnapshotSHA256: "aaa"}); err != nil {
		t.Fatal(err)
	}

	if got := model.Str(s.SyncLastRemoteVersion); got == "v2" {
		t.Errorf("version %q: another device's snapshot was recorded", got)
	} else if got != "aaa" {
		t.Errorf("version %q: the downloaded hash was not used as the marker", got)
	}
	if got := model.Str(s.SyncLastRemoteSha256); got != "aaa" {
		t.Errorf("sha %q", got)
	}
	if got := model.Str(s.SyncLastLocalFingerprint); got != "fp" {
		t.Errorf("fingerprint %q", got)
	}

	same := serveManifest(t, "v2", "AAA", "2026-05-30T13:00:00+00:00")
	defer same.Close()
	s2 := signedIn(model.DefaultSettings())
	syn2 := unitSyncer(t, same, s2, &unitHistory{})
	if err := syn2.PersistAfterDownload(context.Background(), DownloadResult{Fingerprint: "fp", SnapshotSHA256: "aaa"}); err != nil {
		t.Fatal(err)
	}
	if got := model.Str(s2.SyncLastRemoteVersion); got != "v2" {
		t.Errorf("version %q: the manifest of the restored snapshot was not used", got)
	}
}

// A successful but garbled manifest answer must not look like "the server has
// no snapshot": that would upload local history over the remote one.
func TestParseManifestStrictRejectsAGarbledAnswer(t *testing.T) {
	for _, in := range []string{
		"",
		"<html>proxy login</html>",
		`{}`,
		`{"latest":{}}`,
		`{"latest":{"updatedAt":"not a time"}}`,
		`{"latest":{"updatedAt":"2026-05-30T12:00:00+00:00","snapshot":[]}}`,
		`{"latest":`,
	} {
		if _, err := ParseManifestStrict(in); err == nil {
			t.Errorf("%q was accepted as a manifest", in)
		}
	}
	m, err := ParseManifestStrict(`{"schema":1,"latest":{"version":"v1","updatedAt":"2026-05-30T12:00:00+00:00","snapshot":{"sha256":"abc","size":12}}}`)
	if err != nil || m.Version != "v1" || m.SnapshotSHA256 != "abc" || m.UpdatedAt == nil || m.SnapshotSize == nil || *m.SnapshotSize != 12 {
		t.Errorf("%+v %v", m, err)
	}
}

// downloadArrivalHistory has the history change between the decision and the
// download: current is what the history holds when the download applies.
type downloadArrivalHistory struct {
	initial []model.Item
	current []model.Item
	items   []model.Item
}

func (h *downloadArrivalHistory) Items() []model.Item {
	if h.items != nil {
		return append([]model.Item(nil), h.items...)
	}
	return append([]model.Item(nil), h.initial...)
}

func (h *downloadArrivalHistory) Mutate(_ context.Context, f func([]model.Item) ([]model.Item, error)) error {
	next, err := f(append([]model.Item(nil), h.current...))
	if err != nil {
		return err
	}
	h.items = next
	return nil
}

func (h *downloadArrivalHistory) RemoveFiles([]string) error                     { return nil }
func (h *downloadArrivalHistory) ReplaceAll(context.Context, []model.Item) error { return nil }

func (h *downloadArrivalHistory) ApplyLimitsAndSnapshot(s model.Settings) ([]model.Item, []string) {
	before := idsOf(h.Items())
	h.ApplyLimits(s)
	after := h.Items()
	return after, removedIDs(before, idsOf(after))
}

func (h *downloadArrivalHistory) ApplyLimits(model.Settings) {}

// A capture made after Run chose Download but before the snapshot was applied
// is merged, not replaced: the fingerprint at the decision no longer matches.
func TestDownloadKeepsACaptureMadeWhileItRuns(t *testing.T) {
	remote := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{remote}))
	defer srv.Close()
	local := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local at the decision")
	captured := textItemAt("16fd2706-8baf-433b-82eb-8c7fada847da", "captured while downloading")
	h := &downloadArrivalHistory{initial: []model.Item{local}, current: []model.Item{captured, local}}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)

	dr, err := syn.DownloadAndApply(context.Background(), nil, "test", Fingerprint([]model.Item{local}), nil)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, it := range h.items {
		texts = append(texts, model.Str(it.Text))
	}
	if len(texts) != 3 {
		t.Fatalf("history %v: the capture made during the download was lost", texts)
	}
	if got := dr.Fingerprint; got != Fingerprint(h.items) {
		t.Errorf("fingerprint %q is not of the applied history", got)
	}
}

// A pre-merge backup must not accept a cached blob that does not hash to its
// name: the source is still there to repair it.
func TestMergeBackupRepairsACorruptBlob(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	if err := os.MkdirAll(l.Images(), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(l.Images(), "good.png")
	if err := os.WriteFile(src, []byte("good bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := snapshot.FileSHA256(src)
	if err != nil {
		t.Fatal(err)
	}
	target := l.BlobPath(sum)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Flush: func(context.Context) error { return nil }, Layout: l})
	if err := s.ensureLocalBackupBlobs(context.Background(), snapshot.ExportResult{
		Blobs: []snapshot.Blob{{SHA256: sum, SourcePath: src, Size: 10}},
	}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "good bytes" {
		t.Errorf("the corrupt blob was kept: %q", b)
	}
}

// Two runs must not interleave: each looks at the manifest, decides, applies
// and remembers, and a second decision on the same manifest would apply the
// same remote items twice.
func TestRunIsSerialized(t *testing.T) {
	var active, peak atomic.Int32
	enter := func() {
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				return
			}
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enter()
		defer active.Add(-1)
		time.Sleep(20 * time.Millisecond)
		switch r.URL.Path {
		case "/sync/manifest":
			w.WriteHeader(http.StatusNotFound)
		case "/sync/snapshot":
			io.WriteString(w, `{"version":"v1","sha256":"sha-1","size":10}`)
		case "/sync/blobs/check":
			io.WriteString(w, `{"missing":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, &unitHistory{items: []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "one")}})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = syn.Run(context.Background(), nil, "test")
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := peak.Load(); n > 1 {
		t.Errorf("%d requests were in flight at once: the runs overlapped", n)
	}
}

// The same-sha shortcut is only valid while the remote snapshot is still
// there: after a storage reset the export has to be uploaded again.
func TestUploadOnlySkipsWhenTheRemoteHasThatSnapshot(t *testing.T) {
	var puts atomic.Int32
	var mu sync.Mutex
	remoteSHA := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/manifest":
			mu.Lock()
			sha := remoteSHA
			mu.Unlock()
			if sha == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, `{"latest":{"version":"v-old","updatedAt":"2026-05-30T12:00:00+00:00","snapshot":{"sha256":"`+sha+`","size":1}}}`)
		case "/sync/snapshot":
			puts.Add(1)
			io.WriteString(w, `{"version":"v-new","sha256":"x","size":1}`)
		case "/sync/blobs/check":
			io.WriteString(w, `{"missing":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	h := &unitHistory{items: []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "one")}}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	tmp := filepath.Join(t.TempDir(), "export.sqlite")
	exp, err := snapshot.Export(context.Background(), syn.cfg.Layout, tmp, h.items, *s)
	if err != nil {
		t.Fatal(err)
	}
	s.SyncLastRemoteSha256, s.SyncLastRemoteVersion = &exp.SHA256, model.Ptr("v-old")

	// The remote lost its snapshot: upload it again even though the export
	// has the hash the device remembers.
	if _, err := syn.Upload(context.Background(), nil, "again"); err != nil {
		t.Fatal(err)
	}
	if n := puts.Load(); n != 1 {
		t.Fatalf("uploads %d: a snapshot that is gone was not uploaded", n)
	}

	// The remote still has it: the shortcut applies.
	mu.Lock()
	remoteSHA = exp.SHA256
	mu.Unlock()
	up, err := syn.Upload(context.Background(), nil, "again")
	if err != nil {
		t.Fatal(err)
	}
	if n := puts.Load(); n != 1 {
		t.Errorf("uploads %d: the same snapshot was uploaded again", n)
	}
	if up.Version != "v-old" {
		t.Errorf("version %q: the manifest that confirmed the snapshot was not returned", up.Version)
	}
}

// When a capture turns a download into a merge, the merged history is
// uploaded; otherwise it would be remembered as synchronized while the
// server still has the older snapshot.
func TestRunUploadsAfterADownloadMergedACapture(t *testing.T) {
	remote := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "remote")
	data := snapshotFile(t, []model.Item{remote})
	tmp := filepath.Join(t.TempDir(), "remote.sqlite")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, err := snapshot.FileSHA256(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sync/manifest":
			io.WriteString(w, `{"schema":1,"latest":{"version":"v1","updatedAt":"2026-05-30T12:00:00+00:00","snapshot":{"sha256":"`+sha+`","size":1}}}`)
		case r.URL.Path == "/sync/snapshot" && r.Method == http.MethodGet:
			w.Write(data)
		case r.URL.Path == "/sync/snapshot" && r.Method == http.MethodPut:
			puts.Add(1)
			io.WriteString(w, `{"version":"v2","sha256":"uploaded","size":1}`)
		case r.URL.Path == "/sync/blobs/check":
			io.WriteString(w, `{"missing":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	captured := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "captured during the download")
	h := &downloadArrivalHistory{current: []model.Item{captured}}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	res, err := syn.Run(context.Background(), nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision.Direction != Download {
		t.Fatalf("decision %+v", res.Decision)
	}
	if n := puts.Load(); n != 1 {
		t.Errorf("uploads %d: the merged history was not published", n)
	}
	if len(h.items) != 2 {
		t.Errorf("history %d items: remote and the capture should both be there", len(h.items))
	}
	if got := model.Str(s.SyncLastRemoteVersion); got != "v2" {
		t.Errorf("version %q", got)
	}
}

func testJWT(sub string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + sub + `"}`))
	return "header." + payload + ".signature"
}

// An upload that started in one account must not retry with another account's
// token: that would publish the first account's snapshot into the second.
func TestUploadStopsWhenTheSessionChanges(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/sync/snapshot":
			once.Do(func() { close(entered) })
			<-release
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/auth/refresh":
			refreshes.Add(1)
			io.WriteString(w, `{"accessToken":"`+testJWT("account-b")+`","refreshToken":"rb","accessTokenExpiresAt":"2030-01-01T00:00:00+00:00"}`)
		case r.URL.Path == "/sync/blobs/check":
			io.WriteString(w, `{"missing":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	var mu sync.Mutex
	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr(testJWT("account-a"))
	s.AuthRefreshToken = model.Ptr("ra")
	s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	syn := New(Config{
		Flush:  func(context.Context) error { return nil },
		Layout: paths.Layout{Root: t.TempDir()},
		Settings: syncclient.Settings{
			Get:    func() model.Settings { mu.Lock(); defer mu.Unlock(); return s },
			Update: func(f func(*model.Settings)) error { mu.Lock(); defer mu.Unlock(); f(&s); return nil },
		},
		History: &unitHistory{items: []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "private")}},
		Client:  syncclient.New(srv.URL),
		BaseURL: srv.URL,
		Now:     time.Now,
	})

	done := make(chan error, 1)
	go func() {
		_, err := syn.Upload(context.Background(), nil, "test")
		done <- err
	}()
	<-entered
	// The user signs into another account while the upload is in flight.
	mu.Lock()
	s.AuthAccessToken = model.Ptr(testJWT("account-b"))
	s.AuthRefreshToken = model.Ptr("rb")
	mu.Unlock()
	close(release)

	err := <-done
	if !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("a session change during the upload: %v", err)
	}
	if n := refreshes.Load(); n != 0 {
		t.Errorf("the new account's token was refreshed %d times for the old upload", n)
	}
}

// A download that started in one account must not apply its snapshot after
// the user signed into another: the new session's local history would hold
// the old account's clipboard contents, which the next sync would upload.
func TestDownloadStopsWhenTheSessionChanges(t *testing.T) {
	data := snapshotFile(t, []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from account a")})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/snapshot" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		once.Do(func() { close(entered) })
		<-release
		w.Write(data)
	}))
	defer srv.Close()

	var mu sync.Mutex
	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr(testJWT("account-a"))
	s.AuthRefreshToken = model.Ptr("ra")
	s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	h := &unitHistory{}
	syn := New(Config{
		Flush:  func(context.Context) error { return nil },
		Layout: paths.Layout{Root: t.TempDir()},
		Settings: syncclient.Settings{
			Get:    func() model.Settings { mu.Lock(); defer mu.Unlock(); return s },
			Update: func(f func(*model.Settings)) error { mu.Lock(); defer mu.Unlock(); f(&s); return nil },
		},
		History: h,
		Client:  syncclient.New(srv.URL),
		BaseURL: srv.URL,
		Now:     time.Now,
	})

	done := make(chan error, 1)
	go func() {
		_, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil)
		done <- err
	}()
	<-entered
	mu.Lock()
	s.AuthAccessToken = model.Ptr(testJWT("account-b"))
	s.AuthRefreshToken = model.Ptr("rb")
	mu.Unlock()
	close(release)

	if err := <-done; !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("a session change during the download: %v", err)
	}
	if h.replaced != 0 {
		t.Error("the old account's snapshot was applied to the new session")
	}
}

// The token used for a retry is checked itself, not the settings it was read
// from: another account's token is never sent for this workflow's data.
func TestTokenForRejectsAnotherAccountsToken(t *testing.T) {
	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr(testJWT("account-b"))
	syn := New(Config{
		Flush: func(context.Context) error { return nil },
		Settings: syncclient.Settings{
			Get:    func() model.Settings { return s },
			Update: func(f func(*model.Settings)) error { f(&s); return nil },
		},
	})
	if _, err := syn.tokenFor("account-a"); !errors.Is(err, ErrSessionChanged) {
		t.Errorf("another account's token: %v", err)
	}
	if tok, err := syn.tokenFor("account-b"); err != nil || tok == "" {
		t.Errorf("the same account's token: %q %v", tok, err)
	}
	// An opaque token cannot be attributed, so it is not rejected.
	s.AuthAccessToken = model.Ptr("opaque")
	if tok, err := syn.tokenFor("account-a"); err != nil || tok != "opaque" {
		t.Errorf("opaque token: %q %v", tok, err)
	}
}

// Signing out is a session change too: an in-flight download must not put the
// old account's history into the signed-out session.
func TestDownloadStopsWhenSignedOut(t *testing.T) {
	data := snapshotFile(t, []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the account")})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/snapshot" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		once.Do(func() { close(entered) })
		<-release
		w.Write(data)
	}))
	defer srv.Close()

	var mu sync.Mutex
	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr(testJWT("account-a"))
	s.AuthRefreshToken = model.Ptr("ra")
	s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	h := &unitHistory{}
	syn := New(Config{
		Flush:  func(context.Context) error { return nil },
		Layout: paths.Layout{Root: t.TempDir()},
		Settings: syncclient.Settings{
			Get:    func() model.Settings { mu.Lock(); defer mu.Unlock(); return s },
			Update: func(f func(*model.Settings)) error { mu.Lock(); defer mu.Unlock(); f(&s); return nil },
		},
		History: h,
		Client:  syncclient.New(srv.URL),
		BaseURL: srv.URL,
		Now:     time.Now,
	})

	done := make(chan error, 1)
	go func() {
		_, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil)
		done <- err
	}()
	<-entered
	mu.Lock()
	s.AuthAccessToken, s.AuthRefreshToken = nil, nil
	s.AuthExpiresAtUnixMs = 0
	mu.Unlock()
	close(release)

	if err := <-done; !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("a sign-out during the download: %v", err)
	}
	if h.replaced != 0 {
		t.Error("the old account's snapshot was applied after the sign-out")
	}
}

// snapshotFileWithSettings is snapshotFile with the portable settings the
// snapshot carries.
func snapshotFileWithSettings(t *testing.T, items []model.Item, settings model.Settings) []byte {
	t.Helper()
	l := paths.Layout{Root: t.TempDir()}
	path := filepath.Join(l.Root, "snapshot.sqlite")
	if _, err := snapshot.Export(context.Background(), l, path, items, settings); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// pruningHistory applies the item limit as the real history does.
type pruningHistory struct {
	downloadArrivalHistory
}

func (h *pruningHistory) ApplyLimitsAndSnapshot(s model.Settings) ([]model.Item, []string) {
	before := idsOf(h.Items())
	h.ApplyLimits(s)
	after := h.Items()
	return after, removedIDs(before, idsOf(after))
}

func (h *pruningHistory) ApplyLimits(s model.Settings) {
	if n := int(s.MaxItems); n > 0 && len(h.items) > n {
		h.items = h.items[:n]
	}
}

// A download that turns into a merge must raise the item limit: otherwise
// ApplyLimits prunes the merged history before Run can upload it.
func TestDownloadMergeExpandsLimits(t *testing.T) {
	remote := []model.Item{
		textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "remote one"),
		textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "remote two"),
	}
	settings := model.DefaultSettings()
	settings.MaxItems = 2
	srv := serveSnapshot(t, snapshotFileWithSettings(t, remote, settings))
	defer srv.Close()

	local := textItemAt("16fd2706-8baf-433b-82eb-8c7fada847da", "local")
	captured := textItemAt("11111111-2222-3333-4444-555555555555", "captured while downloading")
	h := &pruningHistory{downloadArrivalHistory{initial: []model.Item{local}, current: []model.Item{captured, local}}}
	s := signedIn(model.DefaultSettings())
	s.MaxItems = 2
	syn := unitSyncer(t, srv, s, h)

	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", Fingerprint([]model.Item{local}), nil); err != nil {
		t.Fatal(err)
	}
	if len(h.items) != 4 {
		t.Errorf("history %d items: the merged items were pruned", len(h.items))
	}
	if s.MaxItems < 4 {
		t.Errorf("MaxItems %d: the limit was not expanded for the merge", s.MaxItems)
	}
}

// The old files and history stay until the new history is on disk: a failed
// flush restores both.
func TestDownloadRollsBackWhenFlushFails(t *testing.T) {
	remote := []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")}
	srv := serveSnapshot(t, snapshotFile(t, remote))
	defer srv.Close()

	local := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")
	h := &downloadArrivalHistory{initial: []model.Item{local}, current: []model.Item{local}}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	syn.cfg.Flush = func(context.Context) error { return errors.New("disk full") }

	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); err == nil {
		t.Fatal("a failed flush stopped nothing")
	}
	if len(h.items) != 1 || h.items[0].ID != local.ID {
		t.Errorf("the history was not put back: %+v", h.items)
	}
}

func TestMergeBackupFailsWhenTheSourceIsGone(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	s := New(Config{Flush: func(context.Context) error { return nil }, Layout: l})
	err := s.ensureLocalBackupBlobs(context.Background(), snapshot.ExportResult{
		Blobs: []snapshot.Blob{{
			SHA256:     strings.Repeat("a", 64),
			SourcePath: filepath.Join(t.TempDir(), "gone.png"),
			Size:       10,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "payload is gone") {
		t.Errorf("a backup without its payload: %v", err)
	}
}

// cleaningHistory removes payloads in RemoveFiles, as the real history does.
type cleaningHistory struct {
	unitHistory
	removed []string
}

func (h *cleaningHistory) RemoveFiles(paths []string) error {
	h.removed = append(h.removed, paths...)
	return h.unitHistory.RemoveFiles(paths)
}

// A payload that the downloaded history no longer refers to stays until the
// new history is saved; only then is it removed.
func TestDownloadKeepsPayloadsUntilFlushed(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	if err := os.MkdirAll(l.Images(), 0o755); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(l.Images(), "1780000000000_local.png")
	if err := os.WriteFile(img, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	local := model.Item{
		ID: "7c9e6679-7425-40de-944b-e07fc1f90ae7", Kind: model.KindImage,
		CapturedAt: model.Time{Time: time.Now()}, ApproxBytes: 5, ImageFilePath: &img, CopyCount: 1,
	}
	remote := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{remote}))
	defer srv.Close()

	h := &cleaningHistory{unitHistory: unitHistory{items: []model.Item{local}}}
	s := signedIn(model.DefaultSettings())
	syn := New(Config{
		Flush:    func(context.Context) error { return nil },
		Layout:   l,
		Settings: syncclient.Settings{Get: func() model.Settings { return *s }, Update: func(f func(*model.Settings)) error { f(s); return nil }},
		History:  h,
		Client:   syncclient.New(srv.URL),
		BaseURL:  srv.URL,
		Now:      time.Now,
	})
	syn.cfg.Flush = func(context.Context) error { return errors.New("disk full") }
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); err == nil {
		t.Fatal("a failed flush stopped nothing")
	}
	if _, err := os.Stat(img); err != nil {
		t.Fatal("the old payload was removed before the new history was saved")
	}
	if len(h.items) != 1 || h.items[0].ID != local.ID {
		t.Errorf("the old history was not restored: %+v", h.items)
	}

	// The next attempt saves the history, and only then removes the payload.
	syn.cfg.Flush = func(context.Context) error { return nil }
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(img); !os.IsNotExist(err) {
		t.Errorf("the unreferenced payload is still there: %v", err)
	}
}

// A capture made while the failed download is being flushed is not part of
// what is rolled back; the previous portable settings come back too.
func TestDownloadRollbackKeepsANewCaptureAndTheSettings(t *testing.T) {
	remoteSettings := model.DefaultSettings()
	remoteSettings.MaxItems = 1
	remote := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")
	srv := serveSnapshot(t, snapshotFileWithSettings(t, []model.Item{remote}, remoteSettings))
	defer srv.Close()

	local := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")
	captured := textItemAt("16fd2706-8baf-433b-82eb-8c7fada847da", "captured while flushing")
	h := &unitHistory{items: []model.Item{local}}
	s := signedIn(model.DefaultSettings())
	s.MaxItems = 50
	syn := unitSyncer(t, srv, s, h)
	failed := false
	syn.cfg.Flush = func(context.Context) error {
		if failed {
			return nil
		}
		failed = true
		h.items = append([]model.Item{captured}, h.items...)
		return errors.New("disk full")
	}

	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); err == nil {
		t.Fatal("a failed flush stopped nothing")
	}
	var ids []string
	for _, it := range h.items {
		ids = append(ids, it.ID)
	}
	if len(ids) != 2 {
		t.Fatalf("history %v: the capture made during the flush was lost", ids)
	}
	have := map[string]bool{ids[0]: true, ids[1]: true}
	if !have[captured.ID] || !have[local.ID] {
		t.Errorf("history %v", ids)
	}
	if s.MaxItems != 50 {
		t.Errorf("MaxItems %d: the remote settings were not rolled back", s.MaxItems)
	}
}

// If the upload after a download-time merge fails, the pending state was
// recorded, so the next run uploads the merged history instead of treating it
// as the downloaded snapshot.
func TestDownloadMergeRecordsPendingStateBeforeUpload(t *testing.T) {
	remote := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "remote")
	data := snapshotFile(t, []model.Item{remote})
	tmp := filepath.Join(t.TempDir(), "remote.sqlite")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, err := snapshot.FileSHA256(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var failUpload atomic.Bool
	failUpload.Store(true)
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sync/manifest":
			io.WriteString(w, `{"schema":1,"latest":{"version":"v1","updatedAt":"2026-05-30T12:00:00+00:00","snapshot":{"sha256":"`+sha+`","size":1}}}`)
		case r.URL.Path == "/sync/snapshot" && r.Method == http.MethodGet:
			w.Write(data)
		case r.URL.Path == "/sync/snapshot" && r.Method == http.MethodPut:
			puts.Add(1)
			if failUpload.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			io.WriteString(w, `{"version":"v2","sha256":"uploaded","size":1}`)
		case r.URL.Path == "/sync/blobs/check":
			io.WriteString(w, `{"missing":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	captured := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "captured during the download")
	h := &downloadArrivalHistory{current: []model.Item{captured}}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	if _, err := syn.Run(context.Background(), nil, "test"); err == nil {
		t.Fatal("the failed upload stopped nothing")
	}
	if got := model.Str(s.SyncLastRemoteVersion); got != "v1" {
		t.Errorf("version %q: the pending merge state was not recorded", got)
	}
	if got := model.Str(s.SyncLastLocalFingerprint); got == Fingerprint(h.items) {
		t.Error("the unpublished merge was recorded as synchronized")
	}

	failUpload.Store(false)
	res, err := syn.Run(context.Background(), nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if (res.Decision.Reason != "local_changed_only" && res.Decision.Reason != "pending_upload_remote_changed") || puts.Load() != 2 {
		t.Errorf("retry: %+v, uploads %d", res.Decision, puts.Load())
	}
}

// The FilePaths of history migrated from C# can point at the user's
// documents: cleanup must never pass those to RemoveFiles.
func TestRemoveFilesOnlyGetsKlipPayloads(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	doc := filepath.Join(t.TempDir(), "my-document.pdf")
	if err := os.WriteFile(doc, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}
	local := model.Item{
		ID: "16fd2706-8baf-433b-82eb-8c7fada847da", Kind: model.KindFileList,
		CapturedAt: model.Time{Time: time.Now()}, ApproxBytes: 9, FilePaths: []string{doc}, CopyCount: 1,
	}
	cacheDir := filepath.Join(l.Files(), strings.ReplaceAll(local.ID, "-", ""))
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cacheFile := filepath.Join(cacheDir, "0000_doc.pdf")
	if err := os.WriteFile(cacheFile, []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "remote")}))
	defer srv.Close()
	h := &cleaningHistory{unitHistory: unitHistory{items: []model.Item{local}}}
	s := signedIn(model.DefaultSettings())
	syn := New(Config{
		Flush:    func(context.Context) error { return nil },
		Layout:   l,
		Settings: syncclient.Settings{Get: func() model.Settings { return *s }, Update: func(f func(*model.Settings)) error { f(s); return nil }},
		History:  h,
		Client:   syncclient.New(srv.URL),
		BaseURL:  srv.URL,
		Now:      time.Now,
	})
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(doc); err != nil {
		t.Fatal("a file outside Klip's folders was deleted")
	}
	foundCache := false
	for _, p := range h.removed {
		if filepath.Clean(p) == filepath.Clean(doc) {
			t.Errorf("RemoveFiles was given the user's document: %s", p)
		}
		if filepath.Clean(p) == filepath.Clean(cacheFile) {
			foundCache = true
		}
	}
	if !foundCache {
		t.Errorf("the owned cache of the migrated item was not removed: %v", h.removed)
	}
}

// switchOnCommitHistory changes the signed-in account between f returning and
// the new history being stored, the window a real sign-in can land in.
type switchOnCommitHistory struct {
	unitHistory
	onCommit func()
}

func (h *switchOnCommitHistory) Mutate(ctx context.Context, f func([]model.Item) ([]model.Item, error)) error {
	next, err := f(append([]model.Item(nil), h.items...))
	if err != nil {
		return err
	}
	if h.onCommit != nil {
		h.onCommit()
		h.onCommit = nil
	}
	h.items = append([]model.Item(nil), next...)
	return nil
}

// A sign-in that lands during the history commit must not leave the old
// account's downloaded items in the new session.
func TestDownloadDiscardsItemsWhenTheSessionChangesAtCommit(t *testing.T) {
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "account a data")}))
	defer srv.Close()

	var mu sync.Mutex
	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr(testJWT("account-a"))
	s.AuthRefreshToken = model.Ptr("ra")
	s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	local := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")
	h := &switchOnCommitHistory{unitHistory: unitHistory{items: []model.Item{local}}}
	h.onCommit = func() {
		mu.Lock()
		s.AuthAccessToken = model.Ptr(testJWT("account-b"))
		s.AuthRefreshToken = model.Ptr("rb")
		mu.Unlock()
	}
	syn := New(Config{
		Flush:  func(context.Context) error { return nil },
		Layout: paths.Layout{Root: t.TempDir()},
		Settings: syncclient.Settings{
			Get:    func() model.Settings { mu.Lock(); defer mu.Unlock(); return s },
			Update: func(f func(*model.Settings)) error { mu.Lock(); defer mu.Unlock(); f(&s); return nil },
		},
		History: h,
		Client:  syncclient.New(srv.URL),
		BaseURL: srv.URL,
		Now:     time.Now,
	})

	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("a sign-in during the commit: %v", err)
	}
	if len(h.items) != 0 {
		t.Errorf("the old account's items stayed in the new session: %v", h.items)
	}
}

// The local sync state belongs to an account: after a sign-in to another one
// nothing is sent until the app clears that state, so a snapshot of the old
// account can never be published under the new one.
func TestSyncRefusesAnotherAccountsState(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case r.URL.Path == "/sync/manifest":
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut && r.URL.Path == "/sync/snapshot":
			io.WriteString(w, `{"version":"v1","sha256":"sha-1","size":1}`)
		case r.URL.Path == "/sync/blobs/check":
			io.WriteString(w, `{"missing":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr(testJWT("account-a"))
	s.AuthRefreshToken = model.Ptr("ra")
	s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	syn := New(Config{
		Flush:    func(context.Context) error { return nil },
		Layout:   l,
		Settings: syncclient.Settings{Get: func() model.Settings { return s }, Update: func(f func(*model.Settings)) error { f(&s); return nil }},
		History:  &unitHistory{items: []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "account a")}},
		Client:   syncclient.New(srv.URL),
		BaseURL:  srv.URL,
		Now:      time.Now,
	})
	if err := syn.bindAccount("account-a"); err != nil {
		t.Fatal(err)
	}
	// The user signs into another account.
	s.AuthAccessToken = model.Ptr(testJWT("account-b"))
	s.AuthRefreshToken = model.Ptr("rb")

	if _, err := syn.Run(context.Background(), nil, "test"); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("sync with another account's state: %v", err)
	}
	if _, err := syn.Upload(context.Background(), nil, "test"); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("upload with another account's state: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the server was contacted %d times with the new account's token", n)
	}

	// The app clears the state on sign-in: sync works again.
	if err := os.Remove(filepath.Join(l.Root, "sync", "account")); err != nil {
		t.Fatal(err)
	}
	if _, err := syn.Run(context.Background(), nil, "test"); err != nil {
		t.Fatal(err)
	}
	if got := syn.boundAccount(); got != "account-b" {
		t.Errorf("bound account %q", got)
	}
}

// A sign-in during the final flush must not leave the old account's downloaded
// items saved for the new session.
func TestDownloadDiscardsItemsWhenTheSessionChangesDuringFlush(t *testing.T) {
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "account a data")}))
	defer srv.Close()

	var mu sync.Mutex
	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr(testJWT("account-a"))
	s.AuthRefreshToken = model.Ptr("ra")
	s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	h := &unitHistory{items: []model.Item{textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")}}
	l := paths.Layout{Root: t.TempDir()}
	syn := New(Config{
		Flush:  func(context.Context) error { return nil },
		Layout: l,
		Settings: syncclient.Settings{
			Get:    func() model.Settings { mu.Lock(); defer mu.Unlock(); return s },
			Update: func(f func(*model.Settings)) error { mu.Lock(); defer mu.Unlock(); f(&s); return nil },
		},
		History: h,
		Client:  syncclient.New(srv.URL),
		BaseURL: srv.URL,
		Now:     time.Now,
	})
	syn.cfg.Flush = func(context.Context) error {
		mu.Lock()
		s.AuthAccessToken = model.Ptr(testJWT("account-b"))
		s.AuthRefreshToken = model.Ptr("rb")
		mu.Unlock()
		return nil
	}

	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("a sign-in during the flush: %v", err)
	}
	if len(h.items) != 0 {
		t.Errorf("the old account's items were saved for the new session: %v", h.items)
	}
	if _, err := os.Stat(filepath.Join(l.Root, "sync", "pending-merge.json")); !os.IsNotExist(err) {
		t.Errorf("the pending record of the discarded download survived: %v", err)
	}
}

// switchOnReadHistory changes the account while the history is being read for
// an upload.
type switchOnReadHistory struct {
	unitHistory
	onRead func()
}

func (h *switchOnReadHistory) Items() []model.Item {
	items := h.unitHistory.Items()
	if h.onRead != nil {
		h.onRead()
		h.onRead = nil
	}
	return items
}

// The items read for an upload belong to the session that started it: a
// sign-in during the read must abort before anything is sent.
func TestUploadStopsWhenTheSessionChangesWhileReadingHistory(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var mu sync.Mutex
	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr(testJWT("account-a"))
	s.AuthRefreshToken = model.Ptr("ra")
	s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	h := &switchOnReadHistory{unitHistory: unitHistory{items: []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "private")}}}
	h.onRead = func() {
		mu.Lock()
		s.AuthAccessToken = model.Ptr(testJWT("account-b"))
		s.AuthRefreshToken = model.Ptr("rb")
		mu.Unlock()
	}
	syn := New(Config{
		Flush:  func(context.Context) error { return nil },
		Layout: paths.Layout{Root: t.TempDir()},
		Settings: syncclient.Settings{
			Get:    func() model.Settings { mu.Lock(); defer mu.Unlock(); return s },
			Update: func(f func(*model.Settings)) error { mu.Lock(); defer mu.Unlock(); f(&s); return nil },
		},
		History: h,
		Client:  syncclient.New(srv.URL),
		BaseURL: srv.URL,
		Now:     time.Now,
	})

	if _, err := syn.Upload(context.Background(), nil, "test"); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("a sign-in while the history was read: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("the server was contacted %d times for another account's history", n)
	}
}

// A first sync that fails after applying the remote snapshot has still put
// that account's data into the local history: the binding must be recorded
// before the operation, so a later sign-in to another account cannot upload it.
func TestFailedFirstSyncStillBindsTheAccount(t *testing.T) {
	remote := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "account a data")
	data := snapshotFile(t, []model.Item{remote})
	tmp := filepath.Join(t.TempDir(), "remote.sqlite")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, err := snapshot.FileSHA256(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case r.URL.Path == "/sync/manifest":
			io.WriteString(w, `{"schema":1,"latest":{"version":"v1","updatedAt":"2026-05-30T12:00:00+00:00","snapshot":{"sha256":"`+sha+`","size":1}}}`)
		case r.URL.Path == "/sync/snapshot" && r.Method == http.MethodGet:
			w.Write(data)
		case r.URL.Path == "/sync/snapshot" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusInternalServerError)
		case r.URL.Path == "/sync/blobs/check":
			io.WriteString(w, `{"missing":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr(testJWT("account-a"))
	s.AuthRefreshToken = model.Ptr("ra")
	s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	syn := New(Config{
		Flush:    func(context.Context) error { return nil },
		Layout:   paths.Layout{Root: t.TempDir()},
		Settings: syncclient.Settings{Get: func() model.Settings { return s }, Update: func(f func(*model.Settings)) error { f(&s); return nil }},
		History:  &unitHistory{items: []model.Item{textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")}},
		Client:   syncclient.New(srv.URL),
		BaseURL:  srv.URL,
		Now:      time.Now,
	})
	if _, err := syn.Run(context.Background(), nil, "test"); err == nil {
		t.Fatal("the failed upload stopped nothing")
	}
	if got := syn.boundAccount(); got != "account-a" {
		t.Fatalf("bound account %q: the applied state was not bound", got)
	}

	// Another account signs in: no request goes out, the local state stays A's.
	s.AuthAccessToken = model.Ptr(testJWT("account-b"))
	s.AuthRefreshToken = model.Ptr("rb")
	before := requests.Load()
	if _, err := syn.Run(context.Background(), nil, "test"); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("sync with another account's state: %v", err)
	}
	if n := requests.Load(); n != before {
		t.Errorf("the server was contacted %d more times with the new account", n-before)
	}
}

// A session that only has a refresh token learns its account from the first
// refreshed token; the binding must be recorded then, before any data is
// applied.
func TestRefreshOnlySessionBindsTheAccount(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/auth/refresh":
			io.WriteString(w, `{"accessToken":"`+testJWT("account-a")+`","refreshToken":"ra2","accessTokenExpiresAt":"2030-01-01T00:00:00+00:00"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := model.DefaultSettings()
	s.AuthRefreshToken = model.Ptr("ra")
	s.AuthExpiresAtUnixMs = 0
	syn := New(Config{
		Flush:    func(context.Context) error { return nil },
		Layout:   paths.Layout{Root: t.TempDir()},
		Settings: syncclient.Settings{Get: func() model.Settings { return s }, Update: func(f func(*model.Settings)) error { f(&s); return nil }},
		History:  &unitHistory{},
		Client:   syncclient.New(srv.URL),
		BaseURL:  srv.URL,
		Now:      time.Now,
	})
	if _, err := syn.Run(context.Background(), nil, "test"); err != nil {
		t.Fatal(err)
	}
	if got := syn.boundAccount(); got != "account-a" {
		t.Fatalf("bound account %q: a refresh-only session was not bound", got)
	}

	s.AuthAccessToken = model.Ptr(testJWT("account-b"))
	s.AuthRefreshToken = model.Ptr("rb")
	s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	before := requests.Load()
	if _, err := syn.Run(context.Background(), nil, "test"); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("sync after switching accounts: %v", err)
	}
	if n := requests.Load(); n != before {
		t.Errorf("the server was contacted %d more times", n-before)
	}
}

// A cancellation during Flush must not stop the rollback: it runs on its own
// bounded context.
func TestRollbackRunsWhenTheContextIsCancelled(t *testing.T) {
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "remote")}))
	defer srv.Close()
	local := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")
	h := &unitHistory{items: []model.Item{local}}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	ctx, cancel := context.WithCancel(context.Background())
	syn.cfg.Flush = func(context.Context) error {
		cancel()
		return context.Canceled
	}
	if _, err := syn.DownloadAndApply(ctx, nil, "test", "", nil); err == nil {
		t.Fatal("the failed flush stopped nothing")
	}
	if len(h.items) != 1 || h.items[0].ID != local.ID {
		t.Errorf("the history was not rolled back after cancellation: %+v", h.items)
	}
}

// An edit to an item that exists on both sides survives the rollback.
func TestRollbackKeepsEditsToExistingItems(t *testing.T) {
	const id = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	remote := textItemAt(id, "remote")
	local := textItemAt(id, "local")
	local.Note = model.Ptr("old note")
	local.CopyCount = 1
	h := &unitHistory{items: []model.Item{local}}
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{remote}))
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	syn.cfg.Flush = func(context.Context) error {
		// The user edits the item while the history is being saved.
		h.items[0].Note = model.Ptr("edited")
		h.items[0].CopyCount = 7
		return errors.New("disk full")
	}
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); err == nil {
		t.Fatal("the failed flush stopped nothing")
	}
	if len(h.items) != 1 {
		t.Fatalf("history %+v", h.items)
	}
	got := h.items[0]
	if got.ID != id || model.Str(got.Text) != "local" || model.Str(got.Note) != "edited" || got.CopyCount != 7 {
		t.Errorf("rollback lost the edit or restored the wrong item: %+v", got)
	}
}

// A restore that merged a local capture leaves a pending upload, not a
// synchronized state.
func TestPersistAfterDownloadKeepsAMergedRestorePending(t *testing.T) {
	srv := serveManifest(t, "v1", "aaa", "2026-05-30T12:00:00+00:00")
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, &unitHistory{})
	if err := syn.PersistAfterDownload(context.Background(), DownloadResult{
		Fingerprint: "merged", PreMergeFingerprint: "before-merge", SnapshotSHA256: "aaa", Merged: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := model.Str(s.SyncLastLocalFingerprint); got != "pending-upload" {
		t.Errorf("fingerprint %q: the merged restore was recorded as synchronized", got)
	}
}

// A failed download must not copy the downloaded note/copy count over the
// local item when the user did not edit it meanwhile.
func TestRollbackRestoresMetadataThatWasNotEdited(t *testing.T) {
	const id = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	remote := textItemAt(id, "remote")
	remote.Note, remote.CopyCount = model.Ptr("remote note"), 9
	local := textItemAt(id, "local")
	local.Note, local.CopyCount = model.Ptr("local note"), 1
	h := &unitHistory{items: []model.Item{local}}
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{remote}))
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	syn.cfg.Flush = func(context.Context) error { return errors.New("disk full") }
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); err == nil {
		t.Fatal("the failed flush stopped nothing")
	}
	if len(h.items) != 1 {
		t.Fatalf("history %+v", h.items)
	}
	got := h.items[0]
	if model.Str(got.Note) != "local note" || got.CopyCount != 1 || model.Str(got.Text) != "local" {
		t.Errorf("the failed download changed the local metadata: %+v", got)
	}
}

// A history that does not merge duplicates keeps them when a failed
// operation is rolled back.
func TestRollbackKeepsDuplicateItems(t *testing.T) {
	hash := "same-content"
	a := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "same")
	b := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "same")
	a.ContentHash, b.ContentHash = &hash, &hash
	b.CopyCount = 4
	h := &unitHistory{items: []model.Item{a, b}}
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{textItemAt("16fd2706-8baf-433b-82eb-8c7fada847da", "remote")}))
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	syn.cfg.Flush = func(context.Context) error { return errors.New("disk full") }
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); err == nil {
		t.Fatal("the failed flush stopped nothing")
	}
	if len(h.items) != 2 {
		t.Fatalf("the duplicates were folded during rollback: %+v", h.items)
	}
	counts := map[int32]bool{h.items[0].CopyCount: true, h.items[1].CopyCount: true}
	if !counts[1] || !counts[4] {
		t.Errorf("copy counts changed: %+v", h.items)
	}
}

// The session-change cleanup runs on its own context: a cancelled sync must
// still remove the items it applied.
func TestSessionCleanupSurvivesCancelledFlush(t *testing.T) {
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "account a data")}))
	defer srv.Close()
	var mu sync.Mutex
	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr(testJWT("account-a"))
	s.AuthRefreshToken = model.Ptr("ra")
	s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	h := &unitHistory{items: []model.Item{textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")}}
	syn := New(Config{
		Flush:  func(context.Context) error { return nil },
		Layout: paths.Layout{Root: t.TempDir()},
		Settings: syncclient.Settings{
			Get:    func() model.Settings { mu.Lock(); defer mu.Unlock(); return s },
			Update: func(f func(*model.Settings)) error { mu.Lock(); defer mu.Unlock(); f(&s); return nil },
		},
		History: h,
		Client:  syncclient.New(srv.URL),
		BaseURL: srv.URL,
		Now:     time.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	first := true
	syn.cfg.Flush = func(context.Context) error {
		if !first {
			return nil // the cleanup save
		}
		first = false
		cancel()
		mu.Lock()
		s.AuthAccessToken = model.Ptr(testJWT("account-b"))
		s.AuthRefreshToken = model.Ptr("rb")
		mu.Unlock()
		return context.Canceled
	}
	if _, err := syn.DownloadAndApply(ctx, nil, "test", "", nil); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("a cancelled flush with a session change: %v", err)
	}
	if len(h.items) != 0 {
		t.Errorf("the applied items were left behind: %+v", h.items)
	}
}

func TestDownloadStateUsesAFreshManifestOrTheHash(t *testing.T) {
	var sha atomic.Value
	sha.Store("sha-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"latest":{"version":"v9","updatedAt":"2026-05-30T12:00:00+00:00","snapshot":{"sha256":"`+sha.Load().(string)+`","size":1}}}`)
	}))
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, &unitHistory{})
	dr := DownloadResult{SnapshotSHA256: "sha-1"}
	basis := ManifestInfo{Version: "v1", SnapshotSHA256: "other"}

	if got := syn.downloadState(context.Background(), dr, basis); got.Version != "v9" {
		t.Errorf("version %q: the manifest for the downloaded file was not used", got.Version)
	}
	sha.Store("other2")
	if got := syn.downloadState(context.Background(), dr, basis); got.Version != "sha-1" {
		t.Errorf("version %q: the hash marker was not used", got.Version)
	}
}

// The cleanup of a session change is saved even when the sync context was
// cancelled during the flush that preceded it.
func TestSessionCleanupIsSavedAfterACancelledFlush(t *testing.T) {
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "account a data")}))
	defer srv.Close()
	var mu sync.Mutex
	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr(testJWT("account-a"))
	s.AuthRefreshToken = model.Ptr("ra")
	s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	h := &unitHistory{items: []model.Item{textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")}}
	syn := New(Config{
		Flush:  func(context.Context) error { return nil },
		Layout: paths.Layout{Root: t.TempDir()},
		Settings: syncclient.Settings{
			Get:    func() model.Settings { mu.Lock(); defer mu.Unlock(); return s },
			Update: func(f func(*model.Settings)) error { mu.Lock(); defer mu.Unlock(); f(&s); return nil },
		},
		History: h,
		Client:  syncclient.New(srv.URL),
		BaseURL: srv.URL,
		Now:     time.Now,
	})
	ctx, cancel := context.WithCancel(context.Background())
	var muFlush sync.Mutex
	flushes := 0
	syn.cfg.Flush = func(fctx context.Context) error {
		if err := fctx.Err(); err != nil {
			return err
		}
		muFlush.Lock()
		flushes++
		first := flushes == 1
		muFlush.Unlock()
		if first {
			cancel()
			mu.Lock()
			s.AuthAccessToken = model.Ptr(testJWT("account-b"))
			s.AuthRefreshToken = model.Ptr("rb")
			mu.Unlock()
		}
		return nil
	}
	if _, err := syn.DownloadAndApply(ctx, nil, "test", "", nil); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("a cancelled flush with a session change: %v", err)
	}
	muFlush.Lock()
	n := flushes
	muFlush.Unlock()
	if n < 2 {
		t.Errorf("flush calls %d: the session cleanup was not saved", n)
	}
	if len(h.items) != 0 {
		t.Errorf("the applied items were left behind: %+v", h.items)
	}
}

// A pending fingerprint must never equal the unpublished history it is meant
// to differ from.
func TestPendingFingerprintAlwaysDiffers(t *testing.T) {
	item := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "one")
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, serveSnapshot(t, snapshotFile(t, nil)), s, &unitHistory{items: []model.Item{item}})
	syn.persistPendingMerge(Fingerprint([]model.Item{item}), ManifestInfo{Version: "v1"}, "")
	if got := model.Str(s.SyncLastLocalFingerprint); got == Fingerprint([]model.Item{item}) || got == "" {
		t.Errorf("pending fingerprint %q equals the unpublished history", got)
	}
}

// Without an authoritative manifest the remote update time stays unknown: a
// client clock could look like a server rollback.
func TestUploadFallbackLeavesTheRemoteTimeUnknown(t *testing.T) {
	srv := serveManifest(t, "", "", "") // every answer is a 404
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, &unitHistory{})
	if err := syn.persistAfterUpload(context.Background(), &syncclient.UploadResult{Version: "v1", SHA256: "sha-1"}, nil, "fp", ""); err != nil {
		t.Fatal(err)
	}
	if got := model.Str(s.SyncLastRemoteUpdatedAt); got != "" {
		t.Errorf("updatedAt %q: a client time was invented", got)
	}
}

// A crash between replacing a cache folder and saving the history is undone
// on the next sync: history.json is the source of truth.
func TestRecoverPayloadDirsAfterAnInterruptedReplacement(t *testing.T) {
	const id = "16fd2706-8baf-433b-82eb-8c7fada847da"
	l := paths.Layout{Root: t.TempDir()}
	dir := filepath.Join(l.Files(), strings.ReplaceAll(id, "-", ""))
	target := filepath.Join(dir, "0000_new.txt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	backup := dir + ".old-deadbeef"
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(backup, "0000_old.txt")
	if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The history still names the old file, which the new folder does not have.
	h := &unitHistory{items: []model.Item{{
		ID: id, Kind: model.KindFileList, CapturedAt: model.Time{Time: time.Now()},
		ApproxBytes: 3, FilePaths: []string{filepath.Join(dir, "0000_old.txt")}, CopyCount: 1,
	}}}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, serveSnapshot(t, snapshotFile(t, nil)), s, h)
	syn.cfg.Layout = l
	syn.recoverPayloadDirs()

	if b, err := os.ReadFile(filepath.Join(dir, "0000_old.txt")); err != nil || string(b) != "old" {
		t.Errorf("the old cache was not restored: %q %v", b, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("the unsaved replacement was kept")
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Error("the backup folder was left behind")
	}
}

// A download that merges a concurrent capture has a synchronized baseline:
// the copy count of an item both sides have is not added up again.
func TestDownloadMergeKeepsSharedCopyCounts(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	remote := textItemAt(id, "shared")
	local := textItemAt(id, "shared")
	local.CopyCount = 1
	captured := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "captured")
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{remote}))
	defer srv.Close()
	h := &downloadArrivalHistory{initial: []model.Item{local}, current: []model.Item{captured, local}}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", Fingerprint([]model.Item{local}), nil); err != nil {
		t.Fatal(err)
	}
	for _, it := range h.items {
		if it.ID == id && it.CopyCount != 1 {
			t.Errorf("copy count %d: the shared count was added up", it.CopyCount)
		}
	}
}

// A hash used as the version marker of a download still matches the server
// snapshot it names.
func TestDecideRecognizesADownloadHashMarker(t *testing.T) {
	sha := strings.Repeat("a", 64)
	manifest := ManifestInfo{Version: "v2", SnapshotSHA256: sha, UpdatedAt: tp(t, "2026-05-30T12:00:00+00:00")}
	s := model.DefaultSettings()
	s.SyncLastLocalFingerprint = model.Ptr("old-fp")
	s.SyncLastRemoteVersion = &sha
	s.SyncLastRemoteSha256 = &sha
	s.SyncLastRemoteUpdatedAt = model.Ptr("2026-05-30T12:00:00.0000000+00:00")

	d := Decide(manifest, "new-fp", 2, tp(t, "2026-05-30T13:00:00+00:00"), s)
	if d.Direction != Upload || d.Reason != "local_changed_only" {
		t.Errorf("marker with a local change: %+v", d)
	}
	s.SyncLastLocalFingerprint = model.Ptr("new-fp")
	d = Decide(manifest, "new-fp", 2, tp(t, "2026-05-30T13:00:00+00:00"), s)
	if d.Direction != None || d.Reason != "state_matched" {
		t.Errorf("marker with the same history: %+v", d)
	}
}

// One item whose ID matches but only one side has a content hash is still
// one item: the snapshot keeps one row per id.
func TestMergeItemsReconcilesSameIDWithOneMissingHash(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	hash := "H1"
	remote := model.Item{ID: id, Kind: model.KindText, CapturedAt: model.Time{Time: time.Now()}, Text: model.Ptr("a"), CopyCount: 1, ContentHash: &hash}
	local := model.Item{ID: id, Kind: model.KindText, CapturedAt: model.Time{Time: time.Now().Add(time.Second)}, Text: model.Ptr("b"), CopyCount: 1}
	out := MergeItems([]model.Item{remote}, []model.Item{local})
	if len(out) != 1 {
		t.Fatalf("merged %d items: the same id survived twice", len(out))
	}
}

// Duplicates by hash can change the retained item's ID; the local copy of
// that ID must still reconcile to one row.
func TestMergeItemsRegistersTheRetainedDuplicateID(t *testing.T) {
	const (
		idA = "0f8fad5b-d9cb-469f-a165-70867728950e"
		idB = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	)
	hash := "H"
	older := model.Item{ID: idA, Kind: model.KindText, ContentHash: &hash, Text: model.Ptr("a"), CopyCount: 1, CapturedAt: model.Time{Time: time.Now().Add(-2 * time.Second)}}
	newer := model.Item{ID: idB, Kind: model.KindText, ContentHash: &hash, Text: model.Ptr("b"), CopyCount: 1, CapturedAt: model.Time{Time: time.Now()}}
	local := model.Item{ID: idB, Kind: model.KindText, Text: model.Ptr("b"), CopyCount: 3, CapturedAt: model.Time{Time: time.Now().Add(-time.Second)}}
	out := MergeItems([]model.Item{older, newer}, []model.Item{local})
	if len(out) != 1 || out[0].ID != idB {
		t.Fatalf("merged %+v", out)
	}
}

// The copy-count reconciliation uses the same id-first identity.
func TestDownloadMergeKeepsCountsWithOneMissingHash(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	hash := "H"
	remote := textItemAt(id, "shared")
	remote.ContentHash = &hash
	remote.CopyCount = 1
	local := textItemAt(id, "shared")
	local.CopyCount = 7
	captured := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "captured")
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{remote}))
	defer srv.Close()
	h := &downloadArrivalHistory{initial: []model.Item{local}, current: []model.Item{captured, local}}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", Fingerprint([]model.Item{local}), nil); err != nil {
		t.Fatal(err)
	}
	for _, it := range h.items {
		if it.ID == id && it.CopyCount != 7 {
			t.Errorf("copy count %d: the local count was reset", it.CopyCount)
		}
	}
}

// A first-sync merge records the pending upload before it is attempted.
func TestMergeRecordsThePendingUpload(t *testing.T) {
	remote := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "remote")
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{remote}))
	defer srv.Close()
	h := &mergeHistory{local: []model.Item{textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")}}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	if err := syn.MergeAndApply(context.Background(), ManifestInfo{Version: "v1"}, "pre-merge", nil, "test"); err != nil {
		t.Fatal(err)
	}
	if got := model.Str(s.SyncLastLocalFingerprint); got != "pending-upload" {
		t.Errorf("fingerprint %q: the pending upload was not recorded", got)
	}
}

// The merge joins an id group and a hash group that the same item connects.
func TestMergeItemsUnifiesIDAndHashGroups(t *testing.T) {
	const (
		a = "0f8fad5b-d9cb-469f-a165-70867728950e"
		b = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	)
	hash := "H"
	remote := []model.Item{
		{ID: a, Kind: model.KindText, Text: model.Ptr("a"), CopyCount: 1, CapturedAt: model.Time{Time: time.Now().Add(-3 * time.Second)}},
		{ID: b, Kind: model.KindText, Text: model.Ptr("b"), CopyCount: 1, ContentHash: &hash, CapturedAt: model.Time{Time: time.Now().Add(-2 * time.Second)}},
	}
	local := []model.Item{{
		ID: a, Kind: model.KindText, Text: model.Ptr("a2"), CopyCount: 2, ContentHash: &hash,
		CapturedAt: model.Time{Time: time.Now()},
	}}
	out := MergeItems(remote, local)
	if len(out) != 1 {
		t.Fatalf("merged %+v: the id and hash groups stayed apart", out)
	}
}

// The copy-count reconciliation also covers hash-matched items with
// different ids.
func TestDownloadMergeCountsAcrossIDsByHash(t *testing.T) {
	hash := "H"
	remote := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "same")
	remote.ContentHash, remote.CopyCount = &hash, 1
	local := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "same")
	local.ContentHash, local.CopyCount = &hash, 7
	out := MergeForDownload([]model.Item{remote}, []model.Item{local}, nil)
	if len(out) != 1 || out[0].CopyCount != 7 {
		t.Errorf("merged %+v: the local count was lost", out)
	}
}

// A hash group reached through a local item must stay aliased to the id
// group, so a later item with the same hash joins it too.
func TestMergeItemsKeepsHashAliases(t *testing.T) {
	const (
		a = "0f8fad5b-d9cb-469f-a165-70867728950e"
		b = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	)
	hash := "H"
	remote := []model.Item{{
		ID: a, Kind: model.KindText, Text: model.Ptr("a"), CopyCount: 1,
		CapturedAt: model.Time{Time: time.Now().Add(-3 * time.Second)},
	}}
	local := []model.Item{
		{ID: a, Kind: model.KindText, Text: model.Ptr("a"), CopyCount: 1, ContentHash: &hash, CapturedAt: model.Time{Time: time.Now()}},
		{ID: b, Kind: model.KindText, Text: model.Ptr("b"), CopyCount: 1, ContentHash: &hash, CapturedAt: model.Time{Time: time.Now().Add(-time.Second)}},
	}
	out := MergeItems(remote, local)
	if len(out) != 1 {
		t.Fatalf("merged %d items: the hash alias was lost", len(out))
	}
}

// Folding two remote groups must not let the folded remote note win over a
// local note.
func TestMergeItemsKeepsNoteProvenance(t *testing.T) {
	const (
		a = "0f8fad5b-d9cb-469f-a165-70867728950e"
		b = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	)
	hash := "H"
	remote := []model.Item{
		{ID: a, Kind: model.KindText, Text: model.Ptr("a"), CopyCount: 1, Note: model.Ptr("remote a"), CapturedAt: model.Time{Time: time.Now().Add(-3 * time.Second)}},
		{ID: b, Kind: model.KindText, Text: model.Ptr("b"), CopyCount: 1, Note: model.Ptr("remote b"), ContentHash: &hash, CapturedAt: model.Time{Time: time.Now().Add(-2 * time.Second)}},
	}
	local := []model.Item{{
		ID: a, Kind: model.KindText, Text: model.Ptr("a"), CopyCount: 1, Note: model.Ptr("local a"),
		ContentHash: &hash, CapturedAt: model.Time{Time: time.Now()},
	}}
	out := MergeItems(remote, local)
	if len(out) != 1 {
		t.Fatalf("merged %+v", out)
	}
	if got := model.Str(out[0].Note); got != "local a" {
		t.Errorf("note %q: a remote note overwrote the local one", got)
	}
}

// A deletion made while the flush was failing is not undone by the rollback.
func TestRollbackKeepsAConcurrentDeletion(t *testing.T) {
	const id = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	local := textItemAt(id, "local")
	remote := textItemAt(id, "remote")
	h := &unitHistory{items: []model.Item{local}}
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{remote}))
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	syn.cfg.Flush = func(context.Context) error {
		h.items = nil // the user deleted the item
		return errors.New("disk full")
	}
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); err == nil {
		t.Fatal("the failed flush stopped nothing")
	}
	if len(h.items) != 0 {
		t.Errorf("the deleted item was resurrected: %+v", h.items)
	}
}

// Items the limits pruned are not user deletions: a failed flush restores
// them with the old history.
type keepOnlyHistory struct {
	unitHistory
	keep  string
	armed bool
}

func (h *keepOnlyHistory) ApplyLimitsAndSnapshot(s model.Settings) ([]model.Item, []string) {
	before := idsOf(h.Items())
	h.ApplyLimits(s)
	after := h.Items()
	return after, removedIDs(before, idsOf(after))
}

func (h *keepOnlyHistory) ApplyLimits(model.Settings) {
	if h.armed {
		return // the rollback restores the old history; do not prune again
	}
	h.armed = true
	var kept []model.Item
	for _, it := range h.items {
		if it.ID == h.keep {
			kept = append(kept, it)
		}
	}
	h.items = kept
}

func TestRollbackRestoresItemsPrunedByLimits(t *testing.T) {
	const (
		shared = "0f8fad5b-d9cb-469f-a165-70867728950e"
		other  = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	)
	before := textItemAt(shared, "shared")
	remote := []model.Item{textItemAt(shared, "shared"), textItemAt(other, "other")}
	h := &keepOnlyHistory{unitHistory: unitHistory{items: []model.Item{before}}, keep: other}
	srv := serveSnapshot(t, snapshotFile(t, remote))
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	syn.cfg.Flush = func(context.Context) error { return errors.New("disk full") }
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); err == nil {
		t.Fatal("the failed flush stopped nothing")
	}
	found := false
	for _, it := range h.items {
		if it.ID == shared {
			found = true
		}
	}
	if !found {
		t.Errorf("the limit-pruned shared item was not restored: %+v", h.items)
	}
}

// Only the cache the item actually references is kept; an older unique copy
// is cleaned up.
func TestFileCacheCleanupUsesTheActiveDir(t *testing.T) {
	const id = "16fd2706-8baf-433b-82eb-8c7fada847da"
	idn := strings.ReplaceAll(id, "-", "")
	l := paths.Layout{Root: t.TempDir()}
	oldDir := filepath.Join(l.Files(), idn+"-old")
	newDir := filepath.Join(l.Files(), idn+"-new")
	for _, dir := range []string{oldDir, newDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldFile := filepath.Join(oldDir, "0000_old.txt")
	newFile := filepath.Join(newDir, "0000_new.txt")
	if err := os.WriteFile(oldFile, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newFile, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	syn := New(Config{Flush: func(context.Context) error { return nil }, Layout: l})
	old := model.Item{ID: id, Kind: model.KindFileList, FilePaths: []string{oldFile}}
	next := model.Item{ID: id, Kind: model.KindFileList, FilePaths: []string{newFile}}
	removed := syn.unreferencedFiles([]model.Item{old}, []model.Item{next})
	if len(removed) != 1 || filepath.Clean(removed[0]) != filepath.Clean(oldFile) {
		t.Errorf("removed %v: the obsolete cache was kept", removed)
	}
}

// Deleting an item while its snapshot downloads is a local change: the
// download-time merge must not bring it back.
func TestDownloadMergeKeepsAConcurrentDeletion(t *testing.T) {
	x := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "x")
	y := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "y")
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{x, y}))
	defer srv.Close()
	h := &downloadArrivalHistory{initial: []model.Item{x}} // current is empty: X was deleted
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", Fingerprint([]model.Item{x}), []model.Item{x}); err != nil {
		t.Fatal(err)
	}
	if len(h.items) != 1 || h.items[0].ID != y.ID {
		t.Fatalf("history %+v: the deleted item came back", h.items)
	}
}

// A remote deletion is kept when the local copy was unchanged, even though a
// capture triggered a download-time merge.
func TestDownloadMergeKeepsARemoteDeletion(t *testing.T) {
	x := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "x")
	captured := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "captured")
	remote := []model.Item{textItemAt("16fd2706-8baf-433b-82eb-8c7fada847da", "y")}
	out := MergeForDownload(remote, []model.Item{captured, x}, []model.Item{x})
	for _, it := range out {
		if it.ID == x.ID {
			t.Errorf("the remotely deleted item came back: %+v", out)
		}
	}
}

// A note/pin edit made during a download beats the downloaded metadata.
func TestDownloadMergeKeepsMetadataEdits(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	base := textItemAt(id, "x")
	base.Pinned, base.Note = true, model.Ptr("note")
	local := base
	local.Pinned, local.Note = false, nil
	remote := base
	remote.Note = model.Ptr("remote note")
	out := MergeForDownload([]model.Item{remote}, []model.Item{local}, []model.Item{base})
	if len(out) != 1 {
		t.Fatalf("%+v", out)
	}
	if out[0].Pinned || out[0].Note != nil {
		t.Errorf("the download undid the edit: %+v", out[0])
	}
}

// A remote metadata edit is kept when the local item was not edited during
// the download.
func TestDownloadMergeTakesRemoteMetadata(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	base := textItemAt(id, "x")
	base.Pinned, base.Note = true, model.Ptr("note")
	local := base
	remote := base
	remote.Pinned, remote.Note = false, model.Ptr("remote note")
	out := MergeForDownload([]model.Item{remote}, []model.Item{local}, []model.Item{base})
	if len(out) != 1 {
		t.Fatalf("%+v", out)
	}
	if out[0].Pinned || model.Str(out[0].Note) != "remote note" {
		t.Errorf("the remote edit was lost: %+v", out[0])
	}
}

// A remote deletion does not win over a local metadata edit.
func TestDownloadMergeKeepsAnEditedItemDeletedRemotely(t *testing.T) {
	const (
		x = "0f8fad5b-d9cb-469f-a165-70867728950e"
		y = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	)
	base := textItemAt(x, "x")
	base.Note = model.Ptr("base note")
	local := base
	local.Note = model.Ptr("edited")
	remote := []model.Item{textItemAt(y, "y")}
	out := MergeForDownload(remote, []model.Item{local}, []model.Item{base})
	found := false
	for _, it := range out {
		if it.ID == x {
			found = true
			if model.Str(it.Note) != "edited" {
				t.Errorf("the edit was lost: %+v", it)
			}
		}
	}
	if !found {
		t.Errorf("the locally edited item was dropped: %+v", out)
	}
}

// The decision fingerprints the same slice that becomes the download baseline.
func TestDecideItemsUsesTheGivenHistory(t *testing.T) {
	live := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "live")
	baseline := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "baseline")
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, serveSnapshot(t, snapshotFile(t, nil)), s, &unitHistory{items: []model.Item{live}})
	d := syn.DecideItems(ManifestInfo{Version: "v1", SnapshotSHA256: "x", UpdatedAt: tp(t, "2026-05-30T12:00:00+00:00")}, []model.Item{baseline})
	if d.LocalFingerprint != Fingerprint([]model.Item{baseline}) {
		t.Errorf("fingerprint is not of the baseline slice")
	}
}

// The account is checked inside the settings update, not only before it.
func TestPersistSyncStateChecksTheAccountInsideTheUpdate(t *testing.T) {
	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr(testJWT("account-a"))
	syn := New(Config{
		Flush:    func(context.Context) error { return nil },
		Settings: syncclient.Settings{Get: func() model.Settings { return s }, Update: func(f func(*model.Settings)) error { f(&s); return nil }},
	})
	syn.PersistSyncState("fp", ManifestInfo{Version: "v1"}, "account-b")
	if s.SyncLastLocalFingerprint != nil || s.SyncLastRemoteVersion != nil {
		t.Errorf("another account's state was written: %+v", s)
	}
	syn.PersistSyncState("fp", ManifestInfo{Version: "v1"}, "account-a")
	if model.Str(s.SyncLastLocalFingerprint) != "fp" || model.Str(s.SyncLastRemoteVersion) != "v1" {
		t.Errorf("the current account's state was not written: %+v", s)
	}
}

// A no-op timestamp fallback does not record unequal histories as synced.
func TestRunDoesNotPersistATimestampFallback(t *testing.T) {
	at := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"latest":{"version":"v1","updatedAt":"`+at.Format(time.RFC3339)+`","snapshot":{"sha256":"rsha","size":1}}}`)
	}))
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	s.SyncLastLocalFingerprint = model.Ptr("old-fp")
	s.SyncLastRemoteVersion = model.Ptr("v0")
	s.SyncLastRemoteSha256 = model.Ptr("old-sha")
	s.SyncLastRemoteUpdatedAt = model.Ptr(at.Add(-time.Hour).Format(time.RFC3339))
	h := &unitHistory{items: []model.Item{{ID: "0f8fad5b-d9cb-469f-a165-70867728950e", Kind: model.KindText, CapturedAt: model.Time{Time: at}, Text: model.Ptr("x"), CopyCount: 1}}}
	syn := unitSyncer(t, srv, s, h)
	res, err := syn.Run(context.Background(), nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Decision.Reason != "timestamp_fallback" || res.Decision.Direction != None {
		t.Fatalf("decision %+v", res.Decision)
	}
	if got := model.Str(s.SyncLastLocalFingerprint); got != "old-fp" {
		t.Errorf("fingerprint %q: the no-op was recorded as synchronized", got)
	}
	if got := model.Str(s.SyncLastRemoteVersion); got != "v0" {
		t.Errorf("version %q", got)
	}
}

// The note a group carries keeps its provenance through folds.
func TestMergeItemsTracksTheNoteProvenance(t *testing.T) {
	const (
		a = "0f8fad5b-d9cb-469f-a165-70867728950e"
		b = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	)
	hash := "H"
	remote := []model.Item{
		{ID: a, Kind: model.KindText, Text: model.Ptr("a"), Note: model.Ptr("remote a"), CopyCount: 1, CapturedAt: model.Time{Time: time.Now().Add(-3 * time.Second)}},
		{ID: b, Kind: model.KindText, Text: model.Ptr("b"), Note: model.Ptr("remote b"), ContentHash: &hash, CopyCount: 1, CapturedAt: model.Time{Time: time.Now().Add(-2 * time.Second)}},
	}
	local := []model.Item{
		{ID: b, Kind: model.KindText, Text: model.Ptr("b"), ContentHash: &hash, CopyCount: 1, CapturedAt: model.Time{Time: time.Now().Add(-time.Second)}},
		{ID: a, Kind: model.KindText, Text: model.Ptr("a"), Note: model.Ptr("local a"), ContentHash: &hash, CopyCount: 1, CapturedAt: model.Time{Time: time.Now()}},
	}
	out := MergeItems(remote, local)
	if len(out) != 1 {
		t.Fatalf("merged %+v", out)
	}
	if got := model.Str(out[0].Note); got != "local a" {
		t.Errorf("note %q: a remote note was treated as local", got)
	}
}

// Metadata edits follow the hash identity even when the merged item keeps a
// different id.
func TestDownloadMergeMatchesMetadataByHashIdentity(t *testing.T) {
	hash := "H"
	base := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "same")
	base.ContentHash, base.Pinned, base.Note = &hash, true, model.Ptr("base")
	local := base
	local.Note, local.Pinned = nil, false
	remote := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "same")
	remote.ContentHash, remote.Pinned, remote.Note = &hash, true, model.Ptr("remote")
	remote.CapturedAt = model.Time{Time: time.Now().Add(time.Second)}
	out := MergeForDownload([]model.Item{remote}, []model.Item{local}, []model.Item{base})
	if len(out) != 1 {
		t.Fatalf("%+v", out)
	}
	if out[0].Pinned || out[0].Note != nil {
		t.Errorf("the local edit was lost: %+v", out[0])
	}
}

// A merge whose history was saved before the pending state is recovered on
// the next run instead of merging the items again.
func TestRecoverPendingMergeRecordsTheUpload(t *testing.T) {
	s := signedIn(model.DefaultSettings())
	old := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "old")
	merged := []model.Item{old, textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "new")}
	h := &unitHistory{items: merged}
	syn := unitSyncer(t, serveSnapshot(t, snapshotFile(t, nil)), s, h)
	syn.writePendingMerge(pendingMergeState{
		Fingerprint: "pending-upload",
		Version:     "v1",
		SHA256:      "sha",
		Before:      Fingerprint([]model.Item{old}),
		After:       Fingerprint(merged),
	})
	syn.recoverPendingMerge(context.Background(), "")
	if got := model.Str(s.SyncLastLocalFingerprint); got != "pending-upload" {
		t.Errorf("fingerprint %q: the pending upload was not recovered", got)
	}
	if _, err := os.Stat(syn.pendingMergePath()); !os.IsNotExist(err) {
		t.Errorf("the pending file survived: %v", err)
	}
}

// A pending file whose merged history was never saved is dropped without
// recording anything.
func TestRecoverPendingMergeIgnoresAnUnsavedHistory(t *testing.T) {
	s := signedIn(model.DefaultSettings())
	old := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "old")
	h := &unitHistory{items: []model.Item{old}}
	syn := unitSyncer(t, serveSnapshot(t, snapshotFile(t, nil)), s, h)
	syn.writePendingMerge(pendingMergeState{Fingerprint: "pending-upload", Before: Fingerprint([]model.Item{old})})
	syn.recoverPendingMerge(context.Background(), "")
	if s.SyncLastLocalFingerprint != nil {
		t.Errorf("state was recorded for a history that was never saved: %+v", s)
	}
}

// Run recovers the pending merge before it decides, so a crash after the
// flushed merge resumes the upload instead of merging again.
func TestRunRecoversAPendingMergeBeforeDeciding(t *testing.T) {
	at := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, `{"latest":{"version":"v1","updatedAt":"`+at.Format(time.RFC3339)+`","snapshot":{"sha256":"sha","size":1}}}`)
	}))
	defer srv.Close()
	old := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "old")
	merged := []model.Item{old, textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "new")}
	s := signedIn(model.DefaultSettings())
	h := &unitHistory{items: merged}
	syn := unitSyncer(t, srv, s, h)
	syn.writePendingMerge(pendingMergeState{
		Fingerprint: "pending-upload",
		Version:     "v1",
		SHA256:      "sha",
		Before:      Fingerprint([]model.Item{old}),
		After:       Fingerprint(merged),
	})
	res, _ := syn.Run(context.Background(), nil, "test")
	if res.Decision.Direction != Upload {
		t.Errorf("decision %+v: the pending merge was not recovered before deciding", res.Decision)
	}
	if got := model.Str(s.SyncLastLocalFingerprint); got != "pending-upload" {
		t.Errorf("fingerprint %q", got)
	}
}

// A signed-out settings update is not written even when the workflow account
// is known.
func TestPersistSyncStateRejectsASignedOutSession(t *testing.T) {
	s := model.DefaultSettings()
	syn := New(Config{
		Flush:    func(context.Context) error { return nil },
		Settings: syncclient.Settings{Get: func() model.Settings { return s }, Update: func(f func(*model.Settings)) error { f(&s); return nil }},
	})
	syn.PersistSyncState("fp", ManifestInfo{Version: "v1"}, "account-a")
	if s.SyncLastLocalFingerprint != nil {
		t.Errorf("state was written into a signed-out session: %+v", s)
	}
}

// The megabytes are rounded up without overflowing near MaxInt64.
func TestEnsureSettingsCanHoldItemsRoundsUpWithoutOverflow(t *testing.T) {
	s := model.DefaultSettings()
	s.MaxMegabytes = 1
	EnsureSettingsCanHoldItems(&s, []model.Item{{ApproxBytes: math.MaxInt64}})
	if s.MaxMegabytes <= 1 {
		t.Errorf("MaxMegabytes %d: the overflow disabled the expansion", s.MaxMegabytes)
	}
}

// A capture that was never flushed makes the persisted history different from
// Before; that must not be taken as proof that the merge was saved.
func TestRecoverPendingMergeDoesNotTrustAnUnflushedCapture(t *testing.T) {
	s := signedIn(model.DefaultSettings())
	old := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "old")
	captured := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "captured")
	remote := textItemAt("16fd2706-8baf-433b-82eb-8c7fada847da", "remote")
	h := &unitHistory{items: []model.Item{old}} // the disk still has only H0
	syn := unitSyncer(t, serveSnapshot(t, snapshotFile(t, nil)), s, h)
	before := []model.Item{old, captured} // the unflushed capture
	after := []model.Item{old, captured, remote}
	syn.writePendingMerge(pendingMergeState{
		Fingerprint: "pending-upload",
		Version:     "v1",
		SHA256:      "sha",
		Before:      Fingerprint(before),
		After:       Fingerprint(after),
	})
	syn.recoverPendingMerge(context.Background(), "")
	if s.SyncLastLocalFingerprint != nil {
		t.Errorf("state was recovered for a merge that was never saved: %+v", s)
	}
	if _, err := os.Stat(syn.pendingMergePath()); !os.IsNotExist(err) {
		t.Errorf("the pending file survived: %v", err)
	}
}

// The committed marker is proof even if captures changed the history since.
func TestRecoverPendingMergeUsesTheCommittedMarker(t *testing.T) {
	s := signedIn(model.DefaultSettings())
	old := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "old")
	h := &unitHistory{items: []model.Item{old}} // changed by a capture after the flush
	syn := unitSyncer(t, serveSnapshot(t, snapshotFile(t, nil)), s, h)
	syn.writePendingMerge(pendingMergeState{
		Fingerprint: "pending-upload",
		Before:      Fingerprint([]model.Item{old}),
		After:       "something-else",
		Committed:   true,
	})
	syn.recoverPendingMerge(context.Background(), "")
	if got := model.Str(s.SyncLastLocalFingerprint); got != "pending-upload" {
		t.Errorf("fingerprint %q: the committed marker was not trusted", got)
	}
}

// A metadata-only merge has the same ids before and after; ID presence alone
// is not proof that the merge was saved.
func TestRecoverPendingMergeRequiresProofForSameIDs(t *testing.T) {
	s := signedIn(model.DefaultSettings())
	old := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "old")
	h := &unitHistory{items: []model.Item{old}}
	syn := unitSyncer(t, serveSnapshot(t, snapshotFile(t, nil)), s, h)
	merged := []model.Item{old}
	merged[0].Note = model.Ptr("merged note")
	syn.writePendingMerge(pendingMergeState{
		Fingerprint: "pending-upload",
		Before:      Fingerprint([]model.Item{old}),
		After:       Fingerprint(merged),
	})
	syn.recoverPendingMerge(context.Background(), "")
	if s.SyncLastLocalFingerprint != nil {
		t.Errorf("state was recovered for an unsaved metadata-only merge: %+v", s)
	}
}

// A deleted duplicate must not contribute its count or metadata to the
// surviving one.
func TestDownloadMergeIgnoresDeletedDuplicateMetadata(t *testing.T) {
	const (
		a = "0f8fad5b-d9cb-469f-a165-70867728950e"
		b = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	)
	hash := "H"
	baseA := textItemAt(a, "same")
	baseA.ContentHash, baseA.CopyCount = &hash, 100
	baseA.Note = model.Ptr("deleted note")
	baseB := textItemAt(b, "same")
	baseB.ContentHash, baseB.CopyCount = &hash, 1
	remote := []model.Item{baseA, baseB}
	local := []model.Item{baseB}
	out := MergeForDownload(remote, local, []model.Item{baseA, baseB})
	if len(out) != 1 {
		t.Fatalf("%+v", out)
	}
	if out[0].CopyCount != 1 || model.Str(out[0].Note) != "" {
		t.Errorf("the deleted duplicate leaked into the survivor: %+v", out[0])
	}
}

// A recapture of an item that only the failed import added is kept.
func TestRollbackKeepsARecaptureOfAnImportedItem(t *testing.T) {
	imported := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "imported")
	recaptured := imported
	recaptured.CopyCount = 2
	recaptured.CapturedAt = model.Time{Time: time.Now().Add(time.Second)}
	out := rollbackItems(nil, []model.Item{imported}, []model.Item{recaptured}, nil)
	if len(out) != 1 || out[0].ID != imported.ID || out[0].CopyCount != 2 {
		t.Errorf("the recapture was dropped: %+v", out)
	}
}

// A recaptured image keeps the payload the rollback would otherwise delete.
func TestRollbackKeepsThePayloadOfARecapturedImport(t *testing.T) {
	src := paths.Layout{Root: t.TempDir()}
	if err := os.MkdirAll(src.Images(), 0o755); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(src.Images(), "remote.png")
	if err := os.WriteFile(img, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	remote := model.Item{
		ID: "0f8fad5b-d9cb-469f-a165-70867728950e", Kind: model.KindImage,
		CapturedAt: model.Time{Time: time.Now()}, ApproxBytes: 5, ImageFilePath: &img, CopyCount: 1,
	}
	snapPath := filepath.Join(src.Root, "snapshot.sqlite")
	if _, err := snapshot.Export(context.Background(), src, snapPath, []model.Item{remote}, model.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/sync/snapshot":
			w.Write(data)
		case strings.HasPrefix(r.URL.Path, "/sync/blobs/"):
			w.Write([]byte("image"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	h := &unitHistory{}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	first := true
	syn.cfg.Flush = func(context.Context) error {
		if !first {
			return nil
		}
		first = false
		h.items[0].CopyCount = 2 // recaptured while the flush runs
		return errors.New("disk full")
	}
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); err == nil {
		t.Fatal("the failed flush stopped nothing")
	}
	if len(h.items) != 1 || h.items[0].CopyCount != 2 {
		t.Fatalf("history %+v", h.items)
	}
	p := model.Str(h.items[0].ImageFilePath)
	if _, err := os.Stat(p); err != nil {
		t.Errorf("the retained item's payload was deleted: %v", err)
	}
}

// An exact id match beats a hash match when reconciling metadata.
func TestDownloadMergePrefersTheExactID(t *testing.T) {
	const (
		a = "0f8fad5b-d9cb-469f-a165-70867728950e"
		b = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
	)
	hash := "H"
	baseA := textItemAt(a, "same")
	baseA.ContentHash, baseA.Note = &hash, model.Ptr("a-base")
	baseB := textItemAt(b, "same")
	baseB.ContentHash, baseB.Note = &hash, model.Ptr("b-base")
	local := []model.Item{baseB} // A was deleted during the download
	remoteB := baseB
	remoteB.Note = model.Ptr("b-remote")
	out := MergeForDownload([]model.Item{remoteB}, local, []model.Item{baseA, baseB})
	if len(out) != 1 || model.Str(out[0].Note) != "b-remote" {
		t.Errorf("merged %+v: the remote note update was discarded", out)
	}
}

// An ordinary (non-merged) download also leaves a recoverable baseline: a
// crash before Run saves the markers must not lead to a first-sync merge.
func TestOrdinaryDownloadLeavesARecoverableBaseline(t *testing.T) {
	remote := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "remote")
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{remote}))
	defer srv.Close()
	h := &unitHistory{}
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, h)
	dr, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(syn.pendingMergePath()); err != nil {
		t.Fatalf("no recoverable baseline was written: %v", err)
	}
	s.SyncLastLocalFingerprint = nil
	syn.recoverPendingMerge(context.Background(), "")
	if got := model.Str(s.SyncLastLocalFingerprint); got != dr.Fingerprint {
		t.Errorf("fingerprint %q, want %q", got, dr.Fingerprint)
	}
}

// A failed pending-state write stops the download and rolls the history back.
func TestDownloadFailsWhenThePendingRecordCannotBeWritten(t *testing.T) {
	remote := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "remote")
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{remote}))
	defer srv.Close()
	l := paths.Layout{Root: t.TempDir()}
	syncDir := filepath.Join(l.Root, "sync")
	if err := os.MkdirAll(filepath.Join(syncDir, "pending-merge.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := &unitHistory{}
	syn := New(Config{
		Flush:    func(context.Context) error { return nil },
		Layout:   l,
		Settings: syncclient.Settings{Get: func() model.Settings { return *signedIn(model.DefaultSettings()) }, Update: func(f func(*model.Settings)) error { ; return nil }},
		History:  h,
		Client:   syncclient.New(srv.URL),
		BaseURL:  srv.URL,
		Now:      time.Now,
	})
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); err == nil {
		t.Fatal("the download went on without its recovery record")
	}
	if len(h.items) != 0 {
		t.Errorf("the history was not rolled back: %+v", h.items)
	}
}

// The rollback is the sync's own write: it must not wake the auto-sync.
func TestRollbackSuppressesTheHistoryNotification(t *testing.T) {
	var events []bool
	s := model.DefaultSettings()
	h := &unitHistory{}
	syn := New(Config{
		Flush:            func(context.Context) error { return nil },
		Layout:           paths.Layout{Root: t.TempDir()},
		Settings:         syncclient.Settings{Get: func() model.Settings { return s }, Update: func(f func(*model.Settings)) error { f(&s); return nil }},
		History:          h,
		SuppressAutoSync: func(on bool) { events = append(events, on) },
	})
	if err := syn.rollbackMutation(context.Background(), nil, nil, &settingsUndo{old: model.DefaultSettings()}, nil); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || !events[0] || events[1] {
		t.Errorf("suppress events %v", events)
	}
}

// deletedDuringMergeHistory is a history from which items are deleted while
// the remote snapshot downloads: Items is the history when the merge begins,
// Mutate sees it without them.
type deletedDuringMergeHistory struct {
	mergeHistory
	deleted map[string]bool
}

func (h *deletedDuringMergeHistory) Mutate(_ context.Context, f func([]model.Item) ([]model.Item, error)) error {
	var current []model.Item
	for _, it := range h.local {
		if !h.deleted[it.ID] {
			current = append(current, it)
		}
	}
	next, err := f(current)
	if err != nil {
		return err
	}
	h.stored = next
	return nil
}

// What the user deletes while the first merge downloads stays deleted: the
// same item in the snapshot, and the same content under another id, are left
// out; content the user still keeps a copy of is merged as before.
func TestMergeKeepsDeletionsMadeDuringTheDownload(t *testing.T) {
	hashed := func(id, text string) model.Item {
		it := textItemAt(id, text)
		it.ContentHash = model.Ptr("text:" + text)
		return it
	}
	gone := hashed("0f8fad5b-d9cb-469f-a165-70867728950e", "deleted")
	twin := hashed("7c9e6679-7425-40de-944b-e07fc1f90ae7", "deleted")          // the same text, copied on another device
	keptA := hashed("16fd2706-8baf-433b-82eb-8c7fada847da", "kept")            // a duplicate the user keeps
	keptB := hashed("6ba7b810-9dad-11d1-80b4-00c04fd430c8", "kept")            // and the one deleted
	other := hashed("6ba7b811-9dad-11d1-80b4-00c04fd430c8", "from the server") // unrelated
	srv := serveSnapshot(t, snapshotFile(t, []model.Item{gone, twin, keptB, other}))
	defer srv.Close()
	h := &deletedDuringMergeHistory{
		mergeHistory: mergeHistory{local: []model.Item{gone, keptA, keptB}},
		deleted:      map[string]bool{gone.ID: true, keptB.ID: true},
	}
	syn := unitSyncer(t, srv, signedIn(model.DefaultSettings()), h)
	if err := syn.MergeAndApply(context.Background(), ManifestInfo{}, "", nil, "test"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range h.stored {
		got = append(got, model.Str(it.Text))
	}
	if slices.Contains(got, "deleted") {
		t.Fatalf("a deletion made during the download came back: %v", got)
	}
	if !slices.Contains(got, "kept") || !slices.Contains(got, "from the server") || len(got) != 2 {
		t.Fatalf("merged %v", got)
	}
}

// A download merge leaves out what was deleted here also when the server
// holds the same content under another id; a copy still kept here is merged
// as before.
func TestDownloadMergeKeepsADeletionUnderAnotherID(t *testing.T) {
	hashed := func(id, text string) model.Item {
		it := textItemAt(id, text)
		it.ContentHash = model.Ptr("text:" + text)
		return it
	}
	a := hashed("0f8fad5b-d9cb-469f-a165-70867728950e", "deleted")
	b := hashed("7c9e6679-7425-40de-944b-e07fc1f90ae7", "deleted") // deduplicated elsewhere under another id
	if got := MergeForDownload([]model.Item{b}, nil, []model.Item{a}); len(got) != 0 {
		t.Fatalf("the deletion came back: %v", got)
	}
	keep := hashed("16fd2706-8baf-433b-82eb-8c7fada847da", "deleted") // a copy the user keeps
	got := MergeForDownload([]model.Item{b}, []model.Item{keep}, []model.Item{a, keep})
	if len(got) != 1 || model.Str(got[0].Text) != "deleted" {
		t.Fatalf("the kept copy: %v", got)
	}
}

// Two Syncers of the same data folder share their locks: a run of one waits
// for a run of the other. Another folder has its own.
func TestSyncersOfAFolderShareTheirLocks(t *testing.T) {
	root := t.TempDir()
	a := New(Config{Flush: func(context.Context) error { return nil }, Layout: paths.Layout{Root: root}})
	b := New(Config{Flush: func(context.Context) error { return nil }, Layout: paths.Layout{Root: filepath.Join(root, ".")}})
	other := New(Config{Flush: func(context.Context) error { return nil }, Layout: paths.Layout{Root: t.TempDir()}})
	a.workflow.Lock()
	defer a.workflow.Unlock()
	if b.workflow.TryLock() {
		b.workflow.Unlock()
		t.Fatal("a second Syncer of the folder runs beside the first")
	}
	if !other.workflow.TryLock() {
		t.Fatal("another folder waits for this one")
	}
	other.workflow.Unlock()
	a.gate.Lock()
	defer a.gate.Unlock()
	if b.gate.TryLock() {
		b.gate.Unlock()
		t.Fatal("the gate is not shared")
	}
}

// journalCheckingHistory notes whether the pending-merge record was on disk
// when a mutation was about to be published.
type journalCheckingHistory struct {
	unitHistory
	journal     func() string
	beforeShown []bool
}

func (h *journalCheckingHistory) Mutate(ctx context.Context, f func([]model.Item) ([]model.Item, error)) error {
	next, err := f(append([]model.Item(nil), h.items...))
	if err != nil {
		return err
	}
	_, serr := os.Stat(h.journal())
	h.beforeShown = append(h.beforeShown, serr == nil)
	h.items = append([]model.Item(nil), next...)
	return nil
}

// The pending-merge record is on disk before the downloaded or merged history
// can be seen (and saved by anyone): a crash after such a save resumes the
// upload instead of merging the same items again.
func TestPendingRecordIsWrittenBeforeTheHistoryIsSeen(t *testing.T) {
	remote := []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")}
	for _, merge := range []bool{false, true} {
		srv := serveSnapshot(t, snapshotFile(t, remote))
		h := &journalCheckingHistory{unitHistory: unitHistory{items: []model.Item{textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")}}}
		syn := unitSyncer(t, srv, signedIn(model.DefaultSettings()), h)
		h.journal = syn.pendingMergePath
		var err error
		if merge {
			err = syn.MergeAndApply(context.Background(), ManifestInfo{}, "", nil, "test")
		} else {
			_, err = syn.DownloadAndApply(context.Background(), nil, "test", "", nil)
		}
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(h.beforeShown) != 1 || !h.beforeShown[0] {
			t.Fatalf("merge=%v: the record was written after the history was seen", merge)
		}
	}
}

// A capture that takes the history right after the sync's change, while its
// suppression is still on, is told as a change once it is off.
func TestCaptureHiddenBySuppressionIsTold(t *testing.T) {
	remote := []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")}
	for _, capture := range []bool{false, true} {
		srv := serveSnapshot(t, snapshotFile(t, remote))
		var h History = &unitHistory{}
		if capture {
			h = &captureAfterMutationHistory{extra: textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "captured")}
		}
		syn := unitSyncer(t, srv, signedIn(model.DefaultSettings()), h)
		told := 0
		syn.changed = func() { told++ }
		_, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int{false: 0, true: 1}[capture]; told != want {
			t.Fatalf("capture=%v: told %d times, want %d", capture, told, want)
		}
	}
	a := NewAuto(New(Config{Flush: func(context.Context) error { return nil }, Layout: paths.Layout{Root: t.TempDir()}}), nil)
	if a == nil || New(Config{Flush: func(context.Context) error { return nil }, Layout: paths.Layout{Root: t.TempDir()}}).changed != nil {
		t.Fatal("only NewAuto tells the changes")
	}
}

// A restore records its state only when no other sync ran since its
// download; Restore does both in one workflow.
func TestRestoreStateIsNotRecordedOverANewerSync(t *testing.T) {
	remote := []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")}
	srv := serveSnapshot(t, snapshotFile(t, remote))
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	syn := unitSyncer(t, srv, s, &unitHistory{})
	dr, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	syn.beginWorkflow() // an automatic sync in between
	syn.workflow.Unlock()
	if err := syn.PersistAfterDownload(context.Background(), dr); !errors.Is(err, ErrRestoreSuperseded) {
		t.Fatalf("recorded over a newer sync: %v", err)
	}
	if s.SyncLastLocalFingerprint != nil {
		t.Fatal("the state was written")
	}
	dr, err = syn.Restore(context.Background(), nil, "test", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if model.Str(s.SyncLastLocalFingerprint) != dr.Fingerprint {
		t.Fatalf("Restore recorded %q, want %q", model.Str(s.SyncLastLocalFingerprint), dr.Fingerprint)
	}
}

// A sync that starts with only a refresh token learns its account from the
// token its manifest was asked with: a sign-in to another account while that
// request is out stops it, instead of using A's manifest with B's tokens.
func TestRunKeepsTheAccountOfItsManifest(t *testing.T) {
	var mu sync.Mutex
	st := model.DefaultSettings()
	st.AuthRefreshToken = model.Ptr("refresh-a")
	others := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			fmt.Fprintf(w, `{"accessToken":%q,"refreshToken":"refresh-a2","accessTokenExpiresAt":%q}`,
				testJWT("account-a"), time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case "/sync/manifest":
			mu.Lock() // B signs in while A's manifest is on its way
			st.AuthAccessToken, st.AuthRefreshToken = model.Ptr(testJWT("account-b")), model.Ptr("refresh-b")
			st.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
			mu.Unlock()
			w.WriteHeader(http.StatusNotFound)
		default:
			mu.Lock()
			others++
			mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	h := &unitHistory{items: []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "local")}}
	syn := New(Config{
		Flush:  func(context.Context) error { return nil },
		Layout: paths.Layout{Root: t.TempDir()},
		Settings: syncclient.Settings{
			Get:    func() model.Settings { mu.Lock(); defer mu.Unlock(); return st },
			Update: func(f func(*model.Settings)) error { mu.Lock(); f(&st); mu.Unlock(); return nil },
		},
		History: h, Client: syncclient.New(srv.URL), BaseURL: srv.URL,
	})
	if _, err := syn.Run(context.Background(), nil, "test"); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if others != 0 {
		t.Fatalf("%d requests after A's manifest, with B's tokens", others)
	}
}

// failingSettings are settings whose saves fail (a full disk): the change is
// made in memory, and Update says it was not saved.
func failingSettings(s *model.Settings, fail *bool) syncclient.Settings {
	return syncclient.Settings{
		Get: func() model.Settings { return *s },
		Update: func(f func(*model.Settings)) error {
			f(s)
			// Only the record of a sync fails to save (the tests start with
			// none): the rest of the settings went through just before.
			if *fail && s.SyncLastLocalFingerprint != nil {
				return errors.New("disk full")
			}
			return nil
		},
	}
}

// The record of a merge stays until its state is saved: a merge whose state
// cannot be saved stops before the upload and keeps it, and the next run
// recovers from it instead of merging again.
func TestPendingRecordStaysUntilTheStateIsSaved(t *testing.T) {
	remote := []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")}
	srv := serveSnapshot(t, snapshotFile(t, remote))
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	fail := true
	h := &unitHistory{items: []model.Item{textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")}}
	syn := unitSyncer(t, srv, s, h)
	syn.cfg.Settings = failingSettings(s, &fail)
	syn.session.Settings = syn.cfg.Settings
	if err := syn.MergeAndApply(context.Background(), ManifestInfo{Version: "v1"}, "pre", nil, "test"); err == nil {
		t.Fatal("a merge whose state was not saved went on")
	}
	if _, err := os.Stat(syn.pendingMergePath()); err != nil {
		t.Fatalf("the record went with the state unsaved: %v", err)
	}
	if err := syn.recoverPendingMerge(context.Background(), ""); err == nil {
		t.Fatal("recovered without saving the state")
	}
	if _, err := os.Stat(syn.pendingMergePath()); err != nil {
		t.Fatalf("the record went with the state unsaved: %v", err)
	}
	fail = false
	if err := syn.recoverPendingMerge(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(syn.pendingMergePath()); !os.IsNotExist(err) {
		t.Fatalf("the record outlived its saved state: %v", err)
	}
	if model.Str(s.SyncLastLocalFingerprint) != "pending-upload" {
		t.Fatalf("recovered state %q: not the pending upload", model.Str(s.SyncLastLocalFingerprint))
	}
}

// A record of an earlier download goes once an upload's state is saved: the
// server has the newer history, and recovering the record would put the old
// state back over it.
func TestUploadRetiresAnOlderPendingRecord(t *testing.T) {
	srv := serveSnapshot(t, nil) // no manifest: the upload's own answer is the state
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	fail := true
	syn := unitSyncer(t, srv, s, &unitHistory{})
	syn.cfg.Settings = failingSettings(s, &fail)
	if err := syn.writePendingMerge(pendingMergeState{Fingerprint: "old", Version: "v1", Committed: true}); err != nil {
		t.Fatal(err)
	}
	up := &syncclient.UploadResult{Version: "v2", SHA256: "abc"}
	if err := syn.persistAfterUpload(context.Background(), up, nil, "new", ""); err == nil {
		t.Fatal("a state that was not saved was reported")
	}
	if _, err := os.Stat(syn.pendingMergePath()); err != nil {
		t.Fatal("the record went before the upload's state was saved")
	}
	fail = false
	if err := syn.persistAfterUpload(context.Background(), up, nil, "new", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(syn.pendingMergePath()); !os.IsNotExist(err) {
		t.Fatalf("the older record outlived the upload: %v", err)
	}
	if err := syn.recoverPendingMerge(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if model.Str(s.SyncLastRemoteVersion) != "v2" || model.Str(s.SyncLastLocalFingerprint) != "new" {
		t.Fatalf("state %q %q", model.Str(s.SyncLastRemoteVersion), model.Str(s.SyncLastLocalFingerprint))
	}
}

// A state that is not written because another account is signed in by then
// is an error, and the record it would have replaced stays.
func TestSkippedStateKeepsThePendingRecord(t *testing.T) {
	s := signedIn(model.DefaultSettings())
	s.AuthAccessToken = model.Ptr(testJWT("account-b"))
	syn := unitSyncer(t, serveSnapshot(t, nil), s, &unitHistory{})
	if err := syn.writePendingMerge(pendingMergeState{Fingerprint: "fp", Committed: true}); err != nil {
		t.Fatal(err)
	}
	if err := syn.persistPendingMerge("pre", ManifestInfo{Version: "v1"}, "account-a"); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("a skipped state was reported as saved: %v", err)
	}
	if _, err := os.Stat(syn.pendingMergePath()); err != nil {
		t.Fatal("the record went with nothing saved in its place")
	}
	if s.SyncLastLocalFingerprint != nil {
		t.Fatal("A's state was written into B's settings")
	}
}

// The account file is replaced whole.
func TestAccountFileIsReplacedWhole(t *testing.T) {
	syn := unitSyncer(t, serveSnapshot(t, nil), signedIn(model.DefaultSettings()), &unitHistory{})
	for _, a := range []string{"account-a", "account-b"} {
		if err := syn.bindAccount(a); err != nil {
			t.Fatal(err)
		}
		if got := syn.boundAccount(); got != a {
			t.Fatalf("bound %q, want %q", got, a)
		}
	}
	ents, _ := os.ReadDir(filepath.Dir(syn.accountFile()))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "account.tmp-") {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
}

// limitsHistory counts the times its limits were applied.
type limitsHistory struct {
	unitHistory
	limited int
}

func (h *limitsHistory) ApplyLimitsAndSnapshot(model.Settings) ([]model.Item, []string) {
	h.limited++
	return h.Items(), nil
}

// A sign-in to another account just before a download writes its settings:
// A's settings are not written into B's session, nor A's limits applied to its
// history, and the download is undone.
func TestDownloadSettingsAreNotWrittenIntoAnotherSession(t *testing.T) {
	remote := []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")}
	srv := serveSnapshot(t, snapshotFile(t, remote))
	defer srv.Close()
	s := signedIn(model.DefaultSettings())
	s.AuthAccessToken = model.Ptr(testJWT("account-a"))
	s.MaxItems = 123
	h := &limitsHistory{}
	syn := unitSyncer(t, srv, s, h)
	syn.cfg.Settings.Update = func(f func(*model.Settings)) error {
		probe := *s
		f(&probe)
		if probe.MaxItems != s.MaxItems && model.Str(s.AuthAccessToken) == testJWT("account-a") {
			// B signs in right before A's settings are written, and sets a
			// preference of its own.
			s.AuthAccessToken = model.Ptr(testJWT("account-b"))
			s.Theme = "Dark"
		}
		f(s)
		return nil
	}
	syn.session.Settings = syn.cfg.Settings
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); !errors.Is(err, ErrSessionChanged) {
		t.Fatalf("download: %v", err)
	}
	if s.MaxItems != 123 {
		t.Fatalf("A's settings were written into B's session: MaxItems %d", s.MaxItems)
	}
	if h.limited != 0 {
		t.Fatal("A's limits were applied to B's history")
	}
	if s.Theme != "Dark" {
		t.Fatalf("the undo put A's settings over B's: Theme %q", s.Theme)
	}
	if len(h.items) != 0 {
		t.Fatalf("A's download was left in B's history: %v", h.items)
	}
}

// A merge whose save failed keeps its record until the undo is saved: an
// autosave may have saved the merged history, and a failed undo leaves it
// there.
func TestFailedSaveKeepsTheRecordUntilTheUndoIsSaved(t *testing.T) {
	remote := []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")}
	for _, undoSaved := range []bool{false, true} {
		srv := serveSnapshot(t, snapshotFile(t, remote))
		h := &unitHistory{items: []model.Item{textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")}}
		syn := unitSyncer(t, srv, signedIn(model.DefaultSettings()), h)
		flushes := 0
		syn.cfg.Flush = func(context.Context) error {
			flushes++
			if flushes == 1 || !undoSaved {
				return errors.New("disk full")
			}
			return nil
		}
		err := syn.MergeAndApply(context.Background(), ManifestInfo{}, "", nil, "test")
		srv.Close()
		if err == nil {
			t.Fatal("a merge that was not saved went on")
		}
		_, serr := os.Stat(syn.pendingMergePath())
		if !undoSaved && serr != nil {
			t.Fatal("the record went while the undo was not saved")
		}
		if undoSaved && !os.IsNotExist(serr) {
			t.Fatalf("the record outlived a saved undo: %v", serr)
		}
	}
}

// A merge saved together with a capture (by an autosave, before the commit
// marker) is still recognised after a restart by the items it brought, and
// its pending upload is recorded instead of merging again. A history without
// them was not saved with the merge.
func TestRecoveryRecognisesAMergeSavedWithACapture(t *testing.T) {
	brought := textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")
	local := textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")
	capture := textItemAt("16fd2706-8baf-433b-82eb-8c7fada847da", "captured")
	record := pendingMergeState{
		Fingerprint: "pending-upload", Version: "v1",
		After: Fingerprint([]model.Item{brought, local}), Applied: Fingerprint([]model.Item{brought, local}),
		Added: markOf([]model.Item{local}, []model.Item{brought, local}),
	}
	for _, saved := range []bool{true, false} {
		items := []model.Item{capture, local}
		if saved {
			items = []model.Item{capture, brought, local}
		}
		s := signedIn(model.DefaultSettings())
		syn := unitSyncer(t, serveSnapshot(t, nil), s, &unitHistory{items: items})
		if err := syn.writePendingMerge(record); err != nil {
			t.Fatal(err)
		}
		if err := syn.recoverPendingMerge(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		if got := model.Str(s.SyncLastLocalFingerprint) == "pending-upload"; got != saved {
			t.Fatalf("saved=%v: recovered=%v", saved, got)
		}
	}
	// A mutation that brought nothing leaves no mark: a count or a field it
	// changed is no proof, as the user can make the same change.
	raised := local
	raised.CopyCount = 2
	if p := (pendingMergeState{Added: markOf([]model.Item{local}, []model.Item{raised})}); p.markSeen([]model.Item{raised}) {
		t.Fatal("a raised count was taken as proof")
	}
}

// A Syncer that cannot save the history does not apply a download or a
// merge: it would record as saved, and clean the files of, a history that is
// not on disk.
func TestApplyingNeedsAWayToSaveTheHistory(t *testing.T) {
	remote := []model.Item{textItemAt("0f8fad5b-d9cb-469f-a165-70867728950e", "from the server")}
	srv := serveSnapshot(t, snapshotFile(t, remote))
	defer srv.Close()
	local := []model.Item{textItemAt("7c9e6679-7425-40de-944b-e07fc1f90ae7", "local")}
	h := &unitHistory{items: local}
	syn := unitSyncer(t, srv, signedIn(model.DefaultSettings()), h)
	syn.cfg.Flush = nil
	if _, err := syn.DownloadAndApply(context.Background(), nil, "test", "", nil); !errors.Is(err, ErrNoFlush) {
		t.Fatalf("download: %v", err)
	}
	if err := syn.MergeAndApply(context.Background(), ManifestInfo{}, "", nil, "test"); !errors.Is(err, ErrNoFlush) {
		t.Fatalf("merge: %v", err)
	}
	if h.replaced != 0 || Fingerprint(h.items) != Fingerprint(local) {
		t.Fatal("the history was changed")
	}
}
