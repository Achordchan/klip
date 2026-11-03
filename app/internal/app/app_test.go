package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"klip/internal/model"
	"klip/internal/nastest"
	"klip/internal/paths"
	"klip/internal/settings"
	"klip/internal/syncclient"
	"klip/internal/syncer"
)

type fakeClip struct {
	mu    sync.Mutex
	seq   atomic.Int64
	text  string
	wrote string
}

func (c *fakeClip) Formats() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.text != "" {
		return []string{"text/plain"}
	}
	return nil
}
func (c *fakeClip) ReadText() string             { c.mu.Lock(); defer c.mu.Unlock(); return c.text }
func (c *fakeClip) ReadFiles() ([]string, error) { return nil, nil }
func (c *fakeClip) ReadImage() []byte            { return nil }
func (c *fakeClip) WriteText(s string) error {
	c.mu.Lock()
	c.text, c.wrote = s, s
	c.mu.Unlock()
	c.seq.Add(1)
	return nil
}
func (c *fakeClip) WriteFiles([]string) error       { return nil }
func (c *fakeClip) WriteImage([]byte, string) error { return nil }

// copy is another program copying text.
func (c *fakeClip) copy(s string) {
	c.mu.Lock()
	c.text = s
	c.mu.Unlock()
	c.seq.Add(1)
}

func newApp(t *testing.T, root string, clip *fakeClip, base string) *App {
	t.Helper()
	a, err := New(Config{
		Layout:    paths.Layout{Root: root},
		Clipboard: clip,
		ChangeSeq: clip.seq.Load,
		Interval:  5 * time.Millisecond,
		BaseURL:   base,
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func top(a *App) string {
	if g := texts(a); len(g) > 0 {
		return g[0]
	}
	return ""
}

func texts(a *App) []string {
	var out []string
	for _, it := range a.History.Items() {
		out = append(out, model.Str(it.Text))
	}
	return out
}

func TestCaptureSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	clip := &fakeClip{}
	a := newApp(t, root, clip, "http://127.0.0.1:1") // no server: sync is off without a login
	a.Start()
	time.Sleep(40 * time.Millisecond) // the watcher records the counter
	clip.copy("first")
	waitFor(t, "first captured", func() bool { return len(texts(a)) == 1 })
	clip.copy("second")
	waitFor(t, "second captured", func() bool { return len(texts(a)) == 2 })
	a.Stop()

	if _, err := os.Stat(filepath.Join(root, "history.json")); err != nil {
		t.Fatalf("history.json not written: %v", err)
	}
	b := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1")
	if got := texts(b); len(got) != 2 || got[0] != "second" || got[1] != "first" {
		t.Fatalf("after restart: %v", got)
	}
	// What was on the clipboard before start is not captured.
	pre := &fakeClip{text: "was there before"}
	c := newApp(t, t.TempDir(), pre, "http://127.0.0.1:1")
	c.Start()
	time.Sleep(60 * time.Millisecond)
	c.Stop()
	if len(texts(c)) != 0 {
		t.Fatalf("captured the clipboard from before the start: %v", texts(c))
	}
}

func TestApplyMovesToTopAndIsNotCapturedBack(t *testing.T) {
	clip := &fakeClip{}
	a := newApp(t, t.TempDir(), clip, "http://127.0.0.1:1")
	a.Start()
	defer a.Stop()
	time.Sleep(40 * time.Millisecond)
	for _, s := range []string{"a", "b", "c"} {
		clip.copy(s)
		want := s
		waitFor(t, want, func() bool { return top(a) == want })
	}
	var itemA model.Item
	for _, it := range a.History.Items() {
		if model.Str(it.Text) == "a" {
			itemA = it
		}
	}
	a.Apply(itemA)
	clip.mu.Lock()
	wrote := clip.wrote
	clip.mu.Unlock()
	if wrote != "a" {
		t.Fatalf("clipboard got %q", wrote)
	}
	if got := texts(a); got[0] != "a" || len(got) != 3 {
		t.Fatalf("applied item not on top: %v", got)
	}
	time.Sleep(500 * time.Millisecond)
	// Applying is a use (Touch counts one more copy); a capture of our own
	// write would count another.
	if it := a.History.Items()[0]; it.CopyCount != 2 {
		t.Fatalf("copy count %d, want 2 (one use, no capture of our own write)", it.CopyCount)
	}
	if len(texts(a)) != 3 {
		t.Fatalf("history: %v", texts(a))
	}
}

func TestSettingsChangesTakeEffect(t *testing.T) {
	clip := &fakeClip{}
	root := t.TempDir()
	a := newApp(t, root, clip, "http://127.0.0.1:1")
	a.Start()
	defer a.Stop()
	time.Sleep(40 * time.Millisecond)
	for _, s := range []string{"1", "2", "3", "4"} {
		clip.copy(s)
		want := s
		waitFor(t, want, func() bool { return top(a) == want })
	}
	_ = a.Settings.Update(func(s *model.Settings) { s.MaxItems = 2 })
	// As in C#, the list is trimmed by the next change, not at once.
	clip.copy("5")
	waitFor(t, "5", func() bool { return top(a) == "5" })
	if got := texts(a); len(got) != 2 || got[1] != "4" {
		t.Fatalf("limit not applied: %v", got)
	}
	_ = a.Settings.Update(func(s *model.Settings) { s.CaptureText = false })
	clip.copy("ignored")
	time.Sleep(100 * time.Millisecond)
	if got := texts(a); got[0] != "5" {
		t.Fatalf("captured with text capture off: %v", got)
	}
	data, _ := os.ReadFile(filepath.Join(root, "settings.json"))
	if !strings.Contains(string(data), `"MaxItems":2`) || !strings.Contains(string(data), `"CaptureText":false`) {
		t.Fatalf("settings.json: %s", data)
	}
}

func TestSyncNowWithoutLogin(t *testing.T) {
	var changed atomic.Int32
	a, _ := New(Config{Layout: paths.Layout{Root: t.TempDir()}, Clipboard: &fakeClip{}, Changed: func() { changed.Add(1) }, Logf: t.Logf, BaseURL: "http://127.0.0.1:1"})
	a.SyncNow()
	waitFor(t, "status", func() bool { return a.SyncStatus() == "请先登录" })
	if changed.Load() == 0 {
		t.Fatal("the windows were not told")
	}
}

func TestBrokenFilesGiveDefaults(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "settings.json"), []byte("{broken"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "history.json"), []byte("[broken"), 0o644)
	a := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1")
	if a.Settings.Get().MaxItems != 200 || a.History.Len() != 0 {
		t.Fatal("not the defaults")
	}
	// The files are not overwritten until something changes.
	if b, _ := os.ReadFile(filepath.Join(root, "history.json")); string(b) != "[broken" {
		t.Fatalf("history.json was touched: %q", b)
	}
}

// Two programs, one account, the real server: a copy on A shows up on B.
func TestTwoDevicesThroughServer(t *testing.T) {
	srv := nastest.Start(t)
	ctx := context.Background()
	tokA := srv.Subscribe(ctx, "owner@example.com", "password1")
	tokB, err := syncclient.New(srv.URL).Login(ctx, "owner@example.com", "password1")
	if err != nil {
		t.Fatal(err)
	}
	signedIn := func(root string, tok syncclient.TokenSet) {
		st := model.DefaultSettings()
		st.AuthAccessToken, st.AuthRefreshToken = &tok.AccessToken, &tok.RefreshToken
		st.AuthExpiresAtUnixMs = tok.ExpiresAt.UnixMilli()
		if err := settings.Save(filepath.Join(root, "settings.json"), st); err != nil {
			t.Fatal(err)
		}
	}
	rootA, rootB := t.TempDir(), t.TempDir()
	signedIn(rootA, tokA)
	signedIn(rootB, tokB)

	clipA, clipB := &fakeClip{}, &fakeClip{}
	a := newApp(t, rootA, clipA, srv.URL)
	a.Start()
	defer a.Stop()
	waitFor(t, "A's first sync", func() bool { return strings.HasPrefix(a.SyncStatus(), "已同步") })
	remote := syncclient.New(srv.URL)
	// An empty history has nothing to upload: the server has no snapshot yet.
	m0, _ := remote.GetManifestJSON(ctx, tokA.AccessToken)

	time.Sleep(40 * time.Millisecond)
	clipA.copy("from device A")
	waitFor(t, "A captured", func() bool { return len(texts(a)) == 1 })

	b := newApp(t, rootB, clipB, srv.URL)
	// B starts only after A's change has gone up (the pause is 1.2 s).
	waitFor(t, "A uploaded", func() bool {
		m, err := remote.GetManifestJSON(ctx, tokA.AccessToken)
		return err == nil && m != "" && m != m0
	})
	b.Start()
	defer b.Stop()
	waitFor(t, "B downloaded", func() bool {
		g := texts(b)
		return len(g) == 1 && g[0] == "from device A"
	})

	// And back: B copies, A learns of it through the event stream.
	time.Sleep(40 * time.Millisecond)
	clipB.copy("from device B")
	waitFor(t, "A got B's copy", func() bool {
		g := texts(a)
		return len(g) == 2 && g[0] == "from device B"
	})
}

// Signing in through the account service, then telling the app: it syncs at
// once, without a restart.
func TestSignInStartsSync(t *testing.T) {
	srv := nastest.Start(t)
	ctx := context.Background()
	clip := &fakeClip{}
	a := newApp(t, t.TempDir(), clip, srv.URL)
	a.Start()
	defer a.Stop()
	time.Sleep(40 * time.Millisecond)
	clip.copy("before signing in")
	waitFor(t, "captured", func() bool { return len(texts(a)) == 1 })
	if a.SyncStatus() != "" {
		t.Fatalf("status while signed out: %q", a.SyncStatus())
	}
	if r := a.Account.Register(ctx, "new@example.com", "password1"); !r.OK {
		t.Fatal(r)
	}
	srv.Admin("/admin/user/upgrade", map[string]any{"emailOrUserId": "new@example.com", "durationDays": 30, "tier": "Mini"})
	a.AccountChanged(true)
	waitFor(t, "synced", func() bool { return strings.HasPrefix(a.SyncStatus(), "已同步") })
	m, err := syncclient.New(srv.URL).GetManifestJSON(ctx, model.Str(a.Settings.Get().AuthAccessToken))
	if err != nil || m == "" {
		t.Fatalf("nothing uploaded after sign-in: %q %v", m, err)
	}
}

func TestPrefsAndAccountAreWired(t *testing.T) {
	a := newApp(t, t.TempDir(), &fakeClip{}, "http://127.0.0.1:1")
	if a.Account == nil || a.Prefs == nil {
		t.Fatal("not wired")
	}
	if e := a.Prefs.SetMaxItems("7"); !e.Changed || a.History.Limits().MaxItems == nil || *a.History.Limits().MaxItems != 7 {
		t.Fatalf("a setting changed in the preferences must reach the history: %+v", e)
	}
}

// A newer version on the manifest is found a little after the start, quietly,
// and the files of the old program are cleaned once.
func TestStartChecksForUpdatesAndCleansLegacyFiles(t *testing.T) {
	manifest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"appId":"maccy","latest":{"version":"99.0.0","notes":"n","installer":{"url":"http://x/setup.exe"}}}`))
	}))
	defer manifest.Close()

	root := t.TempDir()
	install := t.TempDir()
	exe := filepath.Join(install, "maccy.exe")
	_ = os.WriteFile(exe, []byte("exe"), 0o755)
	_ = os.WriteFile(filepath.Join(install, "maccy.dll"), []byte("net"), 0o644)
	_ = os.WriteFile(filepath.Join(install, "settings.json"), []byte("{}"), 0o644)
	_ = os.MkdirAll(filepath.Join(root, "updates", "helper"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "updates", "helper", "old.exe"), []byte("x"), 0o755)
	list := `{"appId":"maccy","version":"1.0.11","runtime":"win-x64","files":[{"path":"maccy.exe"}],"legacyFiles":["maccy.dll","settings.json"]}`
	_ = os.WriteFile(filepath.Join(root, "updates", "installed-package-files.json"), []byte(list), 0o644)

	var changed atomic.Int32
	a, err := New(Config{Layout: paths.Layout{Root: root}, Clipboard: &fakeClip{}, ChangeSeq: (&fakeClip{}).seq.Load,
		Interval: 5 * time.Millisecond, BaseURL: "http://127.0.0.1:1", Logf: t.Logf,
		Exe: exe, ManifestURL: manifest.URL, UpdateFirst: 20 * time.Millisecond, UpdateEvery: time.Hour,
		Changed: func() { changed.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	a.Start()
	defer a.Stop()
	waitFor(t, "update found", func() bool { return a.Updates.Available() != nil })
	if v := a.Updates.Available().Version; v != "99.0.0" {
		t.Fatalf("version %q", v)
	}
	if changed.Load() == 0 {
		t.Fatal("the windows were not told")
	}
	waitFor(t, "legacy cleanup", func() bool { _, err := os.Stat(filepath.Join(install, "maccy.dll")); return err != nil })
	if b, _ := os.ReadFile(filepath.Join(install, "settings.json")); string(b) != "{}" {
		t.Fatal("data removed")
	}
	waitFor(t, "helper dir cleaned", func() bool { _, err := os.Stat(filepath.Join(root, "updates", "helper")); return err != nil })
}

func TestFirstSyncIsTriedAgainUntilItGoesThrough(t *testing.T) {
	a, err := New(Config{
		Layout: paths.Layout{Root: t.TempDir()}, Clipboard: &fakeClip{}, ChangeSeq: (&fakeClip{}).seq.Load,
		Interval: 5 * time.Millisecond, BaseURL: "http://127.0.0.1:1", Logf: t.Logf,
		StartupRetry: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	a.Auto.CanSync = func() bool { return true }
	a.Auto.Run = func(ctx context.Context, reason string, manual bool) error {
		if runs.Add(1) < 3 {
			return errors.New("offline") // the first two tries find no network
		}
		return nil
	}
	a.Start()
	defer a.Stop()
	waitFor(t, "third try", func() bool { return runs.Load() >= 3 })
	waitFor(t, "initial sync counted", func() bool { return a.initialOK.Load() })
	time.Sleep(100 * time.Millisecond)
	if n := runs.Load(); n != 3 {
		t.Fatalf("%d syncs: once one has gone through, the retries stop", n)
	}
}

func TestLoginSyncAskedForDuringASyncIsNotLost(t *testing.T) {
	a, err := New(Config{
		Layout: paths.Layout{Root: t.TempDir()}, Clipboard: &fakeClip{}, ChangeSeq: (&fakeClip{}).seq.Load,
		Interval: 5 * time.Millisecond, BaseURL: "http://127.0.0.1:1", Logf: t.Logf,
		StartupRetry: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var mu sync.Mutex
	var reasons []string
	a.Auto.CanSync = func() bool { return true }
	a.Auto.Run = func(ctx context.Context, reason string, manual bool) error {
		mu.Lock()
		reasons = append(reasons, reason)
		first := len(reasons) == 1
		mu.Unlock()
		if first {
			<-release // a long sync of the old account
		}
		return nil
	}
	a.Start()
	defer a.Stop()
	waitFor(t, "first sync running", func() bool { mu.Lock(); defer mu.Unlock(); return len(reasons) == 1 })
	a.AccountChanged(true) // a new sign-in while that runs: its sync is turned away
	close(release)
	waitFor(t, "login sync after it", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reasons) >= 2 && (reasons[len(reasons)-1] == "login" || reasons[len(reasons)-1] == "auto")
	})
}

func TestChangeDuringASyncIsSyncedAfterIt(t *testing.T) {
	a, err := New(Config{
		Layout: paths.Layout{Root: t.TempDir()}, Clipboard: &fakeClip{}, ChangeSeq: (&fakeClip{}).seq.Load,
		Interval: 5 * time.Millisecond, BaseURL: "http://127.0.0.1:1", Logf: t.Logf,
		StartupRetry: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Auto.Debounce = 20 * time.Millisecond
	release := make(chan struct{})
	var mu sync.Mutex
	var reasons []string
	a.Auto.CanSync = func() bool { return true }
	a.Auto.Run = func(ctx context.Context, reason string, manual bool) error {
		a.syncing.Add(1) // as the Run that New installs does
		defer a.syncing.Add(-1)
		mu.Lock()
		reasons = append(reasons, reason)
		first := len(reasons) == 1
		mu.Unlock()
		if first {
			<-release
		}
		return nil
	}
	a.Start()
	defer a.Stop()
	waitFor(t, "first sync running", func() bool { mu.Lock(); defer mu.Unlock(); return len(reasons) == 1 })
	a.History.Add(model.Item{ID: "x", Kind: model.KindText, Text: model.Ptr("new while syncing"), CopyCount: 1})
	close(release)
	waitFor(t, "a follow-up sync for the change", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reasons) >= 2
	})
}

func TestAccountChangeCancelsTheSyncOfTheOldAccount(t *testing.T) {
	a, err := New(Config{
		Layout: paths.Layout{Root: t.TempDir()}, Clipboard: &fakeClip{}, ChangeSeq: (&fakeClip{}).seq.Load,
		Interval: 5 * time.Millisecond, BaseURL: "http://127.0.0.1:1", Logf: t.Logf,
		StartupRetry: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan context.Context, 1)
	a.Auto.CanSync = func() bool { return true }
	a.Auto.Run = func(ctx context.Context, reason string, manual bool) error {
		a.syncing.Add(1)
		a.runActive.Store(true)
		a.runGen.Store(a.acctGen.Load())
		select {
		case started <- ctx:
		default:
		}
		<-ctx.Done() // a long sync of the old account, until it is told to stop
		return ctx.Err()
	}
	a.Start()
	defer a.Stop()
	var ctx context.Context
	select {
	case ctx = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no sync started")
	}
	a.endAccountWork() // what the account service does just before it replaces the login
	if a.acct().Err() == nil {
		t.Fatal("between the old login and the new one no account work may start")
	}
	a.beginAccountWork() // and once the new one is stored
	if a.acct().Err() != nil {
		t.Fatal("the new account must get a live context")
	}
	a.AccountChanged(true)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the sync of the old account was left running")
	}
}

func TestFailedLaterSyncIsTriedAgain(t *testing.T) {
	a, err := New(Config{
		Layout: paths.Layout{Root: t.TempDir()}, Clipboard: &fakeClip{}, ChangeSeq: (&fakeClip{}).seq.Load,
		Interval: 5 * time.Millisecond, BaseURL: "http://127.0.0.1:1", Logf: t.Logf,
		StartupRetry: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	a.Auto.CanSync = func() bool { return true }
	a.Auto.Debounce = 5 * time.Millisecond
	a.Auto.Run = func(ctx context.Context, reason string, manual bool) error {
		if runs.Add(1) == 2 {
			return errors.New("offline") // the upload of a copied item finds no network
		}
		return nil
	}
	a.Start()
	defer a.Stop()
	waitFor(t, "initial sync counted", func() bool { return a.initialOK.Load() })
	a.Auto.Trigger(a.acct(), "auto", false)
	waitFor(t, "the failed sync is tried again", func() bool { return runs.Load() >= 3 })
}

func TestManualSyncTurnedAwayDoesNotLeaveTheFooterSyncing(t *testing.T) {
	a, err := New(Config{
		Layout: paths.Layout{Root: t.TempDir()}, Clipboard: &fakeClip{}, ChangeSeq: (&fakeClip{}).seq.Load,
		BaseURL: "http://127.0.0.1:1", Logf: t.Logf, StartupRetry: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	started := make(chan struct{})
	a.Auto.CanSync = func() bool { return true }
	a.Auto.Run = func(ctx context.Context, reason string, manual bool) error {
		close(started)
		<-release
		return nil
	}
	a.Auto.Trigger(context.Background(), "auto", false) // one is still running
	<-started
	a.setStatus("已同步 10:00") // it has published its result and is still unwinding
	a.SyncNow()              // turned away: nothing will ever follow it
	if got := a.SyncStatus(); got != "已同步 10:00" {
		t.Fatalf("footer %q: a request that was refused must not show syncing", got)
	}
	close(release)
}

func TestManualSyncThatFindsItselfSignedOutIsCounted(t *testing.T) {
	a, err := New(Config{
		Layout: paths.Layout{Root: t.TempDir()}, Clipboard: &fakeClip{}, ChangeSeq: (&fakeClip{}).seq.Load,
		BaseURL: "http://127.0.0.1:1", Logf: t.Logf, StartupRetry: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Auto.CanSync = func() bool { return true } // signed in when asked...
	a.Auto.Run = func(ctx context.Context, reason string, manual bool) error {
		a.syncing.Add(1) // as the wrapper that New installs does
		a.runActive.Store(true)
		a.runEntered.Store(true)
		return syncer.ErrNotLoggedIn // ...and refused by the server when it ran
	}
	a.Auto.Trigger(a.acct(), "manual", true)
	waitFor(t, "the sync is over", func() bool { return a.SyncStatus() == "请先登录" })
	waitFor(t, "counted out", func() bool { return a.syncing.Load() == 0 && !a.runActive.Load() })
}

// A deletion the server never heard of is made again after the next sync,
// which may have merged the item back in, and survives a restart.
func TestPendingDeletionIsMadeAgainAfterTheNextSync(t *testing.T) {
	srv := nastest.Start(t)
	ctx := context.Background()
	tok := srv.Subscribe(ctx, "owner@example.com", "password1")
	root := t.TempDir()
	st := model.DefaultSettings()
	st.AuthAccessToken, st.AuthRefreshToken = &tok.AccessToken, &tok.RefreshToken
	st.AuthExpiresAtUnixMs = tok.ExpiresAt.UnixMilli()
	if err := settings.Save(filepath.Join(root, "settings.json"), st); err != nil {
		t.Fatal(err)
	}
	// The record of an earlier run: an item was deleted, the upload failed.
	_ = os.MkdirAll(filepath.Join(root, "sync"), 0o700)
	_ = os.WriteFile(filepath.Join(root, "sync", "pending-delete.json"), []byte(`{"accounts":{"owner@example.com":[{"id":"gone","at":"`+time.Now().UTC().Format(time.RFC3339Nano)+`"}]}}`), 0o600)
	clip := &fakeClip{}
	a := newApp(t, root, clip, srv.URL)
	a.History.Add(model.Item{ID: "gone", Kind: model.KindText, Text: model.Ptr("restored by a merge"), CopyCount: 1, ContentHash: model.Ptr("text:restored by a merge")})
	a.History.Add(model.Item{ID: "kept", Kind: model.KindText, Text: model.Ptr("stays"), CopyCount: 1, ContentHash: model.Ptr("text:stays")})
	a.Start()
	defer a.Stop()
	waitFor(t, "the owed deletion was made", func() bool {
		for _, it := range a.History.Items() {
			if it.ID == "gone" {
				return false
			}
		}
		return len(a.History.Items()) == 1 && !a.pending.Owed(a.owner())
	})
	// The server has it now; the mark stays for a while, as the server's.
	if got := (&pendingDeletes{path: filepath.Join(root, "sync", "pending-delete.json")}); !(len(got.Tombstones("owner@example.com")) == 1 && !got.Owed("owner@example.com")) {
		t.Fatal("the record on disk does not say that the server has the deletion")
	}
}

func item(id, text string) model.Item {
	return model.Item{ID: id, Kind: model.KindText, Text: model.Ptr(text), CopyCount: 1, ContentHash: model.Ptr("text:" + text)}
}

func ids(stones []syncer.Tombstone) []string {
	var out []string
	for _, t := range stones {
		out = append(out, t.ID)
	}
	sort.Strings(out)
	return out
}

func TestPendingDeletesKeepAcrossRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync", "pending-delete.json")
	p := &pendingDeletes{path: path}
	if err := p.Add("a@example.com", nil, item("a", "a"), item("b", "b")); err != nil {
		t.Fatal(err)
	}
	again := &pendingDeletes{path: path} // a later run
	if got := ids(again.Tombstones("A@example.com")); len(got) != 2 || !again.Owed("a@example.com") {
		t.Fatalf("%v", got)
	}
	if got := again.Tombstones("b@example.com"); got != nil {
		t.Fatalf("another account reads the list: %v", got)
	}
	again.Delivered("a@example.com", nil)
	if p := (&pendingDeletes{path: path}); p.Owed("a@example.com") || len(p.Tombstones("a@example.com")) != 2 {
		t.Fatal("a deletion the server has had is kept, as the server's")
	}
	// Past the time it is enforced for, it is no longer handed to a sync, even
	// before the next Ack drops it; one the server never had still is.
	again.Add("a@example.com", nil, item("c", "c"))
	later := &pendingDeletes{path: path, now: func() time.Time { return time.Now().Add(keepDeleted + time.Hour) }}
	if got := ids(later.Tombstones("a@example.com")); len(got) != 1 || got[0] != "c" {
		t.Fatalf("enforced after the time: %v", got)
	}
	again.Delivered("a@example.com", nil)
	// After the time it is kept for, it goes.
	late := &pendingDeletes{path: path, now: func() time.Time { return time.Now().Add(keepDeleted + time.Hour) }}
	late.Prune("a@example.com")
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the list outlived its marks")
	}
}

// The deletion covers the item even when it says it was captured later than
// now; a copy made after that time is still new.
func TestDeletionCoversAnItemFromTheFuture(t *testing.T) {
	p := &pendingDeletes{}
	future := time.Now().Add(time.Hour)
	it := item("a", "hello")
	it.CapturedAt = model.Time{Time: future}
	p.Add("", nil, it)
	st := p.Tombstones("")
	if len(st) != 1 || st[0].At.Before(future) {
		t.Fatalf("%+v", st)
	}
	s := syncer.New(syncer.Config{Deleted: func() []syncer.Tombstone { return st }})
	later := item("b", "hello")
	later.CapturedAt = model.Time{Time: future.Add(time.Minute)}
	if got := s.Deleted([]model.Item{it, later}); len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("deleted %v", got)
	}
}

// An upload takes only the deletions whose items it did not have; one asked
// for while it was on its way is still owed.
func TestUploadTakesOnlyTheDeletionsItLeftOut(t *testing.T) {
	p := &pendingDeletes{}
	first, second := item("a", "a"), item("b", "b")
	p.Add("x@example.com", nil, first, second) // b's removal still waits for its turn
	p.MarkRepair("x@example.com")
	p.Delivered("x@example.com", []model.Item{second, item("c", "c")})
	if !p.Owed("x@example.com") || p.NeedsRepair("x@example.com") {
		t.Fatal("b's deletion was taken by an upload that still had b")
	}
	p.Delivered("x@example.com", []model.Item{item("c", "c")})
	if p.Owed("x@example.com") {
		t.Fatal("not taken by the upload without b")
	}
}

// A deletion made long ago and delivered only now is enforced for the whole
// time after it reached the server.
func TestDeletionIsKeptFromWhenTheServerHadIt(t *testing.T) {
	now := time.Now()
	p := &pendingDeletes{now: func() time.Time { return now }}
	p.Add("", nil, item("a", "a"))
	now = now.Add(keepDeleted + 24*time.Hour) // offline all this time
	p.Delivered("", nil)
	if got := p.Tombstones(""); len(got) != 1 {
		t.Fatal("dropped as soon as it reached the server")
	}
	now = now.Add(keepDeleted + time.Hour)
	p.Prune("")
	if got := p.Tombstones(""); len(got) != 0 {
		t.Fatalf("kept for ever: %v", got)
	}
}

// That the server needs a repair outlives a restart.
func TestRepairIsWrittenDown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending-delete.json")
	p := &pendingDeletes{path: path}
	p.Add("x@example.com", nil, item("a", "a"))
	p.Delivered("x@example.com", nil)
	p.MarkRepair("x@example.com") // a download left a restored copy out
	if !(&pendingDeletes{path: path}).NeedsRepair("X@example.com") {
		t.Fatal("not on disk")
	}
	p.Delivered("x@example.com", nil)
	if (&pendingDeletes{path: path}).NeedsRepair("x@example.com") {
		t.Fatal("kept after the upload")
	}
}

// Each account has its list: a sign-out, a sign-in to the same account, or
// the work of another account leave it as it is.
func TestPendingDeletesFollowTheirAccount(t *testing.T) {
	p := &pendingDeletes{path: filepath.Join(t.TempDir(), "pending-delete.json")}
	p.Add("a@example.com", nil, item("a1", "a1"))
	p.Add("b@example.com", nil, item("b1", "b1")) // B signed in and deleted something
	p.Add("a@example.com", nil, item("a2", "a2")) // the old account's upload failed now
	p.Delivered("b@example.com", nil)
	if got := ids(p.Tombstones("a@example.com")); len(got) != 2 || !p.Owed("a@example.com") {
		t.Fatalf("A's list: %v", got)
	}
	if got := ids(p.Tombstones("b@example.com")); len(got) != 1 || got[0] != "b1" || p.Owed("b@example.com") {
		t.Fatalf("B's list: %v", got)
	}
}

// A login without an address that learns it keeps what it noted; another
// login, after a change, does not get it.
func TestLoginThatLearnsItsAddressKeepsItsDeletions(t *testing.T) {
	a := newApp(t, t.TempDir(), &fakeClip{}, "http://127.0.0.1:1")
	a.pending.Add(a.owner(), nil, item("x", "x")) // no address yet
	_ = a.Settings.Update(func(m *model.Settings) { m.AuthUserEmail = model.Ptr("a@example.com") })
	waitFor(t, "adopted", func() bool { return len(a.pending.Tombstones("a@example.com")) == 1 })
	a.pending.Add("", nil, item("y", "y")) // late work of the login, under the old name
	if got := ids(a.pending.Tombstones("a@example.com")); len(got) != 2 {
		t.Fatalf("%v", got)
	}

	a.endAccountWork()
	_ = a.Settings.Update(func(m *model.Settings) { m.AuthUserEmail = nil })
	a.beginAccountWork()
	a.pending.Add("", nil, item("z", "z")) // another login without an address
	a.endAccountWork()                     // B signs in: a change of login, nothing to adopt
	// A late write of the old login lands first (the settings are told of it
	// after the generation moved); then the new login is stored.
	_ = a.Settings.Update(func(m *model.Settings) {})
	_ = a.Settings.Update(func(m *model.Settings) { m.AuthUserEmail = model.Ptr("b@example.com") })
	a.beginAccountWork()
	if got := ids(a.pending.Tombstones("b@example.com")); len(got) != 0 {
		t.Fatalf("B got %v", got)
	}
}

// The app keeps the list through a change of login.
func TestAccountChangeKeepsWhatIsOwed(t *testing.T) {
	a := newApp(t, t.TempDir(), &fakeClip{}, "http://127.0.0.1:1")
	_, _, owner := a.acctWork()
	a.pending.Add(owner, nil, item("x", "x"))
	a.endAccountWork()
	a.beginAccountWork()
	if got := a.pending.Tombstones(a.owner()); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

// The list is replaced whole: a write that fails leaves the one before.
func TestPendingDeletesAreWrittenWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending-delete.json")
	p := &pendingDeletes{path: path}
	if err := p.Add("", nil, item("a", "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("the temporary file was left")
	}
	_ = os.Mkdir(path+".tmp", 0o700) // the next write cannot be made
	if err := p.Add("", nil, item("b", "b")); err == nil {
		t.Fatal("a failed write must be reported")
	}
	if got := ids((&pendingDeletes{path: path}).Tombstones("")); len(got) != 1 || got[0] != "a" {
		t.Fatalf("the list on disk: %v", got)
	}
	if got := p.Tombstones(""); len(got) != 2 {
		t.Fatalf("still owed in this run: %v", got)
	}
}

// A file that cannot be read is not written over: what is deleted meanwhile
// is kept in memory, and joins what the file holds once it can be read. A
// file that is not a list of deletions is kept aside.
func TestPendingDeletesUnreadableFileIsNotWrittenOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending-delete.json")
	if err := (&pendingDeletes{path: path}).Add("a@example.com", nil, item("a", "a")); err != nil {
		t.Fatal(err)
	}
	broken := func(string) ([]byte, error) { return nil, fs.ErrPermission }
	p := &pendingDeletes{path: path, readFile: broken}
	if err := p.Add("a@example.com", nil, item("b", "b")); err == nil {
		t.Fatal("written over a file that could not be read")
	}
	if got := ids(p.Tombstones("a@example.com")); len(got) != 1 || got[0] != "b" {
		t.Fatalf("in memory: %v", got)
	}
	if got := ids((&pendingDeletes{path: path}).Tombstones("a@example.com")); len(got) != 1 || got[0] != "a" {
		t.Fatalf("the file: %v", got)
	}
	p.readFile = nil // it can be read again
	if err := p.Add("a@example.com", nil, item("c", "c")); err != nil {
		t.Fatal(err)
	}
	if got := ids((&pendingDeletes{path: path}).Tombstones("a@example.com")); len(got) != 3 {
		t.Fatalf("the file after: %v", got)
	}

	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	q := &pendingDeletes{path: path}
	if err := q.Add("a@example.com", nil, item("d", "d")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path + ".bad"); err != nil || string(b) != "{" {
		t.Fatalf("the damaged file: %q %v", b, err)
	}
	if got := ids((&pendingDeletes{path: path}).Tombstones("a@example.com")); len(got) != 1 || got[0] != "d" {
		t.Fatalf("after the damaged file: %v", got)
	}
}

// A login known by its session learns its address while the deletions' file
// cannot be written: the settings, which are written, keep the move, and it is
// made after a restart, also when the file still cannot be written then.
func TestSessionMoveSurvivesAnUnwritableDeletionsFile(t *testing.T) {
	root := t.TempDir()
	st := model.DefaultSettings()
	st.AuthRefreshToken = model.Ptr("r1")
	if err := settings.Save(filepath.Join(root, "settings.json"), st); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1")
	session := a.owner()
	if err := a.pending.Add(session, nil, item("x", "x")); err != nil {
		t.Fatal(err)
	}
	blocked := a.pending.path + ".tmp"
	if err := os.MkdirAll(filepath.Join(blocked, "x"), 0o700); err != nil { // no write gets through
		t.Fatal(err)
	}
	if err := a.Settings.Update(func(m *model.Settings) { // a refresh
		m.AuthRefreshToken, m.AuthUserEmail = model.Ptr("r1b"), model.Ptr("a@example.com")
	}); err != nil {
		t.Fatal(err)
	}
	if got := ids(a.pending.Tombstones("a@example.com")); len(got) != 1 {
		t.Fatalf("this run: %v", got)
	}

	b := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1") // a restart
	if got := ids(b.pending.Tombstones("a@example.com")); len(got) != 1 || got[0] != "x" {
		t.Fatalf("after a restart: %v", got)
	}
	if len(ownerMoves(b.Settings.Get())) != 1 {
		t.Fatalf("the move was forgotten before it was written: %v", ownerMoves(b.Settings.Get()))
	}
	if got := ids((&pendingDeletes{path: b.pending.path}).Tombstones("a@example.com")); len(got) != 0 {
		t.Fatalf("written through the block: %v", got)
	}

	if err := os.RemoveAll(blocked); err != nil {
		t.Fatal(err)
	}
	b.replayOwnerMoves()
	if got := ids((&pendingDeletes{path: b.pending.path}).Tombstones("a@example.com")); len(got) != 1 {
		t.Fatalf("the file: %v", got)
	}
	if ms := ownerMoves(b.Settings.Get()); ms != nil {
		t.Fatalf("kept after it was written: %v", ms)
	}
	if again, _ := settings.Load(filepath.Join(root, "settings.json")); ownerMoves(again) != nil {
		t.Fatal("the saved settings still name the move")
	}
}

// A refresh whose settings cannot be saved: the program starts again with
// the session it had, and still finds what it noted, which the file already
// keeps under the address. A new session of that login, whose address the
// settings did not keep, finds it too, also after another restart.
func TestSessionRenameSurvivesSettingsThatCannotBeSaved(t *testing.T) {
	root := t.TempDir()
	st := model.DefaultSettings()
	st.AuthRefreshToken = model.Ptr("r1")
	if err := settings.Save(filepath.Join(root, "settings.json"), st); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1")
	session := a.owner()
	if err := a.pending.Add(session, nil, item("x", "x")); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(root, "settings.json")
	saved, err := os.ReadFile(blocked)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}

	if err := a.Settings.Update(func(m *model.Settings) { // a refresh
		m.AuthRefreshToken, m.AuthUserEmail = model.Ptr("r1b"), model.Ptr("a@example.com")
	}); err == nil {
		t.Fatal("the settings were saved")
	}
	if err := os.RemoveAll(blocked); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(blocked, saved, 0600); err != nil {
		t.Fatal(err)
	}

	b := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1") // a restart
	if b.owner() != session {
		t.Fatalf("the settings name %q", b.owner())
	}
	if got := ids(b.pending.Tombstones(session)); len(got) != 1 || got[0] != "x" {
		t.Fatalf("after a restart: %v", got)
	}
	if err := b.Settings.Update(func(m *model.Settings) { // a refresh with no address
		m.AuthRefreshToken = model.Ptr("r3")
	}); err != nil {
		t.Fatal(err)
	}
	if got := ids(b.pending.Tombstones(b.owner())); len(got) != 1 {
		t.Fatalf("the new session: %v", got)
	}
	c := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1") // another
	if got := ids(c.pending.Tombstones(c.owner())); len(got) != 1 || c.owner() == session {
		t.Fatalf("after another restart (%s): %v", c.owner(), got)
	}
	if got := ids(c.pending.Tombstones("a@example.com")); len(got) != 1 {
		t.Fatalf("the account: %v", got)
	}
}

// A repair that could not be written down is written when it is asked for
// again, and is not reported as written before that.
func TestRepairThatCouldNotBeWrittenIsWrittenAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending-delete.json")
	p := &pendingDeletes{path: path}
	if err := p.Add("a@example.com", nil, item("x", "x")); err != nil {
		t.Fatal(err)
	}
	blocked := path + ".tmp"
	if err := os.MkdirAll(filepath.Join(blocked, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkRepair("a@example.com"); err == nil {
		t.Fatal("a failed write must be reported")
	}
	if err := p.MarkRepair("a@example.com"); err == nil {
		t.Fatal("reported as written while the file still lacks it")
	}
	if err := os.RemoveAll(blocked); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkRepair("a@example.com"); err != nil {
		t.Fatal(err)
	}
	if !(&pendingDeletes{path: path}).NeedsRepair("a@example.com") {
		t.Fatal("the file lacks the repair")
	}
}

// A server whose manifest gives no time for its snapshot: a deletion still
// merges in what another device uploaded since, instead of uploading over it.
func TestDeletionMergesASnapshotWithoutATime(t *testing.T) {
	srv := nastest.Start(t)
	ctx := context.Background()
	tokA := srv.Subscribe(ctx, "owner@example.com", "password1")
	login := func() syncclient.TokenSet {
		tok, err := syncclient.New(srv.URL).Login(ctx, "owner@example.com", "password1")
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(r *http.Response) error {
		if r.Request.URL.Path != "/sync/manifest" || r.StatusCode != http.StatusOK {
			return nil
		}
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return err
		}
		var doc map[string]any
		if json.Unmarshal(b, &doc) == nil {
			if latest, ok := doc["latest"].(map[string]any); ok {
				delete(latest, "updatedAt")
				b, _ = json.Marshal(doc)
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.Header.Set("Content-Length", strconv.Itoa(len(b)))
		return nil
	}
	timeless := httptest.NewServer(proxy)
	defer timeless.Close()
	remote := syncclient.New(srv.URL)
	manifest := func() string { m, _ := remote.GetManifestJSON(ctx, tokA.AccessToken); return m }

	rootA := signedInRoot(t, tokA)
	a := newApp(t, rootA, &fakeClip{}, srv.URL)
	a.Start()
	waitFor(t, "A's first sync", func() bool { return strings.HasPrefix(a.SyncStatus(), "已同步") })
	a.History.Add(item("9dd4e461-268c-8034-f5c8-564e155c67a6", "9dd4e461-268c-8034-f5c8-564e155c67a6"))
	a.History.Add(item("41529076-9594-460e-2e48-5922904f345d", "41529076-9594-460e-2e48-5922904f345d"))
	waitFor(t, "A synced", func() bool {
		return model.Str(a.Settings.Get().SyncLastLocalFingerprint) == a.Syncer.Fingerprint() && manifest() != ""
	})
	a.Stop()

	clipB := &fakeClip{}
	b := newApp(t, signedInRoot(t, login()), clipB, srv.URL)
	b.Start()
	waitFor(t, "B downloaded", func() bool { return b.History.Len() == 2 })
	m1 := manifest()
	time.Sleep(40 * time.Millisecond)
	clipB.copy("from B")
	waitFor(t, "B uploaded", func() bool { return b.History.Len() == 3 && manifest() != m1 })
	b.Stop()

	a2 := newApp(t, rootA, &fakeClip{}, timeless.URL)
	if info := syncer.ParseManifestInfo(func() string { m, _ := syncclient.New(timeless.URL).GetManifestJSON(ctx, tokA.AccessToken); return m }()); info.UpdatedAt != nil || info.Version == "" {
		t.Fatalf("the manifest seen: %+v", info)
	}
	a2.Delete("9dd4e461-268c-8034-f5c8-564e155c67a6")
	// An incomplete manifest must fail closed rather than overwrite remote data.
	time.Sleep(300 * time.Millisecond)
	if !a2.pending.Owed(a2.owner()) {
		t.Fatal("invalid manifest cleared pending deletion")
	}

}

// A deletion chooses, writes down and removes its items in one step: the
// same text copied again meanwhile (which keeps the id) waits, then stays,
// and the deletion written down does not cover it.
func TestDeletionAndACopyMadeMeanwhile(t *testing.T) {
	root := t.TempDir()
	st := model.DefaultSettings()
	st.AuthRefreshToken, st.AuthUserEmail = model.Ptr("refresh"), model.Ptr("a@example.com")
	if err := settings.Save(filepath.Join(root, "settings.json"), st); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1")
	old := item("x", "x")
	old.CapturedAt = model.Time{Time: time.Now().Add(-time.Minute)}
	a.History.Add(old)
	again := item("x2", "x") // the same text, copied again
	again.CapturedAt = model.Time{Time: time.Now().Add(time.Minute)}
	var calls atomic.Int32
	copied := make(chan struct{})
	a.remove(func(all []model.Item) []model.Item {
		if calls.Add(1) == 2 { // the removal itself
			go func() { a.History.Add(again); close(copied) }()
			select {
			case <-copied:
				t.Error("the history changed while the items were chosen")
			case <-time.After(50 * time.Millisecond):
			}
		}
		for _, it := range all {
			if it.ID == "x" {
				return []model.Item{it}
			}
		}
		return nil
	})
	waitFor(t, "the removal", func() bool { return calls.Load() >= 2 })
	<-copied
	waitFor(t, "the copy", func() bool { return hasText(a, "x") })
	if got := a.Syncer.Deleted(a.History.Items()); len(got) != 0 {
		t.Fatalf("the deletion covers the copy made after it: %v", idsOf(got))
	}
	a.done.Wait() // its upload, which fails, before the files go
}

// Deletions noted under a session while the file could not be read are the
// account's once the file is read and says the session is known by its
// address.
func TestDeletionsNotedUnreadJoinTheirAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending-delete.json")
	before := &pendingDeletes{path: path}
	if err := before.Add("session:1", nil, item("x", "x")); err != nil {
		t.Fatal(err)
	}
	if err := before.Adopt("session:1", "a@example.com"); err != nil {
		t.Fatal(err)
	}
	p := &pendingDeletes{path: path, readFile: func(string) ([]byte, error) { return nil, fs.ErrPermission }}
	_ = p.Add("session:1", nil, item("y", "y")) // the file cannot be read now
	_ = p.MarkRepair("session:1")
	p.readFile = nil
	if got := ids(p.Tombstones("a@example.com")); len(got) != 2 {
		t.Fatalf("the account: %v", got)
	}
	if !p.NeedsRepair("a@example.com") || !p.Owed("a@example.com") {
		t.Fatal("the repair or the owed deletion was left under the session")
	}
	if got := ids(p.Tombstones("session:1")); len(got) != 2 {
		t.Fatalf("the session: %v", got)
	}
}

func signedInRoot(t *testing.T, tok syncclient.TokenSet) string {
	t.Helper()
	root := t.TempDir()
	st := model.DefaultSettings()
	st.AuthAccessToken, st.AuthRefreshToken = &tok.AccessToken, &tok.RefreshToken
	st.AuthExpiresAtUnixMs = tok.ExpiresAt.UnixMilli()
	if err := settings.Save(filepath.Join(root, "settings.json"), st); err != nil {
		t.Fatal(err)
	}
	return root
}

func hasText(a *App, s string) bool {
	for _, g := range texts(a) {
		if g == s {
			return true
		}
	}
	return false
}

// A deletes an item while another device has added one that A has not seen:
// the upload of the deletion keeps what the other device added.
func TestDeletionKeepsWhatAnotherDeviceAdded(t *testing.T) {
	srv := nastest.Start(t)
	ctx := context.Background()
	tokA := srv.Subscribe(ctx, "owner@example.com", "password1")
	tokB, err := syncclient.New(srv.URL).Login(ctx, "owner@example.com", "password1")
	if err != nil {
		t.Fatal(err)
	}
	rootA, rootB := signedInRoot(t, tokA), signedInRoot(t, tokB)
	remote := syncclient.New(srv.URL)
	manifest := func() string { m, _ := remote.GetManifestJSON(ctx, tokA.AccessToken); return m }

	clipA := &fakeClip{}
	a := newApp(t, rootA, clipA, srv.URL)
	a.Start()
	waitFor(t, "A's first sync", func() bool { return strings.HasPrefix(a.SyncStatus(), "已同步") })
	m0 := manifest()
	time.Sleep(40 * time.Millisecond)
	clipA.copy("one")
	waitFor(t, "A captured", func() bool { return len(texts(a)) == 1 })
	time.Sleep(40 * time.Millisecond)
	clipA.copy("two")
	waitFor(t, "A uploaded", func() bool { m := manifest(); return len(texts(a)) == 2 && m != m0 && a.Syncer.Fingerprint() != "" })
	waitFor(t, "A synced both", func() bool { return model.Str(a.Settings.Get().SyncLastLocalFingerprint) == a.Syncer.Fingerprint() })
	a.Stop() // A goes offline
	var one string
	for _, it := range a.History.Items() {
		if model.Str(it.Text) == "one" {
			one = it.ID
		}
	}

	clipB := &fakeClip{}
	b := newApp(t, rootB, clipB, srv.URL)
	b.Start()
	waitFor(t, "B downloaded", func() bool { return len(texts(b)) == 2 })
	m1 := manifest()
	time.Sleep(40 * time.Millisecond)
	clipB.copy("from B")
	waitFor(t, "B uploaded", func() bool { return len(texts(b)) == 3 && manifest() != m1 })
	b.Stop()

	// A, back, deletes "one" before any sync of its own has run.
	a2 := newApp(t, rootA, &fakeClip{}, srv.URL)
	a2.Delete(one)
	waitFor(t, "the deletion went up", func() bool {
		return !hasText(a2, "one") && !a2.pending.Owed(a2.owner())
	})
	if !hasText(a2, "from B") || !hasText(a2, "two") {
		t.Fatalf("A after the deletion: %v", texts(a2))
	}

	// What the server has now: a new device sees it.
	tokC, err := syncclient.New(srv.URL).Login(ctx, "owner@example.com", "password1")
	if err != nil {
		t.Fatal(err)
	}
	c := newApp(t, signedInRoot(t, tokC), &fakeClip{}, srv.URL)
	c.Start()
	defer c.Stop()
	waitFor(t, "C downloaded", func() bool { return len(texts(c)) == 2 })
	if !hasText(c, "from B") || !hasText(c, "two") {
		t.Fatalf("the server has %v", texts(c))
	}
}

// The deletion is written down before the items go, and stays written down
// when the upload fails; a clearing writes down every item it removes.
func TestDeletionIsWrittenDownBeforeAndKeptWhenTheUploadFails(t *testing.T) {
	root := t.TempDir()
	st := model.DefaultSettings()
	st.AuthRefreshToken = model.Ptr("refresh") // signed in, the server cannot be reached
	if err := settings.Save(filepath.Join(root, "settings.json"), st); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1")
	for _, id := range []string{"x", "y", "z"} {
		a.History.Add(model.Item{ID: id, Kind: model.KindText, Text: model.Ptr(id), CopyCount: 1, ContentHash: model.Ptr("text:" + id)})
	}
	var owedWhenRemoved atomic.Int32
	a.History.OnChanged(func() { owedWhenRemoved.Store(int32(len(a.pending.Tombstones(a.owner())))) })

	a.Delete("x")
	a.done.Wait()
	if a.History.Len() != 2 || owedWhenRemoved.Load() != 1 {
		t.Fatalf("len %d, owed when removed %d", a.History.Len(), owedWhenRemoved.Load())
	}
	a.ClearAll()
	a.done.Wait()
	got := (&pendingDeletes{path: a.pending.path}).Tombstones(a.owner())
	if a.History.Len() != 0 || len(got) != 3 {
		t.Fatalf("len %d, owed on disk %v", a.History.Len(), got)
	}
}

// B was offline with an item A deleted; B comes back with a copy of its own
// and uploads its history over the server's, the deleted item with it. A
// takes the item out again at its next sync, and keeps what B added.
func TestDeviceThatWasOfflineCannotBringBackADeletion(t *testing.T) {
	srv := nastest.Start(t)
	ctx := context.Background()
	tokA := srv.Subscribe(ctx, "owner@example.com", "password1")
	login := func() syncclient.TokenSet {
		tok, err := syncclient.New(srv.URL).Login(ctx, "owner@example.com", "password1")
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	rootA, rootB := signedInRoot(t, tokA), signedInRoot(t, login())
	remote := syncclient.New(srv.URL)
	manifest := func() string { m, _ := remote.GetManifestJSON(ctx, tokA.AccessToken); return m }

	clipA := &fakeClip{}
	a := newApp(t, rootA, clipA, srv.URL)
	a.Start()
	defer a.Stop()
	waitFor(t, "A's first sync", func() bool { return strings.HasPrefix(a.SyncStatus(), "已同步") })
	time.Sleep(40 * time.Millisecond)
	clipA.copy("deleted on A")
	waitFor(t, "A captured", func() bool { return len(texts(a)) == 1 })
	time.Sleep(40 * time.Millisecond)
	clipA.copy("kept")
	waitFor(t, "A synced both", func() bool {
		return len(texts(a)) == 2 && model.Str(a.Settings.Get().SyncLastLocalFingerprint) == a.Syncer.Fingerprint()
	})

	b := newApp(t, rootB, &fakeClip{}, srv.URL)
	b.Start()
	waitFor(t, "B downloaded", func() bool { return len(texts(b)) == 2 })
	b.Stop() // B goes offline with the item

	var gone string
	for _, it := range a.History.Items() {
		if model.Str(it.Text) == "deleted on A" {
			gone = it.ID
		}
	}
	m1 := manifest()
	a.Delete(gone)
	waitFor(t, "A's deletion went up", func() bool { return !hasText(a, "deleted on A") && !a.pending.Owed(a.owner()) && manifest() != m1 })

	// B, offline, copied something, and comes back. The copy is more than two
	// seconds newer than A's upload: when both sides changed, the newer one is
	// sent (Decide's timestamp fallback), and B's goes over the server's.
	time.Sleep(2500 * time.Millisecond)
	b2 := newApp(t, rootB, &fakeClip{}, srv.URL)
	b2.History.Add(model.Item{ID: "a5ba5d3e-b06a-4b3d-be3c-71bdea892905", Kind: model.KindText, Text: model.Ptr("from B"), CopyCount: 1, ContentHash: model.Ptr("text:from B"), CapturedAt: model.Time{Time: time.Now()}})
	b2.Start()
	defer b2.Stop()

	waitFor(t, "A has B's copy and not the deleted item, and so does the server", func() bool {
		if !hasText(a, "from B") || hasText(a, "deleted on A") || !hasText(a, "kept") {
			return false
		}
		return model.Str(a.Settings.Get().SyncLastLocalFingerprint) == a.Syncer.Fingerprint() && !a.pending.NeedsRepair(a.owner())
	})
	c := newApp(t, signedInRoot(t, login()), &fakeClip{}, srv.URL)
	c.Start()
	defer c.Stop()
	waitFor(t, "C downloaded", func() bool { return len(texts(c)) == 2 })
	if hasText(c, "deleted on A") || !hasText(c, "from B") {
		t.Fatalf("the server has %v", texts(c))
	}
}

func jwtFor(email string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"HS256"}`)) + "." + enc([]byte(`{"sub":"u1","email":"`+email+`"}`)) + ".sig"
}

// A login of an older client has no address stored; its token names the
// account, and its deletions are that account's, also after signing out and
// in again.
func TestLoginWithoutAddressIsNamedByItsToken(t *testing.T) {
	st := model.DefaultSettings()
	st.AuthAccessToken = model.Ptr(jwtFor("old@example.com"))
	if got := ownerOf(st); got != "old@example.com" {
		t.Fatalf("owner = %q", got)
	}
	st.AuthAccessToken = model.Ptr("not a token")
	if got := ownerOf(st); got != "" {
		t.Fatalf("owner = %q", got)
	}

	root := t.TempDir()
	st = model.DefaultSettings()
	st.AuthAccessToken, st.AuthRefreshToken = model.Ptr(jwtFor("old@example.com")), model.Ptr("refresh")
	if err := settings.Save(filepath.Join(root, "settings.json"), st); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1")
	a.History.Add(item("x", "x"))
	a.Delete("x")
	a.done.Wait()
	// Signed out, and in again as the same account (with its address now).
	a.endAccountWork()
	_ = a.Settings.Update(func(m *model.Settings) { syncclient.ClearAuthSession(m, true) })
	a.beginAccountWork()
	a.endAccountWork()
	_ = a.Settings.Update(func(m *model.Settings) { m.AuthUserEmail = model.Ptr("old@example.com") })
	a.beginAccountWork()
	if got := ids(a.pending.Tombstones(a.owner())); len(got) != 1 || !a.pending.Owed(a.owner()) {
		t.Fatalf("owed after signing in again: %v", got)
	}
}

// The deletion is written down as soon as the user asks, while it waits for
// a sync to end; a wait cut short (a change of account, a quit) removes the
// item here and leaves the deletion owed by the account it was asked under.
func TestDeletionIsWrittenDownBeforeItWaits(t *testing.T) {
	root := t.TempDir()
	st := model.DefaultSettings()
	st.AuthRefreshToken, st.AuthUserEmail = model.Ptr("refresh"), model.Ptr("a@example.com")
	if err := settings.Save(filepath.Join(root, "settings.json"), st); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Layout: paths.Layout{Root: root}, Clipboard: &fakeClip{}, ChangeSeq: (&fakeClip{}).seq.Load,
		Interval: 5 * time.Millisecond, BaseURL: "http://127.0.0.1:1", Logf: t.Logf, StartupRetry: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	a.Auto.Run = func(context.Context, string, bool) error { return nil }
	a.History.Add(item("x", "x"))
	a.Start()
	defer a.Stop()
	held, release := make(chan struct{}), make(chan struct{})
	go a.Auto.Exclusive(context.Background(), func() { close(held); <-release }) // a sync that runs long
	<-held
	defer close(release)

	a.Delete("x")
	if got := ids(a.pending.Tombstones("a@example.com")); len(got) != 1 {
		t.Fatalf("not written down while waiting: %v", got)
	}
	a.endAccountWork() // the wait is cut short
	waitFor(t, "removed here", func() bool { return a.History.Len() == 0 })
	if !a.pending.Owed("a@example.com") {
		t.Fatal("the deletion was dropped")
	}
}

// What the syncer asks of the deletions while a piece of work runs is about
// the account the work began for, also after the login changed under it.
func TestDeletionsFollowTheAccountTheWorkBeganFor(t *testing.T) {
	root := t.TempDir()
	st := model.DefaultSettings()
	st.AuthUserEmail = model.Ptr("a@example.com")
	if err := settings.Save(filepath.Join(root, "settings.json"), st); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1")
	gone := item("x", "x")
	a.pending.Add("a@example.com", nil, gone)
	_, _, owner := a.acctWork()
	a.runOwner.Store(&owner) // a sync for A starts
	a.endAccountWork()
	_ = a.Settings.Update(func(m *model.Settings) { m.AuthUserEmail = model.Ptr("b@example.com") })
	a.beginAccountWork()
	if got := a.Syncer.Deleted([]model.Item{gone}); len(got) != 1 {
		t.Fatal("A's sync lost A's deletions when B signed in")
	}
	b := "b@example.com"
	a.runOwner.Store(&b) // the next sync is B's
	if got := a.Syncer.Deleted([]model.Item{gone}); len(got) != 0 {
		t.Fatal("B's sync got A's deletions")
	}
}

// A deletion's upload holds the gate when the login changes: the sync the
// sign-in asked for was turned away, and is made once the deletion, cut short,
// lets go.
func TestSignInSyncTurnedAwayByADeletionIsMadeAfterIt(t *testing.T) {
	reached, over := make(chan struct{}, 1), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		select { // the server does not answer before the client gives up
		case <-r.Context().Done():
		case <-over:
		}
	}))
	defer srv.Close()
	defer close(over)
	root := t.TempDir()
	st := model.DefaultSettings()
	st.AuthRefreshToken, st.AuthUserEmail = model.Ptr("refresh"), model.Ptr("a@example.com")
	if err := settings.Save(filepath.Join(root, "settings.json"), st); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Layout: paths.Layout{Root: root}, Clipboard: &fakeClip{}, ChangeSeq: (&fakeClip{}).seq.Load,
		Interval: 5 * time.Millisecond, BaseURL: srv.URL, Logf: t.Logf, StartupRetry: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var reasons []string
	a.Auto.Run = func(_ context.Context, reason string, _ bool) error {
		mu.Lock()
		reasons = append(reasons, reason)
		mu.Unlock()
		return nil
	}
	a.History.Add(item("x", "x"))
	a.Start()
	defer a.Stop()
	waitFor(t, "the first sync", func() bool { mu.Lock(); defer mu.Unlock(); return len(reasons) > 0 })

	a.Delete("x")
	select { // the deletion's upload holds the gate
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the deletion's upload never reached the server")
	}
	// A sign-in now: its sync is turned away (what AccountChanged notes when
	// it is), then the old login's work is ended.
	a.loginWanted.Store(true)
	a.endAccountWork()
	a.beginAccountWork()
	waitFor(t, "the sign-in's sync", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, r := range reasons {
			if r == "login" {
				return true
			}
		}
		return false
	})
}

// A login with nothing but a refresh token is known by its session: another
// such login does not get its deletions, and a refresh that tells its address
// (with a new refresh token) gives them to its account.
func TestSessionWithoutAddressKeepsItsOwnDeletions(t *testing.T) {
	session := func(refresh string) model.Settings {
		st := model.DefaultSettings()
		st.AuthRefreshToken = model.Ptr(refresh)
		return st
	}
	one, two := ownerOf(session("r1")), ownerOf(session("r2"))
	if !sessionOwner(one) || one == "" || one == two {
		t.Fatalf("sessions %q %q", one, two)
	}

	root := t.TempDir()
	if err := settings.Save(filepath.Join(root, "settings.json"), session("r1")); err != nil {
		t.Fatal(err)
	}
	a := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1")
	a.pending.Add(a.owner(), nil, item("x", "x"))
	a.pending.Add(two, nil, item("y", "y"))         // left by another session
	_ = a.Settings.Update(func(m *model.Settings) { // a refresh
		m.AuthRefreshToken, m.AuthUserEmail = model.Ptr("r1b"), model.Ptr("a@example.com")
	})
	if got := ids(a.pending.Tombstones("a@example.com")); len(got) != 1 || got[0] != "x" {
		t.Fatalf("the account got %v", got)
	}
	if got := ids(a.pending.Tombstones(two)); len(got) != 1 {
		t.Fatalf("the other session's: %v", got)
	}
}

// With duplicates not merged, two entries of the same text (with their own
// notes) are two items: deleting one leaves the other, here and in a sync.
func TestDeletingOneOfTwoDuplicatesKeepsTheOther(t *testing.T) {
	one, two := item("a", "same"), item("b", "same")
	one.Note, two.Note = model.Ptr("first"), model.Ptr("second")
	p := &pendingDeletes{}
	p.Add("", []model.Item{one, two}, one)
	s := syncer.New(syncer.Config{Deleted: func() []syncer.Tombstone { return p.Tombstones("") }})
	other := item("c", "same") // the same text, copied on another device
	got := s.Deleted([]model.Item{one, two, other})
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("deleted %v", idsOf(got))
	}
	// The server having the kept one is no sign that it lacks the deletion.
	p.Delivered("", []model.Item{two})
	if p.Owed("") {
		t.Fatal("the deletion counts as undelivered because the kept duplicate is there")
	}
}

// A login renamed twice (a session, then another, then its address): what
// still names the first name reaches the list.
func TestRenamedTwiceStillReachesTheList(t *testing.T) {
	p := &pendingDeletes{}
	p.Add("session:1", nil, item("x", "x"))
	p.Adopt("session:1", "session:2")
	p.Adopt("session:2", "a@example.com")
	if got := ids(p.Tombstones("session:1")); len(got) != 1 {
		t.Fatalf("the first name: %v", got)
	}
	p.Add("session:1", nil, item("y", "y")) // late work under the first name
	if got := ids(p.Tombstones("a@example.com")); len(got) != 2 {
		t.Fatalf("the account: %v", got)
	}
}

// With duplicates not merged, A holds two entries of the same text, both on
// the server; the server has also changed since. Deleting the newer one
// merges the server's history in before the upload: the older one, which the
// user kept, stays, here and on the server.
func TestDeletingTheNewerDuplicateKeepsTheOlderThroughAMerge(t *testing.T) {
	srv := nastest.Start(t)
	ctx := context.Background()
	tokA := srv.Subscribe(ctx, "owner@example.com", "password1")
	login := func() syncclient.TokenSet {
		tok, err := syncclient.New(srv.URL).Login(ctx, "owner@example.com", "password1")
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	noMerge := func(root string) {
		path := filepath.Join(root, "settings.json")
		st, _ := settings.Load(path)
		st.MergeDuplicates = false
		if err := settings.Save(path, st); err != nil {
			t.Fatal(err)
		}
	}
	rootA, rootB := signedInRoot(t, tokA), signedInRoot(t, login())
	noMerge(rootA)
	noMerge(rootB)
	remote := syncclient.New(srv.URL)
	manifest := func() string { m, _ := remote.GetManifestJSON(ctx, tokA.AccessToken); return m }

	a := newApp(t, rootA, &fakeClip{}, srv.URL)
	a.Start()
	waitFor(t, "A's first sync", func() bool { return strings.HasPrefix(a.SyncStatus(), "已同步") })
	older, newer := item("92eb5ffe-e6ae-2fec-3ad7-1c777531578f", "same"), item("0cc175b9-c0f1-b6a8-31c3-99e269772661", "same")
	older.Note, newer.Note = model.Ptr("kept"), model.Ptr("deleted")
	older.CapturedAt = model.Time{Time: time.Now().Add(-time.Minute)}
	newer.CapturedAt = model.Time{Time: time.Now()}
	a.History.Add(older)
	a.History.Add(newer)
	if a.History.Len() != 2 {
		t.Fatalf("duplicates were merged: %d", a.History.Len())
	}
	waitFor(t, "A synced both", func() bool {
		return model.Str(a.Settings.Get().SyncLastLocalFingerprint) == a.Syncer.Fingerprint() && manifest() != ""
	})
	a.Stop()

	clipB := &fakeClip{}
	b := newApp(t, rootB, clipB, srv.URL)
	b.Start()
	waitFor(t, "B downloaded", func() bool { return b.History.Len() == 2 })
	m1 := manifest()
	time.Sleep(40 * time.Millisecond)
	clipB.copy("from B")
	waitFor(t, "B uploaded", func() bool { return b.History.Len() == 3 && manifest() != m1 })
	b.Stop()

	a2 := newApp(t, rootA, &fakeClip{}, srv.URL)
	a2.Delete("0cc175b9-c0f1-b6a8-31c3-99e269772661")
	waitFor(t, "the deletion went up", func() bool { return !a2.pending.Owed(a2.owner()) && hasText(a2, "from B") })
	notes := func(x *App) []string {
		var out []string
		for _, it := range x.History.Items() {
			out = append(out, it.ID+":"+model.Str(it.Note))
		}
		return out
	}
	kept := func(got []string) bool { // "from B" and the older duplicate
		return len(got) == 2 && slices.Contains(got, "92eb5ffe-e6ae-2fec-3ad7-1c777531578f:kept") && !slices.Contains(got, "0cc175b9-c0f1-b6a8-31c3-99e269772661:deleted")
	}
	if got := notes(a2); !kept(got) {
		t.Fatalf("A after the deletion: %v", got)
	}
	c := newApp(t, signedInRoot(t, login()), &fakeClip{}, srv.URL)
	c.Start()
	defer c.Stop()
	waitFor(t, "C downloaded", func() bool { return c.History.Len() >= 2 })
	if got := notes(c); !kept(got) {
		t.Fatalf("the server has %v", got)
	}
}

// With duplicates not merged, A holds two entries of the same text and
// another item; the server has changed since. Deleting the other item merges
// the server's history in before the upload: the two entries stay two, with
// their notes, here and on the server.
func TestDeletingAnotherItemKeepsBothDuplicatesThroughAMerge(t *testing.T) {
	srv := nastest.Start(t)
	ctx := context.Background()
	tokA := srv.Subscribe(ctx, "owner@example.com", "password1")
	login := func() syncclient.TokenSet {
		tok, err := syncclient.New(srv.URL).Login(ctx, "owner@example.com", "password1")
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	noMerge := func(root string) {
		path := filepath.Join(root, "settings.json")
		st, _ := settings.Load(path)
		st.MergeDuplicates = false
		if err := settings.Save(path, st); err != nil {
			t.Fatal(err)
		}
	}
	rootA, rootB := signedInRoot(t, tokA), signedInRoot(t, login())
	noMerge(rootA)
	noMerge(rootB)
	remote := syncclient.New(srv.URL)
	manifest := func() string { m, _ := remote.GetManifestJSON(ctx, tokA.AccessToken); return m }

	a := newApp(t, rootA, &fakeClip{}, srv.URL)
	a.Start()
	waitFor(t, "A's first sync", func() bool { return strings.HasPrefix(a.SyncStatus(), "已同步") })
	older, newer, other := item("92eb5ffe-e6ae-2fec-3ad7-1c777531578f", "same"), item("0cc175b9-c0f1-b6a8-31c3-99e269772661", "same"), item("d9567975-2134-a2d9-eb61-dbd7b91c4bcc", "other")
	older.Note, newer.Note = model.Ptr("first"), model.Ptr("second")
	older.CapturedAt = model.Time{Time: time.Now().Add(-time.Minute)}
	newer.CapturedAt = model.Time{Time: time.Now()}
	a.History.Add(other)
	a.History.Add(older)
	a.History.Add(newer)
	if a.History.Len() != 3 {
		t.Fatalf("duplicates were merged: %d", a.History.Len())
	}
	waitFor(t, "A synced them", func() bool {
		return model.Str(a.Settings.Get().SyncLastLocalFingerprint) == a.Syncer.Fingerprint() && manifest() != ""
	})
	a.Stop()

	clipB := &fakeClip{}
	b := newApp(t, rootB, clipB, srv.URL)
	b.Start()
	waitFor(t, "B downloaded", func() bool { return b.History.Len() == 3 })
	m1 := manifest()
	time.Sleep(40 * time.Millisecond)
	clipB.copy("from B")
	waitFor(t, "B uploaded", func() bool { return b.History.Len() == 4 && manifest() != m1 })
	b.Stop()

	a2 := newApp(t, rootA, &fakeClip{}, srv.URL)
	a2.Delete("d9567975-2134-a2d9-eb61-dbd7b91c4bcc")
	waitFor(t, "the deletion went up", func() bool { return !a2.pending.Owed(a2.owner()) && hasText(a2, "from B") })
	notes := func(x *App) []string {
		var out []string
		for _, it := range x.History.Items() {
			out = append(out, it.ID+":"+model.Str(it.Note))
		}
		return out
	}
	kept := func(got []string) bool { // "from B" and both duplicates, with their notes
		return len(got) == 3 && slices.Contains(got, "0cc175b9-c0f1-b6a8-31c3-99e269772661:second") && slices.Contains(got, "92eb5ffe-e6ae-2fec-3ad7-1c777531578f:first")
	}
	if got := notes(a2); !kept(got) {
		t.Fatalf("A after the deletion: %v", got)
	}
	c := newApp(t, signedInRoot(t, login()), &fakeClip{}, srv.URL)
	c.Start()
	defer c.Stop()
	waitFor(t, "C downloaded", func() bool { return c.History.Len() >= 3 })
	if got := notes(c); !kept(got) {
		t.Fatalf("the server has %v", got)
	}
}

// While the deletion waits, a sync replaces the item with a later version of
// the same id (copied again on another device): the deletion written down at
// first does not cover that version, so the one removed is written down too.
func TestDeletionCoversTheVersionItRemoves(t *testing.T) {
	root := t.TempDir()
	st := model.DefaultSettings()
	st.AuthRefreshToken, st.AuthUserEmail = model.Ptr("refresh"), model.Ptr("a@example.com")
	if err := settings.Save(filepath.Join(root, "settings.json"), st); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Layout: paths.Layout{Root: root}, Clipboard: &fakeClip{}, ChangeSeq: (&fakeClip{}).seq.Load,
		Interval: 5 * time.Millisecond, BaseURL: "http://127.0.0.1:1", Logf: t.Logf, StartupRetry: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	a.Auto.Run = func(context.Context, string, bool) error { return nil }
	old := item("x", "x")
	old.CapturedAt = model.Time{Time: time.Now().Add(-time.Hour)}
	a.History.Add(old)
	a.Start()
	defer a.Stop()
	held, release := make(chan struct{}), make(chan struct{})
	go a.Auto.Exclusive(context.Background(), func() { close(held); <-release }) // a sync
	<-held

	a.Delete("x")
	later := old
	later.CapturedAt = model.Time{Time: time.Now().Add(time.Hour)} // what the sync brings in
	if err := a.History.ReplaceAll(context.Background(), []model.Item{later}); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, "removed", func() bool { return a.History.Len() == 0 }) // written down before
	if got := a.Syncer.Deleted([]model.Item{later}); len(got) != 1 {
		t.Fatal("the version removed is not covered: a merge would bring it back")
	}
}

func TestQueuedSyncUsesAccountCancellation(t *testing.T) {
	a := newApp(t, t.TempDir(), &fakeClip{}, "http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.acctCtx = ctx
	a.mu.Unlock()
	cancel()
	if err := a.Auto.Run(context.Background(), "auto", false); err == nil {
		t.Fatal("cancelled account run succeeded")
	}
}
