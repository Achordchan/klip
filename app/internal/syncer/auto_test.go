package syncer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type autoRig struct {
	a     *Auto
	runs  atomic.Int32
	mu    sync.Mutex
	why   []string
	done  []error
	can   atomic.Bool
	block chan struct{}
	fail  atomic.Bool
}

func (r *autoRig) gate() chan struct{} { r.mu.Lock(); defer r.mu.Unlock(); return r.block }

func (r *autoRig) setGate(c chan struct{}) { r.mu.Lock(); r.block = c; r.mu.Unlock() }

func newRig() *autoRig {
	r := &autoRig{block: make(chan struct{})}
	r.can.Store(true)
	close(r.block)
	r.a = &Auto{
		Debounce: 30 * time.Millisecond,
		CanSync:  r.can.Load,
		Run: func(ctx context.Context, reason string, manual bool) error {
			r.runs.Add(1)
			r.mu.Lock()
			r.why = append(r.why, reason)
			r.mu.Unlock()
			<-r.gate()
			if r.fail.Load() {
				return ErrNotLoggedIn
			}
			return nil
		},
		Done: func(reason string, manual bool, err error) {
			r.mu.Lock()
			r.done = append(r.done, err)
			r.mu.Unlock()
		},
	}
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestAutoChangesWaitForTheFirstSync(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	r.a.OnHistoryChanged(ctx) // before the first sync: ignored
	time.Sleep(80 * time.Millisecond)
	if r.runs.Load() != 0 {
		t.Fatal("a change before the first sync started one")
	}
	r.a.Startup(ctx)
	waitFor(t, "startup sync", func() bool { return r.runs.Load() == 1 })
	waitFor(t, "idle", func() bool { return !r.a.busy.Load() })

	// A burst of changes is one sync, after the pause.
	for i := 0; i < 5; i++ {
		r.a.OnHistoryChanged(ctx)
		time.Sleep(5 * time.Millisecond)
	}
	waitFor(t, "auto sync", func() bool { return r.runs.Load() == 2 })
	time.Sleep(80 * time.Millisecond)
	if r.runs.Load() != 2 {
		t.Errorf("runs %d: the burst should be one sync", r.runs.Load())
	}
	r.mu.Lock()
	why := append([]string(nil), r.why...)
	r.mu.Unlock()
	if why[0] != "startup" || why[1] != "auto" {
		t.Errorf("reasons %v", why)
	}
}

func TestAutoSuppressedBusyAndSignedOut(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	r.a.Startup(ctx)
	waitFor(t, "startup", func() bool { return r.runs.Load() == 1 && !r.a.busy.Load() })

	// The sync's own changes do not start a sync.
	r.a.Suppress(true)
	r.a.OnHistoryChanged(ctx)
	time.Sleep(80 * time.Millisecond)
	r.a.Suppress(false)
	if r.runs.Load() != 1 {
		t.Error("a suppressed change started a sync")
	}

	// Signed out: no sync for a change; a manual one says why.
	r.can.Store(false)
	r.a.OnHistoryChanged(ctx)
	time.Sleep(80 * time.Millisecond)
	if r.runs.Load() != 1 {
		t.Error("a sync without a sign-in")
	}
	r.a.Trigger(ctx, "manual", true)
	waitFor(t, "the manual refusal", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.done) == 2 })
	r.mu.Lock()
	second := r.done[1]
	r.mu.Unlock()
	if second != ErrNotLoggedIn || r.runs.Load() != 1 {
		t.Errorf("manual while signed out: %v, runs %d", second, r.runs.Load())
	}
	// A background trigger while signed out is silent.
	r.a.Trigger(ctx, "remote_event", false)
	time.Sleep(50 * time.Millisecond)
	r.mu.Lock()
	n := len(r.done)
	r.mu.Unlock()
	if n != 2 {
		t.Errorf("done calls %d", n)
	}
	r.can.Store(true)

	// One sync at a time.
	gate := make(chan struct{})
	r.setGate(gate)
	r.a.Trigger(ctx, "one", true)
	waitFor(t, "the running sync", func() bool { return r.runs.Load() == 2 })
	r.a.Trigger(ctx, "two", true)
	r.a.Trigger(ctx, "three", true)
	time.Sleep(50 * time.Millisecond)
	if r.runs.Load() != 2 {
		t.Errorf("runs %d: triggers during a sync are dropped", r.runs.Load())
	}
	close(gate)
	waitFor(t, "end", func() bool { return !r.a.busy.Load() })
}

func TestAutoFailedStartupKeepsChangesQuiet(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	r.fail.Store(true)
	r.a.Startup(ctx)
	waitFor(t, "startup", func() bool { return r.runs.Load() == 1 && !r.a.busy.Load() })
	r.a.OnHistoryChanged(ctx) // the first sync failed: changes still wait
	time.Sleep(80 * time.Millisecond)
	if r.runs.Load() != 1 {
		t.Error("a change after a failed first sync started one")
	}
	// A manual success does not count as the first background sync either.
	r.fail.Store(false)
	r.a.Trigger(ctx, "manual", true)
	waitFor(t, "manual", func() bool { return r.runs.Load() == 2 && !r.a.busy.Load() })
	r.a.OnHistoryChanged(ctx)
	time.Sleep(80 * time.Millisecond)
	if r.runs.Load() != 2 {
		t.Error("only a successful background sync opens the gate (C# _initialSyncCompleted)")
	}
}

// A change that arrives while a sync runs is not dropped: the sync has
// already read the history, so it runs once more when it ends.
func TestAutoSyncsAgainForAChangeDuringASync(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	r.a.Startup(ctx)
	waitFor(t, "startup", func() bool { return r.runs.Load() == 1 && !r.a.busy.Load() })

	gate := make(chan struct{})
	r.setGate(gate)
	r.a.Trigger(ctx, "one", true)
	waitFor(t, "the running sync", func() bool { return r.runs.Load() == 2 })
	r.a.OnHistoryChanged(ctx)
	close(gate)

	waitFor(t, "the follow-up sync", func() bool { return r.runs.Load() == 3 })
	waitFor(t, "idle", func() bool { return !r.a.busy.Load() })
	time.Sleep(80 * time.Millisecond)
	if n := r.runs.Load(); n != 3 {
		t.Errorf("runs %d: a change during a sync should be one more sync", n)
	}
}

// A remote update that arrives while a sync runs is not dropped: the running
// sync may have read an older manifest, so another sync follows.
func TestAutoSyncsAgainForARemoteUpdateDuringASync(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	r.a.Startup(ctx)
	waitFor(t, "startup", func() bool { return r.runs.Load() == 1 && !r.a.busy.Load() })

	gate := make(chan struct{})
	r.setGate(gate)
	r.a.Trigger(ctx, "one", true)
	waitFor(t, "the running sync", func() bool { return r.runs.Load() == 2 })
	r.a.OnRemoteUpdated(ctx)
	close(gate)

	waitFor(t, "the follow-up sync", func() bool { return r.runs.Load() == 3 })
	waitFor(t, "idle", func() bool { return !r.a.busy.Load() })
	time.Sleep(80 * time.Millisecond)
	if n := r.runs.Load(); n != 3 {
		t.Errorf("runs %d: a remote update during a sync should be one more sync", n)
	}
}

// A capture made while the first (startup) sync is running must not be lost:
// the first sync remembers only what it exported, so a follow-up is needed.
func TestAutoKeepsACaptureDuringTheFirstSync(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	gate := make(chan struct{})
	r.setGate(gate)
	r.a.Startup(ctx)
	waitFor(t, "the startup sync", func() bool { return r.runs.Load() == 1 })
	r.a.OnHistoryChanged(ctx) // initialDone is still false, but the sync runs
	close(gate)

	waitFor(t, "the follow-up sync", func() bool { return r.runs.Load() == 2 })
	waitFor(t, "idle", func() bool { return !r.a.busy.Load() })
	time.Sleep(80 * time.Millisecond)
	if n := r.runs.Load(); n != 2 {
		t.Errorf("runs %d: the capture made during the first sync was dropped", n)
	}
}
