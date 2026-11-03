package previewwin

import (
	"strings"
	"sync"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"klip/internal/model"
	"klip/internal/panel"
)

// Manager owns the preview window: made when first needed, then shown, moved
// and hidden. Show and Hide may be called from any goroutine.
type Manager struct {
	// IconOf gives the icon of a program (may be nil).
	IconOf func(path string) *ui.Bitmap
	// Left is called when the pointer leaves the window.
	Left func()
	// Quitting says the program is quitting: the window then lets itself be
	// closed instead of hiding, or Quit would wait for it for ever.
	Quitting func() bool

	// decode reads a picture cut down to a size (panel.ScaledBitmap when
	// nil), and onMain runs on the UI thread (mygo.RunOnMain when nil).
	decode func(path string, max int) (*ui.Bitmap, int, int)
	onMain func(func())

	mu    sync.Mutex
	model *Model
	win   *mygo.Window
	id    string
	seq   int  // moves with each Show and each Hide
	shut  bool // Close was called: nothing is shown any more
	// failed is the picture that could not be shown, which is not read again
	// while the pointer stays on its row (the panel asks for it each frame);
	// Hide, or another item, forgets it.
	failed   string
	iconBusy bool
	icons    map[string]*ui.Bitmap
}

func (mg *Manager) runOnMain(f func()) {
	if mg.onMain != nil {
		mg.onMain(f)
		return
	}
	mygo.RunOnMain(f)
}

// Preview says which item the window shows ("" when hidden) and whether the
// pointer is over it.
func (mg *Manager) Preview() (string, bool) {
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if mg.model == nil {
		return mg.id, false // a picture is on its way, the window is not there yet
	}
	return mg.id, mg.model.Over()
}

// maxPicture is the longest side a picture is kept at for the window: the
// largest window shows it at 980 points, twice that for sharp screens.
const maxPicture = 1960

// Show opens the window for an item beside anchor, which is in screen pixels.
// A picture is read and cut down, and the files of an item are looked at, in
// the background; the window opens when that is done, unless the pointer has
// moved on by then. Any goroutine.
func (mg *Manager) Show(it model.Item, anchor Rect) {
	mg.mu.Lock()
	shut, failed := mg.shut, mg.failed != "" && mg.failed == it.ID
	if !failed {
		mg.failed = ""
	}
	mg.mu.Unlock()
	if shut || failed {
		return
	}
	if it.Kind != model.KindImage && it.Kind != model.KindFileList {
		mg.mu.Lock()
		mg.seq++ // a picture that is still being made is not wanted
		seq := mg.seq
		mg.mu.Unlock()
		mg.present(it, nil, 0, 0, (&Model{Item: it}).Meta(), anchor, seq)
		return
	}
	mg.mu.Lock()
	mg.id = it.ID // the panel asks for each item once
	mg.seq++
	seq := mg.seq
	mg.mu.Unlock()
	go func() {
		previewWork <- struct{}{}
		defer func() { <-previewWork }()
		mg.mu.Lock()
		current := mg.wanted(seq)
		mg.mu.Unlock()
		if !current {
			return
		}
		var bm *ui.Bitmap
		var pw, ph int
		if it.Kind == model.KindImage {
			decode := mg.decode
			if decode == nil {
				decode = panel.ScaledBitmap
			}
			bm, pw, ph = decode(model.Str(it.ImageFilePath), maxPicture)
		}
		mg.mu.Lock()
		current = mg.wanted(seq)
		mg.mu.Unlock()
		if !current {
			return // another item, or none, since
		}
		if it.Kind == model.KindImage && bm == nil {
			// The file is gone, or too big to show: nothing to show, and
			// nothing to read again for it.
			mg.mu.Lock()
			if mg.wanted(seq) {
				mg.failed = it.ID
			}
			mg.mu.Unlock()
			mg.hide(seq)
			return
		}
		meta := (&Model{Item: it, Bitmap: bm, Width: pw, Height: ph}).Meta()
		mg.present(it, bm, pw, ph, meta, anchor, seq)
	}()
}

// present puts the window up for an item whose content is ready, if seq is
// still the latest request when the UI thread gets to it (a Hide or another
// Show in between voids it).
func (mg *Manager) present(it model.Item, bm *ui.Bitmap, pw, ph int, meta string, anchor Rect, seq int) {
	mg.runOnMain(func() {
		mg.mu.Lock()
		defer mg.mu.Unlock()
		if !mg.wanted(seq) {
			return
		}
		if mg.model == nil {
			mg.model = &Model{Left: mg.Left, Open: func(p string) { _ = mygo.Shell.OpenPath(p) }}
		}
		mg.model.Item, mg.model.Bitmap, mg.model.Width, mg.model.Height = it, bm, pw, ph
		mg.model.MetaLine = meta
		path := strings.TrimSpace(model.Str(it.SourceAppPath))
		mg.model.icon = mg.icons[path]
		mg.loadIcon(path)
		mg.id = it.ID
		w, h := Size(it, pw, ph)
		d := mygo.Screen.DisplayNearestPoint(mygo.Point{X: anchor.X + anchor.W/2, Y: anchor.Y + anchor.H/2})
		wa := d.WorkArea
		x, y, w, h := Place(anchor, w, h, Rect{wa.X, wa.Y, wa.Width, wa.Height})
		if mg.win == nil {
			mg.win = mygo.NewWindow(mygo.WindowOptions{
				Title:         "Klip 预览",
				Panel:         true, // never takes the focus from the main window
				Frameless:     true,
				Transparent:   true,
				AlwaysOnTop:   true,
				SkipTaskbar:   true,
				DisableResize: true,
				Hidden:        true,
				Width:         w,
				Height:        h,
				Content:       ui.View(mg.model.View),
			})
			mg.win.OnClose(func(e *mygo.CloseEvent) {
				if mg.Quitting != nil && mg.Quitting() {
					return
				}
				e.PreventDefault()
				mg.win.Hide()
			})
		}
		mg.win.SetSize(w, h)
		mg.win.SetPosition(x, y)
		mg.win.ShowInactive()
		mg.win.Update(func() {})
	})
}

// wanted says that the request seq is still the latest, and that the manager
// is not closed (under mu).
func (mg *Manager) wanted(seq int) bool { return !mg.shut && mg.seq == seq }

// Hide closes the window.
func (mg *Manager) Hide() { mg.hide(-1) }

// hide closes the window if seq is still the latest request when the UI
// thread gets to it (-1: whatever it is). A picture that could not be read
// must not close the window another item has opened since.
func (mg *Manager) hide(seq int) {
	mg.runOnMain(func() {
		mg.mu.Lock()
		defer mg.mu.Unlock()
		if seq >= 0 && mg.seq != seq {
			return
		}
		if seq < 0 {
			mg.failed = "" // the pointer left the row: it may be tried again
		}
		mg.id = ""
		mg.seq++ // a picture that is still being made is not wanted
		if mg.win != nil && mg.win.IsVisible() {
			mg.win.Hide()
		}
		if mg.model != nil {
			mg.model.over, mg.model.wasOver = false, false
		}
	})
}

// Close destroys the window (when the program quits). A picture or the files
// still being read for a Show are not shown then, nor is anything after.
func (mg *Manager) Close() {
	mg.mu.Lock()
	win := mg.win
	mg.win, mg.shut, mg.id = nil, true, ""
	mg.seq++
	mg.mu.Unlock()
	if win != nil {
		win.Close()
	}
}

// previewWork bounds concurrent image decoding and filesystem metadata reads.
var previewWork = make(chan struct{}, 2)
