// Package platform is where Klip meets the operating system: the MyGo
// clipboard behind capture.Clipboard, the clipboard change counter, and the
// program that is in front. Everything that needs a window or a native call
// lives here, so the other packages stay testable without one.
package platform

import (
	"context"
	"sync"
	"time"
)

// DefaultInterval is how often the change counter is asked. The counter is
// cheap (about 0.03% CPU at this rate, measured in spikes/), so this is the
// delay between a copy and its capture.
const DefaultInterval = 400 * time.Millisecond

// Watcher tells when the clipboard changed. It asks Seq every Interval and
// calls OnChange when the number moved. A negative Seq means the platform has
// no counter; then Fingerprint (the content, see ContentFingerprint) says
// whether anything changed, and without it OnChange is called every tick.
//
// The first tick only records the counter: whatever was on the clipboard
// before Klip started is not captured, as in the C# client.
type Watcher struct {
	Interval time.Duration
	Seq      func() int64
	OnChange func()
	// Fingerprint stands in for the counter when Seq is negative; may be nil.
	Fingerprint func() string

	mu     sync.Mutex
	last   int64
	seen   bool
	lastFP string
}

// Run blocks until ctx ends. OnChange runs on the calling goroutine, one at
// a time; MyGo hops each clipboard call to its main thread.
func (w *Watcher) Run(ctx context.Context) {
	iv := w.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	seq := w.Seq
	if seq == nil {
		seq = ChangeSeq
		if w.Fingerprint == nil {
			w.Fingerprint = ContentFingerprint // used only when ChangeSeq has no counter
		}
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := seq()
			var moved bool
			if s < 0 && w.Fingerprint != nil {
				moved = w.contentMoved(w.Fingerprint())
			} else {
				moved = w.moved(s)
			}
			if moved && w.OnChange != nil {
				w.OnChange()
			}
		}
	}
}

// moved says whether the counter shows a change since the last call.
func (w *Watcher) moved(s int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if s < 0 {
		was := w.seen // no counter: after the first tick, always look
		w.seen = true
		return was
	}
	if !w.seen {
		w.seen, w.last = true, s
		return false
	}
	if s == w.last {
		return false
	}
	w.last = s
	return true
}

// contentMoved is moved for the fallback: the first fingerprint only records
// itself, a different one is a change.
func (w *Watcher) contentMoved(fp string) bool {
	if fp == "" {
		w.mu.Lock()
		if !w.seen {
			w.seen = true
		}
		w.mu.Unlock()
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	was := w.seen
	w.seen = true
	if !was {
		w.lastFP = fp
		return false
	}
	if fp == w.lastFP {
		return false
	}
	w.lastFP = fp
	return true
}

// Acknowledge records the exact content captured, not a later clipboard read.
func (w *Watcher) Acknowledge(fp string) { w.mu.Lock(); w.lastFP = fp; w.seen = true; w.mu.Unlock() }
