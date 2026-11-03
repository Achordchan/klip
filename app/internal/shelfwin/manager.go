package shelfwin

import (
	"sync"
	"sync/atomic"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// Manager opens shelves (ShelfService's window half): each is a window of its
// own near the cursor. Any number can be open.
type Manager struct {
	epoch   atomic.Int64 // moves when the shelves are closed
	mu      sync.Mutex
	shelves map[*Model]*mygo.Window
}

// NewManager returns a Manager with no shelves open.
func NewManager() *Manager { return &Manager{shelves: map[*Model]*mygo.Window{}} }

type host struct {
	mgr *Manager
	m   *Model
	win *mygo.Window
}

func (h *host) Close() { h.mgr.remove(h.m) }

// Resize changes the size and keeps the window inside the work area: a shelf
// that was made near an edge would otherwise grow off the screen.
func (h *host) Resize(size int) {
	h.win.SetSize(size, size)
	x, y := h.win.Position()
	wa := mygo.Screen.DisplayNearestPoint(mygo.Point{X: x + size/2, Y: y + size/2}).WorkArea
	nx := max(wa.X, min(x, wa.X+wa.Width-size))
	ny := max(wa.Y, min(y, wa.Y+wa.Height-size))
	if nx != x || ny != y {
		h.win.SetPosition(nx, ny)
	}
}

// Spawn opens a shelf below and to the right of the cursor, kept inside the
// work area of that display (PositionShelf). Any goroutine may call it.
func (mg *Manager) Spawn() {
	epoch := mg.epoch.Load()
	// Not waited for: the caller is the thread that reads the mouse, and the UI
	// thread may at that moment be waiting for it (to stop it). A spawn that was
	// asked for before the shelves were closed is dropped when its turn comes.
	go mygo.RunOnMain(func() {
		if mg.epoch.Load() != epoch {
			return
		}
		m := New(nil)
		m.IconOf = SystemIcon
		cur := mygo.Screen.CursorScreenPoint()
		d := mygo.Screen.DisplayNearestPoint(cur)
		x, y := cur.X+18, cur.Y+18
		wa := d.WorkArea
		x = max(wa.X, min(x, wa.X+wa.Width-CompactSize))
		y = max(wa.Y, min(y, wa.Y+wa.Height-CompactSize))

		win := mygo.NewWindow(mygo.WindowOptions{
			Title:         "Klip 货架",
			Panel:         true, // does not take the focus from the program being dragged from
			Frameless:     true,
			Transparent:   true,
			AlwaysOnTop:   true,
			SkipTaskbar:   true,
			DisableResize: true,
			Hidden:        true,
			Width:         CompactSize,
			Height:        CompactSize,
			Content:       ui.View(m.View),
		})
		m.Post = func(f func()) { win.Update(f) }
		h := &host{mgr: mg, m: m, win: win}
		m.Host = h
		win.OnClose(func(e *mygo.CloseEvent) { mg.forget(m) })
		win.SetPosition(x, y)
		mg.mu.Lock()
		mg.shelves[m] = win
		mg.mu.Unlock()
		win.ShowInactive()
	})
}

func (mg *Manager) forget(m *Model) {
	mg.mu.Lock()
	delete(mg.shelves, m)
	mg.mu.Unlock()
}

func (mg *Manager) remove(m *Model) {
	mg.mu.Lock()
	win := mg.shelves[m]
	delete(mg.shelves, m)
	mg.mu.Unlock()
	if win != nil {
		win.Close()
	}
}

// CloseAll closes every open shelf.
func (mg *Manager) CloseAll() {
	mg.epoch.Add(1) // pending spawns are void
	mg.mu.Lock()
	wins := make([]*mygo.Window, 0, len(mg.shelves))
	for _, w := range mg.shelves {
		wins = append(wins, w)
	}
	mg.shelves = map[*Model]*mygo.Window{}
	mg.mu.Unlock()
	for _, w := range wins {
		w.Close()
	}
}

// Count is the number of open shelves.
func (mg *Manager) Count() int {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	return len(mg.shelves)
}
