package syncer_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"klip/internal/model"
	"klip/internal/nastest"
	"klip/internal/paths"
	"klip/internal/syncclient"
	"klip/internal/syncer"
)

// These tests run the real nas-agent-py (see package nastest) with several
// devices on one account, and check what each sync does and what each device
// remembers afterwards.

type fakeHistory struct {
	mu      sync.Mutex
	items   []model.Item
	limits  int
	replace int
}

func (h *fakeHistory) Items() []model.Item {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]model.Item(nil), h.items...)
}
func (h *fakeHistory) ReplaceAll(_ context.Context, items []model.Item) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.items = append([]model.Item(nil), items...)
	h.replace++
	return nil
}
func (h *fakeHistory) RemoveFiles([]string) error { return nil }

func (h *fakeHistory) Mutate(_ context.Context, f func([]model.Item) ([]model.Item, error)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	next, err := f(append([]model.Item(nil), h.items...))
	if err != nil {
		return err
	}
	h.items = append([]model.Item(nil), next...)
	h.replace++
	return nil
}
func (h *fakeHistory) ApplyLimits(model.Settings) { h.mu.Lock(); h.limits++; h.mu.Unlock() }
func (h *fakeHistory) ApplyLimitsAndSnapshot(model.Settings) ([]model.Item, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.limits++
	return append([]model.Item(nil), h.items...), nil
}
func (h *fakeHistory) add(it model.Item) {
	h.mu.Lock()
	h.items = append([]model.Item{it}, h.items...)
	h.mu.Unlock()
}

type device struct {
	t       *testing.T
	name    string
	layout  paths.Layout
	hist    *fakeHistory
	mu      sync.Mutex
	st      model.Settings
	flushes int
	suppr   []bool
	s       *syncer.Syncer
}

func (d *device) settings() model.Settings { d.mu.Lock(); defer d.mu.Unlock(); return d.st }

func newDevice(t *testing.T, srv *nastest.Server, name string, tok syncclient.TokenSet) *device {
	d := &device{t: t, name: name, layout: paths.Layout{Root: t.TempDir()}, hist: &fakeHistory{}, st: model.DefaultSettings()}
	d.st.AuthAccessToken, d.st.AuthRefreshToken = &tok.AccessToken, &tok.RefreshToken
	d.st.AuthExpiresAtUnixMs = tok.ExpiresAt.UnixMilli()
	d.st.NasAgentBaseUrl = srv.URL
	d.s = syncer.New(syncer.Config{
		Layout: d.layout,
		Settings: syncclient.Settings{
			Get:    d.settings,
			Update: func(f func(*model.Settings)) error { d.mu.Lock(); f(&d.st); d.mu.Unlock(); return nil },
		},
		History:          d.hist,
		Flush:            func(context.Context) error { d.mu.Lock(); d.flushes++; d.mu.Unlock(); return nil },
		Client:           syncclient.New(srv.URL),
		BaseURL:          srv.URL,
		SuppressAutoSync: func(on bool) { d.mu.Lock(); d.suppr = append(d.suppr, on); d.mu.Unlock() },
	})
	return d
}

func (d *device) run(ctx context.Context) syncer.RunResult {
	d.t.Helper()
	res, err := d.s.Run(ctx, nil, "test")
	if err != nil {
		d.t.Fatalf("%s: Run: %v", d.name, err)
	}
	return res
}

func (d *device) texts() []string {
	var out []string
	for _, it := range d.hist.Items() {
		out = append(out, model.Str(it.Text))
	}
	return out
}

func tm(t time.Time) model.Time { return model.Time{Time: t} }

func textItem(id, text string, at time.Time, hash string) model.Item {
	it := model.Item{ID: id, Kind: model.KindText, CapturedAt: tm(at), ApproxBytes: int64(len(text)) * 2, Text: &text, CopyCount: 1}
	if hash != "" {
		it.ContentHash = &hash
	}
	return it
}

func (d *device) imageItem(id string, data []byte, at time.Time) model.Item {
	os.MkdirAll(d.layout.Images(), 0o755)
	p := filepath.Join(d.layout.Images(), "1780000000000_"+id[:8]+".png")
	os.WriteFile(p, data, 0o644)
	h := "img-" + id[:8]
	return model.Item{ID: id, Kind: model.KindImage, CapturedAt: tm(at), ApproxBytes: int64(len(data)), ImageFilePath: &p, CopyCount: 1, ContentHash: &h}
}

func guid(n int) string {
	return strings.Replace("00000000-0000-0000-0000-0000000000NN", "NN", string(rune('0'+n/10))+string(rune('0'+n%10)), 1)
}

func TestDevicesSyncThroughTheRealServer(t *testing.T) {
	srv := nastest.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tok := srv.Subscribe(ctx, "owner@example.com", "password1")
	login := func() syncclient.TokenSet {
		tk, err := syncclient.New(srv.URL).Login(ctx, "owner@example.com", "password1")
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	a := newDevice(t, srv, "A", tok)
	b := newDevice(t, srv, "B", login())
	imgBytes := bytes.Repeat([]byte("\x89PNG-image-"), 3000)
	a.hist.items = []model.Item{
		textItem(guid(1), "alpha", base.Add(3*time.Minute), "h-alpha"),
		a.imageItem(guid(2), imgBytes, base.Add(2*time.Minute)),
		textItem(guid(3), "gamma 你好", base.Add(1*time.Minute), "h-gamma"),
	}
	a.st.Theme = "Dark"

	// ---- the account has no snapshot: the first sync uploads
	if _, err := a.s.ManifestInfo(ctx); !syncclient.IsStatus(err, 404) {
		t.Fatalf("a new account has no manifest: %v", err)
	}
	res := a.run(ctx)
	if res.Decision.Direction != syncer.Upload || res.Decision.Reason != "remote_missing" || res.Uploaded == nil {
		t.Fatalf("first sync of A: %+v", res.Decision)
	}
	man, err := a.s.ManifestInfo(ctx)
	if err != nil || man.Version != res.Uploaded.Version {
		t.Fatalf("manifest %+v %v", man, err)
	}
	st := a.settings()
	if model.Str(st.SyncLastRemoteVersion) != man.Version || strings.ToLower(model.Str(st.SyncLastRemoteSha256)) != man.SnapshotSHA256 ||
		model.Str(st.SyncLastLocalFingerprint) != a.s.Fingerprint() {
		t.Errorf("A remembers: %+v", st)
	}
	if at, err := model.ParseTime(model.Str(st.SyncLastRemoteUpdatedAt)); err != nil || !at.Equal(*man.UpdatedAt) {
		t.Errorf("updatedAt %q %v", model.Str(st.SyncLastRemoteUpdatedAt), err)
	}
	if _, err := os.Stat(filepath.Join(a.layout.Root, "sync_timing.log")); err != nil {
		t.Errorf("no timing log: %v", err)
	}

	// Nothing changed: nothing is sent, and the same snapshot is not sent again.
	res = a.run(ctx)
	if res.Decision.Direction != syncer.None || res.Decision.Reason != "state_matched" {
		t.Errorf("second sync of A: %+v", res.Decision)
	}
	up, err := a.s.Upload(ctx, nil, "again")
	if err != nil || up.Version != man.Version {
		t.Errorf("an unchanged snapshot is not uploaded again: %+v %v", up, err)
	}
	if again, _ := a.s.ManifestInfo(ctx); again.Version != man.Version {
		t.Error("the server has a new version")
	}

	// ---- B has nothing: it downloads, blobs included
	res = b.run(ctx)
	if res.Decision.Direction != syncer.Download || res.Decision.Reason != "first_sync_remote_exists" {
		t.Fatalf("first sync of B: %+v", res.Decision)
	}
	if got := strings.Join(b.texts(), "|"); got != "alpha||gamma 你好" {
		t.Errorf("B has %q", got)
	}
	items := b.hist.Items()
	if got, _ := os.ReadFile(model.Str(items[1].ImageFilePath)); !bytes.Equal(got, imgBytes) {
		t.Error("the image differs on B")
	}
	if !strings.HasPrefix(model.Str(items[1].ImageFilePath), b.layout.Images()) {
		t.Errorf("image path %q", model.Str(items[1].ImageFilePath))
	}
	if _, err := os.Stat(b.layout.BlobPath(strings.ToLower(model.Str(a.hist.items[1].ContentHash)))); err == nil {
		t.Log("(blobs are named by content, not by item hash)")
	}
	if bs := b.settings(); bs.Theme != "Dark" || model.Str(bs.SyncLastLocalFingerprint) != b.s.Fingerprint() ||
		model.Str(bs.SyncLastRemoteVersion) != man.Version {
		t.Errorf("B's settings: theme %s, %+v", bs.Theme, bs)
	}
	if b.hist.limits != 1 || b.flushes != 1 || len(b.suppr) != 2 || !b.suppr[0] || b.suppr[1] {
		t.Errorf("limits %d, flushes %d, suppress %v", b.hist.limits, b.flushes, b.suppr)
	}
	if res := b.run(ctx); res.Decision.Direction != syncer.None {
		t.Errorf("B again: %+v", res.Decision)
	}

	// ---- B changes, A follows
	b.hist.add(textItem(guid(4), "delta from B", base.Add(10*time.Minute), "h-delta"))
	res = b.run(ctx)
	if res.Decision.Direction != syncer.Upload || res.Decision.Reason != "local_changed_only" {
		t.Fatalf("B uploads: %+v", res.Decision)
	}
	res = a.run(ctx)
	if res.Decision.Direction != syncer.Download || res.Decision.Reason != "remote_changed_only" {
		t.Fatalf("A downloads: %+v", res.Decision)
	}
	if got := strings.Join(a.texts(), "|"); got != "delta from B|alpha||gamma 你好" {
		t.Errorf("A has %q", got)
	}
	if res := b.run(ctx); res.Decision.Direction != syncer.None {
		t.Errorf("B settled: %+v", res.Decision)
	}

	// ---- both change: the newer capture wins, and the other change is lost.
	// This is what the C# client does; the test says so, so that a change of
	// the rule is a decision and not an accident.
	a.hist.add(textItem(guid(5), "epsilon from A", base.Add(20*time.Minute), "h-eps"))
	if res := a.run(ctx); res.Decision.Reason != "local_changed_only" {
		t.Fatalf("A uploads epsilon: %+v", res.Decision)
	}
	b.hist.add(textItem(guid(6), "zeta from B", time.Now().UTC().Add(time.Minute), "h-zeta")) // newer than the upload
	res = b.run(ctx)
	if res.Decision.Direction != syncer.Upload || res.Decision.Reason != "timestamp_fallback" {
		t.Fatalf("B, both changed: %+v", res.Decision)
	}
	if res := a.run(ctx); res.Decision.Direction != syncer.Download {
		t.Fatalf("A follows: %+v", res.Decision)
	}
	if got := strings.Join(a.texts(), "|"); strings.Contains(got, "epsilon") || !strings.Contains(got, "zeta from B") {
		t.Errorf("A after the conflict: %q (epsilon was overwritten)", got)
	}

	// ---- a device with its own history and no state merges
	c := newDevice(t, srv, "C", login())
	dup := textItem(guid(7), "alpha", base.Add(30*time.Minute), "h-alpha") // the same content as A's alpha
	dup.CopyCount, dup.Note = 4, model.Ptr("note from C")
	c.hist.items = []model.Item{dup, textItem(guid(8), "only on C", base.Add(31*time.Minute), "h-c")}
	res = c.run(ctx)
	if res.Decision.Direction != syncer.Merge || res.Decision.Reason != "first_sync_merge" || res.Uploaded == nil {
		t.Fatalf("C: %+v", res.Decision)
	}
	backups, _ := filepath.Glob(filepath.Join(c.layout.Root, "sync", "backups", "pre-merge-*.sqlite"))
	if len(backups) != 1 || !regexpMatch(`pre-merge-\d{8}-\d{6}\.\d{3}-[0-9a-f]{8}\.sqlite$`, backups[0]) {
		t.Errorf("backups %v", backups)
	}
	merged := c.hist.Items()
	if len(merged) != 6 { // alpha (merged), C's own, and four of the server's five (epsilon was lost above)
		var ids []string
		for _, it := range merged {
			ids = append(ids, model.Str(it.Text))
		}
		t.Errorf("merged %d items: %v", len(merged), ids)
	}
	var alpha model.Item
	alphas := 0
	for _, it := range merged {
		if model.Str(it.Text) == "alpha" {
			alpha = it
			alphas++
		}
	}
	if alphas != 1 || alpha.CopyCount != 5 || model.Str(alpha.Note) != "note from C" {
		t.Errorf("alpha: %d copies in the list, %+v", alphas, alpha)
	}
	if merged[0].CapturedAt.Before(merged[len(merged)-1].CapturedAt.Time) {
		t.Error("the merged list is newest first")
	}
	if cs := c.settings(); model.Str(cs.SyncLastRemoteVersion) == "" || model.Str(cs.SyncLastLocalFingerprint) != c.s.Fingerprint() {
		t.Errorf("C remembers: %+v", cs)
	}
	if res := c.run(ctx); res.Decision.Direction != syncer.None {
		t.Errorf("C settled: %+v", res.Decision)
	}
	if res := a.run(ctx); res.Decision.Direction != syncer.Download || len(a.hist.Items()) != 6 {
		t.Errorf("A gets the merge: %+v, %d items", res.Decision, len(a.hist.Items()))
	}
}

func regexpMatch(pattern, s string) bool {
	ok, _ := regexpMatchString(pattern, s)
	return ok
}

func TestTokensAndSubscription(t *testing.T) {
	srv := nastest.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srv.Subscribe(ctx, "owner@example.com", "password1")
	// Each device signs in on its own: a refresh token works once.
	login := func() syncclient.TokenSet {
		tk, err := syncclient.New(srv.URL).Login(ctx, "owner@example.com", "password1")
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	base := time.Now().UTC().Add(-time.Hour)

	// Signed out: nothing is tried.
	out := newDevice(t, srv, "out", syncclient.TokenSet{})
	out.st.AuthAccessToken, out.st.AuthRefreshToken = nil, nil
	if out.s.CanAutoSync() {
		t.Error("no tokens: no auto sync")
	}
	if _, err := out.s.Run(ctx, nil, "t"); !errors.Is(err, syncer.ErrNotLoggedIn) {
		t.Errorf("signed out: %v", err)
	}

	// An access token that looks valid but is not: the server says 401, the
	// session renews it, and the call is repeated.
	d := newDevice(t, srv, "D", login())
	bad := "not-a-real-token"
	d.st.AuthAccessToken = &bad
	d.st.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	d.hist.items = []model.Item{textItem(guid(1), "one", base, "h1")}
	if !d.s.CanAutoSync() {
		t.Error("a valid-looking token")
	}
	if _, err := d.s.Run(ctx, nil, "t"); err != nil {
		t.Fatalf("after a 401: %v", err)
	}
	if got := model.Str(d.settings().AuthAccessToken); got == bad || got == "" {
		t.Errorf("the token was not renewed: %q", got)
	}

	// A token about to expire is renewed before the call; one that cannot be
	// renewed is "not logged in" and the server's refusal signs the device out.
	e := newDevice(t, srv, "E", login())
	e.st.AuthExpiresAtUnixMs = time.Now().Add(20 * time.Second).UnixMilli()
	e.hist.items = []model.Item{textItem(guid(1), "one", base, "h1")}
	if _, err := e.s.ManifestJSON(ctx); err != nil && !syncclient.IsStatus(err, 404) {
		t.Fatal(err)
	}
	if e.settings().AuthExpiresAtUnixMs < time.Now().Add(time.Hour).UnixMilli() {
		t.Error("the token was not renewed ahead of its expiry")
	}
	revoked := newDevice(t, srv, "R", syncclient.TokenSet{AccessToken: "x", RefreshToken: "revoked", ExpiresAt: time.Now().Add(-time.Minute)})
	if _, err := revoked.s.Run(ctx, nil, "t"); !errors.Is(err, syncer.ErrNotLoggedIn) {
		t.Errorf("revoked refresh token: %v", err)
	}
	if revoked.settings().AuthRefreshToken != nil {
		t.Error("a refused refresh token signs the device out")
	}

	// An account without a subscription.
	free, err := syncclient.New(srv.URL).Register(ctx, "free@example.com", "password1")
	if err != nil {
		t.Fatal(err)
	}
	f := newDevice(t, srv, "F", free)
	f.hist.items = []model.Item{textItem(guid(1), "one", base, "h1")}
	_, err = f.s.Run(ctx, nil, "t")
	if !errors.Is(err, syncer.ErrSubscriptionExpired) {
		t.Fatalf("no subscription: %v", err)
	}
	if got := syncer.Explain(err, syncer.InMainWindow); got != "订阅已过期，请续费后重试" {
		t.Errorf("explained as %q", got)
	}
	if _, err := f.s.Upload(ctx, nil, "t"); !errors.Is(err, syncer.ErrSubscriptionExpired) {
		t.Errorf("upload without a subscription: %v", err)
	}
}

func TestProgressSteps(t *testing.T) {
	srv := nastest.Start(t)
	ctx := context.Background()
	tok := srv.Subscribe(ctx, "owner@example.com", "password1")
	a := newDevice(t, srv, "A", tok)
	a.hist.items = []model.Item{a.imageItem(guid(2), []byte("img-bytes"), time.Now().UTC().Add(-time.Hour))}

	var seen []syncer.Progress
	if _, err := a.s.Run(ctx, func(p syncer.Progress) { seen = append(seen, p) }, "t"); err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, p := range seen {
		codes = append(codes, p.Code)
	}
	want := "check_remote prepare_local export_snapshot upload_blobs upload_snapshot"
	if strings.Join(codes, " ") != want {
		t.Errorf("steps %v", codes)
	}
	if seen[1].Percent != 30 || seen[2].Percent != 52 || seen[3].Percent != 68 || seen[4].Percent != 84 {
		t.Errorf("percents %+v", seen)
	}
}

func regexpMatchString(pattern, s string) (bool, error) { return regexp.MatchString(pattern, s) }
