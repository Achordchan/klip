package history

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"klip/internal/model"
	"klip/internal/paths"
)

// Persister keeps history.json up to date: it saves the list a short while
// after it changed (ClipboardPersistenceService).
type Persister struct {
	svc    *Service
	layout paths.Layout
	// Delay is the wait before a save, 350 ms.
	Delay time.Duration

	scheduled atomic.Bool
	mu        sync.Mutex
	timer     *time.Timer
	closed    bool
	fails     int // saves in a row that failed
	saveMu    sync.Mutex
}

// NewPersister saves svc's list under layout whenever it changes.
func NewPersister(svc *Service, layout paths.Layout) *Persister {
	p := &Persister{svc: svc, layout: layout, Delay: 350 * time.Millisecond}
	svc.mu.Lock()
	svc.deferCleanup = true
	svc.mu.Unlock()
	svc.OnChanged(p.onChanged)
	return p
}

// Load reads history.json into the service (the items whose content is gone
// are dropped, see Normalize). A missing file is an empty history.
func (p *Persister) Load(ctx context.Context) error {
	items, err := Load(p.layout)
	if err != nil {
		return err
	}
	p.svc.mu.Lock()
	p.svc.mutation = SavedMutation(p.layout)
	p.svc.mu.Unlock()
	return p.svc.ReplaceAll(ctx, items)
}

// Flush saves now.
func (p *Persister) Flush(context.Context) error { return p.save() }

// Close stops the timer and saves once more.
func (p *Persister) Close() error {
	p.mu.Lock()
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
	}
	p.mu.Unlock()
	return p.save()
}

func (p *Persister) save() error {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.svc.mu.Lock()
	defer p.svc.mu.Unlock()
	items := append([]model.Item(nil), p.svc.items...)
	marker := p.svc.mutation
	trash := p.svc.pendingTrash
	p.svc.pendingTrash = historyTrashEmpty()
	if err := SaveWithMutation(p.layout, items, marker); err != nil {
		p.svc.pendingTrash.images = append(p.svc.pendingTrash.images, trash.images...)
		p.svc.pendingTrash.fileDirs = append(p.svc.pendingTrash.fileDirs, trash.fileDirs...)
		return err
	}
	retireCapturedFiles(p.layout.Files(), items)
	live := p.svc.items
	p.svc.items = append(append([]model.Item(nil), live...), items...)
	p.svc.emptyNow(context.Background(), trash)
	p.svc.items = live
	return nil
}

// onChanged schedules one save: changes during the wait are in that save,
// they do not start another (and do not push it back). A change after the
// snapshot is taken starts the next one.
func (p *Persister) onChanged() {
	if !p.scheduled.CompareAndSwap(false, true) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		p.scheduled.Store(false)
		return
	}
	d := p.Delay
	if d <= 0 {
		d = 350 * time.Millisecond
	}
	p.arm(d)
}

// arm sets the timer for a save; the caller holds mu.
func (p *Persister) arm(d time.Duration) {
	p.timer = time.AfterFunc(d, func() {
		// Clear the flag before the snapshot: a change that comes while the
		// disk is written then schedules the next save instead of being lost.
		p.scheduled.Store(false)
		if err := p.save(); err != nil {
			p.retry()
			return
		}
		p.mu.Lock()
		p.fails = 0
		p.mu.Unlock()
	})
}

// retry tries a failed save again with a growing wait (the disk may be full or
// away for a while): 1 s, 2 s, 4 s ... up to 30 s. A change that comes
// meanwhile joins the same save.
func (p *Persister) retry() {
	if !p.scheduled.CompareAndSwap(false, true) {
		return // a change has scheduled the next save already
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		p.scheduled.Store(false)
		return
	}
	p.fails = min(p.fails+1, 5)
	p.arm(min(time.Second<<(p.fails-1), 30*time.Second))
}

func historyTrashEmpty() trash { return trash{} }
