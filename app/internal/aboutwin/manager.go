package aboutwin

import (
	"sync"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// Manager owns the window: opened under the About chip, closed by its own
// button, by pressing the chip again, or when the main window goes.
type Manager struct {
	Version string
	// Quitting says the program is quitting: the window then lets itself be
	// closed instead of hiding, or Quit would wait for it for ever.
	Quitting func() bool

	mu    sync.Mutex
	model *Model
	win   *mygo.Window
}

// Toggle opens the window under the anchor (a point in screen pixels: the
// middle of the lower edge of the chip), or closes it when it is open.
func (mg *Manager) Toggle(anchorX, anchorY int) {
	mygo.RunOnMain(func() {
		mg.mu.Lock()
		defer mg.mu.Unlock()
		if mg.win != nil && mg.win.IsVisible() {
			mg.win.Hide()
			return
		}
		if mg.model == nil {
			mg.model = &Model{
				Version: mg.Version,
				Open:    func(u string) { _ = mygo.Shell.OpenExternal(u) },
				Close:   mg.Close,
			}
		}
		if mg.win == nil {
			mg.win = mygo.NewWindow(mygo.WindowOptions{
				Title:         "关于 Klip",
				Panel:         true,
				Frameless:     true,
				Transparent:   true,
				AlwaysOnTop:   true,
				SkipTaskbar:   true,
				DisableResize: true,
				Hidden:        true,
				Width:         Width,
				Height:        Height,
				Content:       ui.View(mg.model.View),
			})
			mg.model.Redraw = mg.redraw
			mg.win.OnClose(func(e *mygo.CloseEvent) {
				if mg.Quitting != nil && mg.Quitting() {
					return
				}
				e.PreventDefault()
				mg.win.Hide()
			})
		}
		x, y := Place(anchorX, anchorY+10, mg.workArea(anchorX, anchorY))
		mg.win.SetPosition(x, y)
		mg.win.ShowInactive()
		mg.model.LoadAvatar()
	})
}

func (mg *Manager) workArea(x, y int) [4]int {
	d := mygo.Screen.DisplayNearestPoint(mygo.Point{X: x, Y: y})
	wa := d.WorkArea
	return [4]int{wa.X, wa.Y, wa.Width, wa.Height}
}

// Place is where the window goes: centred on x, under y, and kept inside
// the work area (x, y, width, height).
func Place(x, y int, wa [4]int) (int, int) {
	x -= Width / 2
	x = max(wa[0], min(x, max(wa[0], wa[0]+wa[2]-Width)))
	y = max(wa[1], min(y, max(wa[1], wa[1]+wa[3]-Height)))
	return x, y
}

// Close hides the window.
func (mg *Manager) Close() {
	mygo.RunOnMain(func() {
		mg.mu.Lock()
		defer mg.mu.Unlock()
		if mg.win != nil {
			mg.win.Hide()
		}
	})
}

// redraw asks for a frame, from any goroutine (the picture of the author
// arrives on its own): on the UI thread, where Destroy also runs, and only
// while the window is there.
func (mg *Manager) redraw() {
	mygo.RunOnMain(func() {
		mg.mu.Lock()
		defer mg.mu.Unlock()
		if mg.win != nil {
			mg.win.Update(func() {})
		}
	})
}

// Destroy ends the window for good (when the program quits). It runs on the
// UI thread, so that a redraw cannot reach the window while it goes.
func (mg *Manager) Destroy() {
	mygo.RunOnMain(func() {
		mg.mu.Lock()
		w := mg.win
		mg.win = nil
		mg.mu.Unlock()
		if w != nil {
			w.Close()
		}
	})
}
