package updatewin

import (
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// Manager puts the window the model calls for on screen, in the middle of
// the display the pointer is on, and hides the others. Sync may be called
// from any goroutine, as often as the state changes.
type Manager struct {
	Model *Model

	wins map[Kind]*mygo.Window
	cur  Kind
}

// Sync shows the window of the state, and hides the one that is not wanted.
// A window the user closed stays closed while its state lasts.
func (mg *Manager) Sync() { mg.sync(false) }

// Reshow is Sync that also brings back the window of the state when it was
// closed: for a mandatory update, whose window is the only way on while the
// rest of the program is shut.
func (mg *Manager) Reshow() { mg.sync(true) }

func (mg *Manager) sync(reshow bool) {
	mygo.RunOnMain(func() {
		want := mg.Model.Kind()
		if mg.wins == nil {
			mg.wins = map[Kind]*mygo.Window{}
		}
		for k, w := range mg.wins {
			if k != want && w.IsVisible() {
				w.Hide()
			}
		}
		if want == None {
			mg.cur = None
			return
		}
		w := mg.wins[want]
		if w == nil {
			w = mg.open(want)
			mg.wins[want] = w
		}
		w.SetTitle(mg.Model.Title(want))
		if mg.cur != want || (reshow && !w.IsVisible()) {
			size := Sizes[want]
			wa := mygo.Screen.DisplayNearestPoint(mygo.Screen.CursorScreenPoint()).WorkArea
			w.SetPosition(wa.X+(wa.Width-size[0])/2, wa.Y+(wa.Height-size[1])/2)
			w.Show()
			if want != Check {
				w.Focus()
			}
		}
		mg.cur = want
		w.Update(func() {})
	})
}

func (mg *Manager) open(k Kind) *mygo.Window {
	size := Sizes[k]
	view := map[Kind]func(*ui.Context){Check: mg.Model.ViewCheck, Prompt: mg.Model.ViewPrompt, Progress: mg.Model.ViewProgress, Message: mg.Model.ViewMessage}[k]
	w := mygo.NewWindow(mygo.WindowOptions{
		Title:         mg.Model.Title(k),
		Width:         size[0],
		Height:        size[1],
		DisableResize: true,
		AlwaysOnTop:   true,
		SkipTaskbar:   k == Check,
		Frameless:     k == Check,
		Transparent:   k == Check,
		Hidden:        true,
		Content:       ui.View(view),
	})
	w.OnClose(func(e *mygo.CloseEvent) {
		if mg.quitting() {
			return
		}
		e.PreventDefault()
		w.Hide()
		if k == Prompt || k == Message {
			mg.Model.WindowClosed(k)
			mg.Sync()
		}
	})
	return w
}

func (mg *Manager) quitting() bool { return mg.Model.Quitting != nil && mg.Model.Quitting() }

// Visible says that one of the windows is on screen.
func (mg *Manager) Visible() bool {
	for _, w := range mg.wins {
		if w.IsVisible() {
			return true
		}
	}
	return false
}
