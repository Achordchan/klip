// Package app wires Klip's parts into one running program, without any window:
// settings, the history and its file, clipboard capture, and the sync with
// the server. The windows (package panel, cmd/klip) sit on top of it.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"klip/internal/account"
	"klip/internal/autostart"
	"klip/internal/capture"
	"klip/internal/history"
	"klip/internal/model"
	"klip/internal/paths"
	"klip/internal/platform"
	"klip/internal/prefs"
	"klip/internal/settings"
	"klip/internal/syncclient"
	"klip/internal/syncer"
	"klip/internal/update"
	"klip/internal/version"
)

// Config says what the App is made of. Clipboard is required; the rest has
// defaults for the real program.
type Config struct {
	Layout    paths.Layout
	Clipboard capture.Clipboard
	// Foreground says which program is in front when something is captured.
	Foreground func() *capture.AppIdentity
	// ChangeSeq and Interval drive the clipboard watcher.
	ChangeSeq func() int64
	Interval  time.Duration
	// StartupRetry is the first pause before the first sync is tried again
	// (30 s unless a test sets it).
	StartupRetry time.Duration
	// BaseURL is the sync server: the official one unless a test sets it.
	BaseURL string
	// Autostart is the start at sign-in (autostart.Default when nil).
	Autostart autostart.Service
	// Quit ends the program (an update asks for it); nil does nothing.
	Quit func()
	// Exe is the running program (os.Executable when empty).
	Exe string
	// ManifestURL is the update manifest (UpdateManifestURL when empty).
	ManifestURL string
	// UpdateFirst and UpdateEvery time the check for a new version (6 s and
	// 12 h unless a test sets them).
	UpdateFirst, UpdateEvery time.Duration
	// Changed is called, from any goroutine, when something the windows show
	// changed (the sync status; history changes are seen through the history).
	Changed func()
	// Toast shows a short message in the main window (may be nil).
	Toast func(msg string)
	Logf  func(format string, args ...any)
}

// UpdateManifestURL is where the update manifest is published: the address
// that 1.0.10 and older read, which stays the release channel.
const UpdateManifestURL = "https://gitee.com/Achordchan/maccy/raw/master/docs/updates/manifest.json"

// App is the program without windows.
type App struct {
	cfg       Config
	Layout    paths.Layout
	Settings  *settings.Manager
	History   *history.Service
	Persister *history.Persister
	Capture   *capture.Service
	Syncer    *syncer.Syncer
	Auto      *syncer.Auto
	Account   *account.Service
	Prefs     *prefs.Controller
	Updates   *update.Coordinator

	mu                sync.Mutex
	syncMu            sync.Mutex // the end of a sync and the notes of changes that came during it
	syncOver          bool       // under syncMu: the running sync has taken its notes and is ending
	status            string
	view              SyncView
	viewSeq           int // under mu: moves when a sync begins
	cancel            context.CancelFunc
	done              sync.WaitGroup
	started           bool
	ctx               context.Context // lives as long as the program, once started
	syncing           atomic.Int32    // syncs running, background or asked for
	runActive         atomic.Bool     // a sync counted in syncing is between Run and syncDone
	remoteDuringSync  atomic.Bool     // another device uploaded while a sync ran
	acctCtx           context.Context
	acctCancel        context.CancelFunc
	changedDuringSync atomic.Bool               // the history changed while a sync ran
	acctGen           atomic.Int64              // moves when the account changes
	runGen            atomic.Int64              // the account generation the running sync started under
	runOwner          atomic.Pointer[string]    // the account the running sync started for
	retrying          atomic.Bool               // the loop that tries the first sync again is running
	loginWanted       atomic.Bool               // a sync for a new sign-in is owed (one was running when it was asked for)
	initialOK         atomic.Bool               // a background sync has gone through
	runEntered        atomic.Bool               // the sync that syncDone is told about did start (Run was entered)
	pending           pendingDeletes            // what the user deleted here, by account
	login             atomic.Pointer[loginSeen] // see ownerLearned
	failStreak        atomic.Int32              // background syncs that failed in a row since the initial one
	stopWatch         context.CancelFunc        // ends the event stream of the current account
}

// New reads the settings and the history and connects the parts. A settings
// or history file that cannot be read is logged and replaced by the defaults
// in memory (the file is left alone until something changes), as the C#
// client does.
func New(cfg Config) (*App, error) {
	if cfg.Clipboard == nil {
		return nil, fmt.Errorf("app: no clipboard")
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = model.OfficialSyncBaseUrl
	}
	a := &App{cfg: cfg, Layout: cfg.Layout}

	var err error
	a.Settings, err = settings.Open(cfg.Layout.Settings())
	if err != nil {
		cfg.Logf("settings: %v", err)
	}
	a.History = history.NewService(cfg.Layout)
	a.pending.path = filepath.Join(cfg.Layout.Root, "sync", "pending-delete.json")
	a.pending.logf = cfg.Logf
	a.login.Store(&loginSeen{owner: a.owner()})
	a.Settings.Watch(a.ownerLearned)
	a.replayOwnerMoves()
	a.Persister = history.NewPersister(a.History, cfg.Layout)
	if err := a.Persister.Load(context.Background()); err != nil {
		cfg.Logf("history: %v", err)
	}
	a.Capture = capture.New(cfg.Layout, a.History, cfg.Clipboard)
	a.Capture.ForegroundApp = cfg.Foreground
	// The change counter lets Capture tell the change Apply made from a copy
	// by somebody else; where there is none, the content token does it.
	seq := cfg.ChangeSeq
	if seq == nil {
		seq = platform.ChangeSeq
	}
	a.Capture.Seq = seq
	if seq() < 0 {
		a.Capture.Token = platform.ContentFingerprint
	}
	// Without a counter a write is seen up to one poll late.
	iv := cfg.Interval
	if iv <= 0 {
		iv = platform.DefaultInterval
	}
	a.Capture.OwnWindow = iv + 250*time.Millisecond

	st := a.Settings.Get()
	a.applySettings(st)
	a.Settings.OnChange(a.applySettings)

	a.Syncer = syncer.New(syncer.Config{
		Layout: cfg.Layout,
		Settings: syncclient.Settings{
			Get: a.Settings.Get,
			// The record of a sync belongs to the account it ran for: one that
			// outlives a change of account does not write into the new one's.
			Update: func(f func(*model.Settings)) error {
				gen := a.runGen.Load()
				rejected := false
				err := a.Settings.Update(func(m *model.Settings) {
					// Looked at inside the update, which the replacing of the
					// tokens is serialized with: a write that comes after the
					// account changed is the old account's, and is dropped.
					if a.runActive.Load() && gen != a.acctGen.Load() {
						rejected = true
						return
					}
					f(m)
				})
				if rejected {
					return syncer.ErrSessionChanged
				}
				return err
			},
		},
		History:          a.History,
		Flush:            a.Persister.Flush,
		Client:           syncclient.New(cfg.BaseURL),
		BaseURL:          cfg.BaseURL,
		SuppressAutoSync: func(on bool) { a.Auto.Suppress(on) },
		// What the syncer asks and tells of the deletions is about the account
		// the work running was started for (runOwner), whatever is signed in by
		// the time it asks: an upload made for A tells of A's server.
		Deleted: func() []syncer.Tombstone { return a.pending.Tombstones(a.workOwner()) },
		Account: a.workOwner,
		Dropped: func() error {
			err := a.pending.MarkRepair(a.workOwner())
			if err != nil {
				a.cfg.Logf("pending deletions: %v", err)
			}
			return err
		},
		Uploaded: func(items []model.Item) {
			if err := a.pending.Delivered(a.workOwner(), items); err != nil {
				a.cfg.Logf("pending deletions: %v", err)
			}
		},
	})
	a.Auto = syncer.NewAuto(a.Syncer, a.syncDone)
	// Every sync is counted while it runs, so that Stop can wait for them (they
	// start their own goroutines inside Auto).
	a.Auto.Begin = a.syncBegin
	a.Auto.Progress = a.syncProgress
	run := a.Auto.Run
	a.Auto.Run = func(ctx context.Context, reason string, manual bool) error {
		a.syncMu.Lock()
		a.syncOver = false
		a.syncing.Add(1) // ended by syncDone, which Auto calls when Run returns
		a.syncMu.Unlock()
		a.runActive.Store(true)
		a.runEntered.Store(true)
		accountCtx, gen, owner := a.acctWork()
		ctx, cancel := context.WithCancel(ctx)
		stopCancel := context.AfterFunc(accountCtx, cancel)
		defer stopCancel()
		defer cancel()
		if accountCtx.Err() != nil {
			cancel()
		}
		a.runGen.Store(gen) // which account this sync is for
		a.runOwner.Store(&owner)
		a.replayOwnerMoves()
		if err := a.pending.Ready(); err != nil {
			return err
		}
		err := run(ctx, reason, manual)
		if err != nil {
			return err
		}
		return a.settleDeletions(ctx, owner)
	}

	a.Account = &account.Service{Client: syncclient.New(cfg.BaseURL), Store: a.Settings, AccountFile: filepath.Join(cfg.Layout.Root, "sync", "account.txt"), BeforeLoginChange: a.endAccountWork, AfterLoginChange: a.beginAccountWork}
	if cfg.Autostart == nil {
		cfg.Autostart = autostart.Default()
	}
	a.Prefs = &prefs.Controller{Settings: a.Settings, Autostart: cfg.Autostart}

	manifestURL := cfg.ManifestURL
	if manifestURL == "" {
		manifestURL = UpdateManifestURL
	}
	exe := cfg.Exe
	if exe == "" {
		exe, _ = os.Executable()
	}
	a.Updates = update.NewCoordinator(&update.Client{ManifestURL: manifestURL, Current: version.Version},
		filepath.Join(cfg.Layout.Root, "updates"), exe, func() {
			if cfg.Quit != nil {
				cfg.Quit()
			}
		})
	a.Updates.OnChange = a.changed
	return a, nil
}

func (a *App) applySettings(st model.Settings) {
	a.Capture.ApplySettings(st)
	a.History.ApplyLimits(st)
}

func (a *App) changed() {
	if a.cfg.Changed != nil {
		a.cfg.Changed()
	}
}

// Start begins capturing, watching the server and the first sync. It returns
// at once. The clipboard must be usable by then (MyGo: after App.WhenReady).
func (a *App) Start() {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return
	}
	a.started = true
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.ctx = ctx
	a.acctCtx, a.acctCancel = context.WithCancel(ctx)
	a.mu.Unlock()

	// A change of the history is a reason to sync, a moment later.
	a.History.OnChanged(func() {
		a.syncMu.Lock()
		if a.syncing.Load() > 0 {
			// A sync is running and has taken its picture of the history
			// already; Auto turns this away, so it is kept for after. What the
			// sync itself changes is not that.
			over := a.syncOver
			if !a.Auto.Suppressed() && !over {
				a.changedDuringSync.Store(true)
			}
			a.syncMu.Unlock()
			if over && !a.Auto.Suppressed() {
				// The sync has handed in its notes and is only finishing: this
				// one is asked for after it, on its own.
				time.AfterFunc(200*time.Millisecond, func() {
					if ctx := a.acct(); ctx.Err() == nil {
						a.Auto.OnHistoryChanged(ctx)
					}
				})
			}
			return
		}
		a.syncMu.Unlock()
		a.Auto.OnHistoryChanged(a.acct())
	})

	// A capture that gave way to an Apply, or could not read a busy
	// clipboard, comes back here.
	a.Capture.Retry = func() {
		if err := a.Capture.Capture(ctx); err != nil {
			a.cfg.Logf("capture: %v", err)
		}
	}
	a.Capture.RetryGeneration = func(gen uint64, key string) {
		if err := a.Capture.CaptureRetry(ctx, gen, key); err != nil {
			a.cfg.Logf("capture retry: %v", err)
		}
	}
	w := &platform.Watcher{Interval: a.cfg.Interval, Seq: a.Capture.Seq, OnChange: func() {
		if err := a.Capture.Capture(ctx); err != nil {
			a.cfg.Logf("capture: %v", err)
		}
	}, Retry: a.Capture.FoundNothing}
	a.Capture.CapturedToken = w.Acknowledge
	a.done.Add(1)
	go func() { defer a.done.Done(); w.Run(ctx) }()
	a.watchRemote(ctx)
	a.Auto.Startup(a.acct())
	a.retryStartup(ctx)

	// What earlier updates left, and what the .NET program left; then the
	// check for a new version, a little after the start and every 12 hours,
	// without a word to the user.
	go func() {
		a.Updates.CleanOld()
		installDir := filepath.Dir(a.Updates.Exe)
		if removed, err := update.CleanLegacy(installDir, filepath.Join(a.Updates.Root, "installed-package-files.json")); err != nil {
			a.cfg.Logf("legacy files: %v", err)
		} else if len(removed) > 0 {
			a.cfg.Logf("removed %d files of the old program", len(removed))
		}
	}()
	a.done.Add(1)
	go func() {
		defer a.done.Done()
		first, every := a.cfg.UpdateFirst, a.cfg.UpdateEvery
		if first <= 0 {
			first = 6 * time.Second
		}
		if every <= 0 {
			every = 12 * time.Hour
		}
		t := time.NewTimer(first)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.Updates.Refresh(ctx)
				t.Reset(every)
			}
		}
	}()
}

// stopWait is how long Stop waits for the background work: a worker that is
// inside a clipboard call needs the UI thread, which is where Stop may be
// running (it is a quit callback), so waiting for ever could be waiting for
// ourselves.
const stopWait = 3 * time.Second

// retryStartup tries the first background sync again, 30 s, 1 min, 2 min ...
// up to every 10 minutes, until one has gone through: history-triggered syncing
// only starts after it, and a program started offline would otherwise wait for
// another device to speak first.
func (a *App) retryStartup(ctx context.Context) {
	if !a.retrying.CompareAndSwap(false, true) {
		return // one loop is enough
	}
	a.done.Add(1)
	go func() {
		defer a.done.Done()
		defer func() {
			a.retrying.Store(false)
			// A sign-in may have asked for a loop while this one was ending: look
			// again after letting go, so that one of the two is always left.
			if ctx.Err() == nil && !a.initialOK.Load() {
				a.retryStartup(ctx)
			}
		}()
		wait := 30 * time.Second
		if a.cfg.StartupRetry > 0 {
			wait = a.cfg.StartupRetry
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			if a.initialOK.Load() {
				return
			}
			a.Auto.Trigger(a.acct(), "startup", false)
			wait = min(wait*2, 10*time.Minute)
		}
	}()
}

// watchRemote listens for uploads from other devices. The stream belongs to
// the signed-in account, so it has a context of its own, ended and started
// again when the account changes (the old connection would go on telling
// about the old account).
func (a *App) watchRemote(ctx context.Context) {
	wctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if a.stopWatch != nil {
		a.stopWatch()
	}
	a.stopWatch = cancel
	a.mu.Unlock()
	a.done.Add(1)
	go func() {
		defer a.done.Done()
		sess := &syncclient.Session{
			Client: syncclient.New(a.cfg.BaseURL),
			Settings: syncclient.Settings{
				Get:    a.Settings.Get,
				Update: a.Settings.Update,
			},
			BaseURL: a.cfg.BaseURL,
		}
		sess.Client.Watch(wctx, sess, syncclient.WatchOptions{}, func() {
			a.syncMu.Lock()
			if a.syncing.Load() > 0 {
				// The runner would turn it away; it is kept for when this one ends.
				over := a.syncOver
				if !over {
					a.remoteDuringSync.Store(true)
				}
				a.syncMu.Unlock()
				if over {
					time.AfterFunc(200*time.Millisecond, func() {
						if ctx := a.acct(); ctx.Err() == nil {
							a.Auto.OnRemoteUpdated(ctx)
						}
					})
				}
				return
			}
			a.syncMu.Unlock()
			a.Auto.OnRemoteUpdated(a.acct())
		})
	}()
}

// Stop ends the background work and writes the history.
func (a *App) Stop() {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		until := time.Now().Add(stopWait)
		done := make(chan struct{})
		go func() { a.done.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(stopWait):
			a.cfg.Logf("stop: background work did not end in %v", stopWait)
		}
		// The syncs started from Auto are not in done; they were cancelled
		// with the context and end soon.
		for a.syncing.Load() > 0 && time.Now().Before(until) {
			time.Sleep(10 * time.Millisecond)
		}
		if a.syncing.Load() > 0 {
			a.cfg.Logf("stop: a sync did not end")
		}
	}
	if err := a.Persister.Close(); err != nil {
		a.cfg.Logf("history: %v", err)
	}
}

// acct is the context of the work that belongs to the account signed in now: it
// ends when the account changes, which stops a sync of the old account before
// it can put its history into the new one's place.
func (a *App) acct() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.acctCtx != nil {
		return a.acctCtx
	}
	return context.Background()
}

// acctWork is acct with the account generation and the address of the account
// it belongs to, read so that they agree: a change of login moves the
// generation and ends the context before it stores the new address, and the
// new context comes after it.
func (a *App) acctWork() (ctx context.Context, gen int64, owner string) {
	for {
		a.mu.Lock()
		ctx, gen = a.acctCtx, a.acctGen.Load()
		a.mu.Unlock()
		if ctx == nil {
			ctx = context.Background()
		}
		owner = a.owner()
		if a.acctGen.Load() == gen {
			return ctx, gen, owner
		}
	}
}

// loginSeen is the address a login had when it was put in place, and the
// account generation it was put in place in.
type loginSeen struct {
	owner string
	gen   int64
}

// ownerLearned watches each change of the settings, as it is made: a login
// known only by its session (sessionOwner: an older client's login with no
// address and no token naming one) that learns its address, or gets a new
// refresh token, with a refresh is the same login, and what it noted is now
// under its new name. A change of login is not that: it moves the generation
// (endAccountWork) before it stores the new login, and the login it puts in
// place is written down only after (beginAccountWork).
//
// When the move cannot be written to the deletions' file, it is written with
// the settings that name the new address (ownerMove), and made again
// (replayOwnerMoves) until it is: after a restart nothing else tells whose
// what was noted under the session is.
func (a *App) ownerLearned(before model.Settings, after *model.Settings) {
	was, is := ownerOf(before), ownerOf(*after)
	if was == is || !sessionOwner(was) || is == "" {
		return
	}
	l := a.login.Load()
	if l == nil || l.owner != was || l.gen != a.acctGen.Load() {
		return
	}
	a.login.Store(&loginSeen{owner: is, gen: l.gen})
	if err := a.pending.Adopt(was, is); err != nil {
		a.cfg.Logf("pending deletions: %v", err)
		setOwnerMoves(after, append(ownerMoves(*after), ownerMove{From: was, To: is}))
	}
}

// ownerMovesKey is where the settings keep the moves of deletions that
// ownerLearned could not write: a property of this client's that the C# one
// keeps as it is (Extra).
const ownerMovesKey = "KlipPendingDeleteMoves"

// ownerMove is that what was noted under the address From is To's.
type ownerMove struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func ownerMoves(st model.Settings) []ownerMove {
	var ms []ownerMove
	if raw, ok := st.Extra[ownerMovesKey]; ok {
		_ = json.Unmarshal(raw, &ms)
	}
	return ms
}

// setOwnerMoves replaces the moves st keeps, in a new map: the one st has is
// shared with the settings given out before.
func setOwnerMoves(st *model.Settings, ms []ownerMove) {
	extra := maps.Clone(st.Extra)
	if extra == nil {
		extra = map[string]json.RawMessage{}
	}
	delete(extra, ownerMovesKey)
	if len(ms) > 0 {
		raw, _ := json.Marshal(ms)
		extra[ownerMovesKey] = raw
	}
	if len(extra) == 0 {
		extra = nil
	}
	st.Extra = extra
}

// replayOwnerMoves makes again the moves the settings keep, in order, and
// forgets those that are written now.
func (a *App) replayOwnerMoves() {
	var made []ownerMove
	for _, m := range ownerMoves(a.Settings.Get()) {
		if err := a.pending.Move(m.From, m.To); err != nil {
			a.cfg.Logf("pending deletions: %v", err)
			break
		}
		made = append(made, m)
	}
	if len(made) == 0 {
		return
	}
	err := a.Settings.Update(func(s *model.Settings) {
		ms := ownerMoves(*s)
		for _, m := range made {
			if i := slices.Index(ms, m); i >= 0 {
				ms = slices.Delete(ms, i, i+1)
			}
		}
		setOwnerMoves(s, ms)
	})
	if err != nil {
		a.cfg.Logf("settings: %v", err)
	}
}

// workOwner is the account the sync or the upload running was started for
// (one runs at a time: Auto), read once at its start with its generation.
func (a *App) workOwner() string {
	if o := a.runOwner.Load(); o != nil {
		return *o
	}
	return a.owner()
}

// owner is the address of the account signed in, whose the pending deletions
// are (see ownerOf).
func (a *App) owner() string { return ownerOf(a.Settings.Get()) }

// ownerOf is whose the deletions made under the login in st are: its address.
// A login made by an older client may have none stored; its access token,
// which the server issued to it and which names the account it was issued to,
// gives it then. A login with nothing but a refresh token is known by that
// session alone ("session:" and a digest of the token): its deletions are its
// own, no other login's, and become its account's once a refresh tells its
// address (ownerLearned). If it ends before that, nothing ties them to an
// account, as nothing ties its checkpoints to one: they are not given to
// whoever signs in next. "" when there is no login.
func ownerOf(st model.Settings) string {
	if e := strings.TrimSpace(model.Str(st.AuthUserEmail)); e != "" {
		return e
	}
	for _, tok := range []string{model.Str(st.AuthAccessToken), model.Str(st.AuthIdToken)} {
		if e := tokenEmail(tok); e != "" {
			return e
		}
	}
	if r := strings.TrimSpace(model.Str(st.AuthRefreshToken)); r != "" {
		sum := sha256.Sum256([]byte(r))
		return sessionPrefix + hex.EncodeToString(sum[:8])
	}
	return ""
}

const sessionPrefix = "session:"

// sessionOwner says that owner names a login by its session (or names none),
// not an account.
func sessionOwner(owner string) bool { return owner == "" || strings.HasPrefix(owner, sessionPrefix) }

// tokenEmail is the address a JWT names ("" when tok is not one, or names
// none). The signature is not checked: the token is this login's own, as kept
// in the settings, and is used to name the account, not to grant anything.
func tokenEmail(tok string) string {
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) != 3 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return ""
	}
	for _, k := range []string{"email", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress"} {
		if e, ok := claims[k].(string); ok && strings.TrimSpace(e) != "" {
			return strings.TrimSpace(e)
		}
	}
	return ""
}

// Apply puts an item on the clipboard and moves it to the top, as the main
// window does when one is chosen.
func (a *App) Apply(it model.Item) error {
	if err := a.Capture.Apply(it); err != nil {
		a.cfg.Logf("apply: %v", err)
		return err
	}
	a.History.Touch(it.ID)
	return nil
}

// SyncNow is the user asking for a sync.
func (a *App) SyncNow() {
	// The status and the bar are set when the run starts (Auto.Begin): a request
	// that is turned away because one is still unwinding changes nothing.
	a.Auto.Trigger(a.acct(), "manual", true)
}

// endAccountWork is called by the account service just before it replaces or
// ends the login: what the old account was doing (a sync, the event stream's
// callbacks) is stopped before the new tokens are in place, and a sync that
// still manages to finish is known to be of the old account.
//
// The deletions owed to the server stay with the account that owes them
// (pendingDeletes, one list for each): a sign-out or a sign-in to the same
// account keeps them, and the old account's work, which is not waited for
// here (the account service is holding the login), can only write to its own.
func (a *App) endAccountWork() {
	a.mu.Lock()
	a.acctGen.Add(1)
	if a.started {
		a.acctCancel()
		// Until the new login is in place the account context is a dead one: a
		// callback or timer of the old account that fires now finds its work
		// refused instead of starting with tokens about to be replaced.
		dead, stop := context.WithCancel(a.ctx)
		stop()
		a.acctCtx, a.acctCancel = dead, func() {}
	}
	a.pending.ForgetAliases()
	a.mu.Unlock()
}

// beginAccountWork is called by the account service once the new login (or
// none) is stored: the work of the account that is signed in now may start.
func (a *App) beginAccountWork() {
	a.login.Store(&loginSeen{owner: a.owner(), gen: a.acctGen.Load()})
	a.mu.Lock()
	if a.started {
		a.acctCtx, a.acctCancel = context.WithCancel(a.ctx)
	}
	a.mu.Unlock()
}

// AccountChanged is for the preferences window: after a sign-in the history
// is synchronized at once, as the C# client does.
func (a *App) AccountChanged(signedIn bool) {
	a.mu.Lock()
	ctx, started := a.ctx, a.started
	a.mu.Unlock()
	if started {
		a.watchRemote(ctx) // the stream of the old account is dropped, a new one opened
	}
	if signedIn {
		a.loginWanted.Store(true)
		a.initialOK.Store(false)
		a.failStreak.Store(0)
		a.Auto.Trigger(a.acct(), "login", false)
		if started {
			a.retryStartup(ctx) // and again later if that sync does not go through
		}
	}
	a.changed()
}

// SyncStatus is the text for the window's footer: how the last sync went.
func (a *App) SyncStatus() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

func (a *App) setStatus(s string) {
	a.mu.Lock()
	a.status = s
	a.mu.Unlock()
	a.changed()
}

func (a *App) syncDone(reason string, manual bool, err error) {
	// The sync counts as running until its result has been dealt with: Stop
	// waits for that, and nothing may use the program (the log) after it.
	// A request turned away before it ran (a manual sync with nobody signed in)
	// has no run of its own to end; one that was entered and then found itself
	// signed out has, whatever error it returns. Run says which it was.
	ran := a.runEntered.Swap(false)
	defer func() {
		if ran && a.runActive.Swap(false) {
			a.syncing.Add(-1)
		}
	}()
	a.syncMu.Lock()
	changed, remote := a.changedDuringSync.Load(), a.remoteDuringSync.Load()
	if err == nil {
		a.changedDuringSync.Store(false)
		a.remoteDuringSync.Store(false)
	}
	a.syncOver = true // from here a change is asked for on its own
	a.syncMu.Unlock()
	if changed {
		// Changes that came in while it ran were not part of it: sync again
		// when it is over (the runner is busy until this returns). After a
		// failure the flag stays and the next try is later.
		wait := 150 * time.Millisecond
		if err != nil {
			wait = 30 * time.Second
		}
		time.AfterFunc(wait, func() {
			if ctx := a.acct(); ctx.Err() == nil {
				a.Auto.OnHistoryChanged(ctx)
			}
		})
	}
	if remote {
		wait := 150 * time.Millisecond
		if err != nil {
			wait = 30 * time.Second
		}
		time.AfterFunc(wait, func() {
			if ctx := a.acct(); ctx.Err() == nil {
				a.Auto.OnRemoteUpdated(ctx)
			}
		})
	}
	// A sync that began before the account changed says nothing about the new
	// account: it neither counts as its first sync nor settles what it is owed.
	current := a.runGen.Load() == a.acctGen.Load()
	switch {
	case err == nil:
		a.failStreak.Store(0)
	case !manual && !changed && !remote && current && a.initialOK.Load() && !errors.Is(err, syncer.ErrNotLoggedIn):
		// A sync after the first one (a copied item, a remote event) failed and
		// nothing else is pending, so nothing would ask again until the next
		// copy: try again later, further apart each time it fails.
		n := a.failStreak.Add(1)
		base := 30 * time.Second
		if a.cfg.StartupRetry > 0 {
			base = a.cfg.StartupRetry
		}
		wait := base << min(n-1, 4) // 30 s ... 8 min
		time.AfterFunc(wait, func() {
			if ctx := a.acct(); ctx.Err() == nil {
				a.Auto.OnHistoryChanged(ctx)
			}
		})
	}
	if err == nil && !manual && current {
		a.initialOK.Store(true)
	}
	if reason == "login" && current {
		a.loginWanted.Store(false)
	} else if a.loginWanted.Load() {
		// The sync for the new sign-in was asked for while this one ran and was
		// turned away. The runner is still marked busy until this returns, so
		// ask again a moment later.
		time.AfterFunc(100*time.Millisecond, func() {
			if ctx := a.acct(); ctx.Err() == nil {
				a.Auto.Trigger(ctx, "login", false)
			}
		})
	}
	a.finishView(reason, manual, err)
}
