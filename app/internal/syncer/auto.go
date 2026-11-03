package syncer

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Auto decides when a background sync starts: at startup, when the server
// says another device uploaded, and a moment after the history changed. It
// is the part of MainWindowViewModel around RunAutoSyncAsync: one sync at a
// time, none for a change the sync itself made, none for a change before the
// first sync has succeeded, and a pause so that a burst of changes is one
// sync.
type Auto struct {
	// Debounce is the pause after a change before the sync starts (1.2 s).
	Debounce time.Duration
	// CanSync says whether a sync can be tried (Syncer.CanAutoSync).
	CanSync func() bool
	// Run does the sync; manual says that the user asked for it.
	Run func(ctx context.Context, reason string, manual bool) error
	// Done is called after each sync, with its error (nil on success).
	Done func(reason string, manual bool, err error)
	// Context, when set, is the application's lifetime context for a follow-up
	// run: the trigger that queued it may have been cancelled by then.
	Context context.Context

	busy        atomic.Bool
	suppressed  atomic.Bool
	initialDone atomic.Bool
	// pending says a history change came while a sync was running; another
	// sync follows when it ends, so that the change is not left unsynced.
	pending atomic.Bool

	// startMu makes the busy/pending transition atomic: a trigger that cannot
	// start records pending before the finish of the running sync can look,
	// and a finish that clears busy either sees the pending work or the
	// trigger starts the next run itself.
	startMu sync.Mutex

	mu    sync.Mutex
	timer *time.Timer
}

// NewAuto returns an Auto that runs s. A change of the history that the
// sync's suppression hid is told to it (Syncer.mutateHistory).
func NewAuto(s *Syncer, done func(reason string, manual bool, err error)) *Auto {
	a := &Auto{
		CanSync: s.CanAutoSync,
		Run: func(ctx context.Context, reason string, manual bool) error {
			_, err := s.Run(ctx, nil, reason)
			return err
		},
		Done: done,
	}
	s.changed = func() { a.OnHistoryChanged(a.baseContext()) }
	return a
}

func (a *Auto) debounce() time.Duration {
	if a.Debounce > 0 {
		return a.Debounce
	}
	return 1200 * time.Millisecond
}

// Suppress turns off the reaction to history changes while the sync changes
// the history itself.
func (a *Auto) Suppress(on bool) { a.suppressed.Store(on) }

// Trigger runs a sync now, unless one is running. A manual one that cannot
// start because there is no sign-in reports ErrNotLoggedIn.
func (a *Auto) Trigger(ctx context.Context, reason string, manual bool) {
	a.startMu.Lock()
	if a.busy.Load() {
		// A background trigger that cannot start is remembered: it may be the
		// only notice of a change or of another device's upload. A manual one
		// is the user asking again, and is dropped as before.
		if !manual {
			a.pending.Store(true)
		}
		a.startMu.Unlock()
		return
	}
	a.busy.Store(true)
	a.startMu.Unlock()
	go a.perform(ctx, reason, manual)
}

// perform runs one sync and then hands over to pending work, if any.
func (a *Auto) perform(ctx context.Context, reason string, manual bool) {
	if !a.CanSync() {
		if manual && a.Done != nil {
			a.Done(reason, manual, ErrNotLoggedIn)
		}
		a.finish(ctx)
		return
	}
	err := a.Run(ctx, reason, manual)
	if err == nil && !manual {
		a.initialDone.Store(true)
	}
	if a.Done != nil {
		a.Done(reason, manual, err)
	}
	a.finish(ctx)
}

// finish clears busy and starts a remembered run atomically with Trigger. A
// suppressed change (the sync's own write) is not started.
func (a *Auto) finish(ctx context.Context) {
	a.startMu.Lock()
	a.busy.Store(false)
	if a.pending.Swap(false) && !a.suppressed.Load() && a.CanSync() {
		a.busy.Store(true)
		a.startMu.Unlock()
		go a.perform(a.baseContext(), "auto", false)
		return
	}
	a.startMu.Unlock()
}

// Startup is the first background sync.
func (a *Auto) Startup(ctx context.Context) { a.Trigger(ctx, "startup", false) }

// OnRemoteUpdated is for the event stream: another device uploaded. An event
// that arrives while a sync runs is kept (Trigger records it) and synced after
// it: the running sync may have read an older manifest.
func (a *Auto) OnRemoteUpdated(ctx context.Context) { a.Trigger(ctx, "remote_event", false) }

// baseContext is the application's lifetime context, or Background.
func (a *Auto) baseContext() context.Context {
	if a.Context != nil {
		return a.Context
	}
	return context.Background()
}

// OnHistoryChanged is for each change of the history. A sync follows after
// the pause, unless another change comes first. A change that arrives while
// a sync is running makes that sync run again when it ends: the sync has
// already read the history it uploaded.
func (a *Auto) OnHistoryChanged(ctx context.Context) {
	if a.suppressed.Load() || !a.CanSync() {
		return
	}
	if !a.initialDone.Load() {
		// Before the first success changes wait, as in C#; a capture made
		// while the first sync is running is kept for the run after it.
		a.startMu.Lock()
		if a.initialDone.Load() {
			// The first sync finished while this notification waited: treat
			// it as a normal change instead of dropping it.
			a.startMu.Unlock()
		} else {
			if a.busy.Load() {
				a.pending.Store(true)
			}
			a.startMu.Unlock()
			return
		}
	}
	if a.busy.Load() {
		a.startMu.Lock()
		if a.busy.Load() {
			a.pending.Store(true)
			a.startMu.Unlock()
			return
		}
		a.startMu.Unlock()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
	}
	a.timer = time.AfterFunc(a.debounce(), func() { a.Trigger(ctx, "auto", false) })
}
