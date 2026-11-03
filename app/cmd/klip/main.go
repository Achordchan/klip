// Command klip is the clipboard manager: a tray icon, a panel that a global
// shortcut brings up, and the capture and sync running behind them.
package main

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"klip/internal/aboutwin"
	"klip/internal/app"
	"klip/internal/model"
	"klip/internal/panel"
	"klip/internal/paths"
	"klip/internal/platform"
	"klip/internal/prefswin"
	"klip/internal/previewwin"
	"klip/internal/shelf"
	"klip/internal/shelfwin"
	"klip/internal/update"
	"klip/internal/updatewin"
	"klip/internal/version"
)

//go:embed icon.png
var trayIcon []byte

// trayTemplate is a black ring on a transparent ground: the menu bar of macOS
// draws a template image in its own colour, and a coloured square would show
// as a solid white block.
//
//go:embed tray.png
var trayTemplate []byte

// shortcuts are tried in turn: the one of the C# client first.
var shortcuts = []string{"Ctrl+Alt+2", "CmdOrCtrl+Alt+Shift+2"}

type program struct {
	app   *app.App
	model *panel.Model
	win   *mygo.Window
	tray  *mygo.Tray

	prefs    *prefswin.Model
	prefsWin *mygo.Window
	upd      *updatewin.Model
	updWins  *updatewin.Manager

	about   *aboutwin.Manager
	preview *previewwin.Manager

	shelves *shelfwin.Manager
	shelf   *shelf.Service
	layout  paths.Layout

	lastTrayClick time.Time

	redrawPending  atomic.Bool // a redraw is queued for the UI thread
	followShelf    func()      // starts or stops the shelf as the settings say
	mandatoryShown atomic.Bool // the window for a mandatory update was put up
	quitting       atomic.Bool // set when the program is quitting, see OnBeforeQuit
}

func main() {
	// The name keys the single-instance lock; by default it is the file name
	// of the executable, so Klip.app and a binary of another name would not
	// see each other.
	mygo.App.SetName("Klip")
	// Started as the helper of an update: replace the files and start the
	// program again. This comes before everything else, as a second
	// instance must not be told to show itself.
	if handled, code := update.HelperMain(os.Args, update.Hooks{}); handled {
		os.Exit(code)
	}
	if !mygo.App.RequestSingleInstanceLock() {
		// Windows: the .NET client refuses a second start with a box, and the
		// running one does nothing. Elsewhere the running Klip shows itself
		// (OnSecondInstance).
		platform.ShowAlreadyRunning()
		return
	}
	if update.UpdateInProgress() {
		return
	}
	if exe, err := os.Executable(); err == nil {
		if needed, err := update.StartRecovery(filepath.Dir(exe)); needed || err != nil {
			return
		}
	}
	if platform.HoldAppMutex() {
		platform.ShowAlreadyRunning() // another instance, perhaps 1.0.10, holds the named mutex
		return
	}
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
	mygo.App.OnWindowAllClosed(func() {})
	p := &program{}
	mygo.App.OnSecondInstance(func([]string, string) {
		if p.win != nil && runtime.GOOS != "windows" {
			p.show()
		}
	})
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { p.quitting.Store(true) })
	mygo.App.WhenReady(p.ready)
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

func (p *program) ready() {
	layout, err := paths.Default()
	if err != nil {
		log.Fatal(err)
	}
	p.app, err = app.New(app.Config{
		Layout:     layout,
		Clipboard:  platform.Clipboard{},
		Foreground: platform.ForegroundApp,
		ChangeSeq:  platform.ChangeSeq,
		Autostart:  platform.Autostart(),
		Quit:       func() { mygo.App.Quit() },
		Changed: func() {
			p.redraw()
			p.gateMandatory()
		},
		Toast: func(msg string) {
			// Called from the goroutine of a sync; the model belongs to the UI
			// thread. Not waited for: the sync must not hang on a thread that
			// may be waiting for it.
			go mygo.RunOnMain(func() {
				if p.model != nil {
					p.model.ShowToast(msg)
				}
			})
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	p.layout = layout
	applyTheme(p.app.Settings.Get().Theme)
	p.app.Settings.OnChange(func(st model.Settings) {
		applyTheme(st.Theme)
		p.redraw() // a window shows the settings (the login among them)
	})
	p.model = panel.New(p.app.History, host{p})
	p.model.Redraw = p.redraw
	p.about = &aboutwin.Manager{Version: host{p}.Version(), Quitting: p.quitting.Load}
	p.preview = &previewwin.Manager{IconOf: shelfwin.SystemIcon, Left: p.model.PreviewLeft, Quitting: p.quitting.Load}

	// The window of the .NET client: 360x620, a bare rounded box.
	p.win = mygo.NewWindow(mygo.WindowOptions{
		Title:         "Klip",
		Panel:         true,
		Hidden:        true,
		Frameless:     true,
		Transparent:   true,
		Width:         360,
		Height:        620,
		DisableResize: true,
		Content:       ui.View(p.model.View),
	})
	p.win.OnClose(func(e *mygo.CloseEvent) {
		if p.quitting.Load() {
			return // Quit closes every window; one that refuses stops it
		}
		e.PreventDefault()
		p.hide()
	})
	p.win.OnBlur(p.hide)

	registered := ""
	for _, s := range shortcuts {
		if err := mygo.GlobalShortcut.Register(s, p.shortcut); err == nil {
			registered = s
			break
		} else {
			log.Printf("shortcut %s: %v", s, err)
		}
	}

	_ = registered
	icon, template := trayIcon, false
	if runtime.GOOS == "darwin" {
		icon, template = trayTemplate, true
	}
	p.tray, err = mygo.NewTray(mygo.TrayOptions{
		Icon:           icon,
		IconIsTemplate: template,
		ToolTip:        "Klip剪贴板工具",
		Menu: mygo.NewMenu([]*mygo.MenuItem{
			{Label: "打开", Click: func(*mygo.MenuItem, *mygo.Window) { p.showCentered() }},
			{Label: "设置...", Click: func(*mygo.MenuItem, *mygo.Window) { p.showPrefs() }},
			{Label: "检测更新...", Click: func(*mygo.MenuItem, *mygo.Window) { p.checkUpdates() }},
			mygo.Separator(),
			{Label: "退出", Click: func(*mygo.MenuItem, *mygo.Window) { mygo.App.Quit() }},
		}),
	})
	if err != nil {
		log.Println("tray:", err)
	} else {
		// A double click on the icon opens the window, as in the .NET client
		// (two clicks within 380 ms).
		p.tray.OnClick(func() {
			now := time.Now()
			if now.Sub(p.lastTrayClick) <= 380*time.Millisecond {
				p.lastTrayClick = time.Time{}
				p.showCentered()
				return
			}
			p.lastTrayClick = now
		})
	}

	p.startShelf()
	mygo.App.OnQuit(func() { p.preview.Close(); p.about.Destroy() })
	mygo.App.OnQuit(p.stopShelf)
	mygo.App.OnQuit(p.app.Stop)
	p.app.Start()
}

// startShelf makes the shelf follow the settings. It is a Windows feature: it
// listens to the mouse everywhere.
func (p *program) startShelf() {
	if runtime.GOOS != "windows" {
		return
	}
	p.shelves = shelfwin.NewManager()
	p.shelf = &shelf.Service{
		Settings: p.shelfSettings,
		Spawn:    p.shelves.Spawn,
	}
	follow := func() {
		if p.app.Updates.MustUpdate() != nil {
			p.shelf.Stop() // a mandatory update is waiting: no shelf until it is dealt with
			return
		}
		if err := p.shelf.Follow(p.shelfSettings()); err != nil {
			log.Println("shelf:", err)
		}
	}
	p.followShelf = follow
	follow()
	p.app.Settings.OnChange(func(model.Settings) { follow() })
}

func (p *program) shelfSettings() shelf.Settings {
	st := p.app.Settings.Get()
	return shelf.FromSettings(st.ShelfEnabled, st.ShelfTriggerModifier)
}

func (p *program) stopShelf() {
	if p.shelf != nil {
		p.shelf.Stop()
	}
	if p.shelves != nil {
		p.shelves.CloseAll()
	}
}

// redraw asks the visible windows for a frame; any goroutine may call it.
func (p *program) redraw() {
	// One request waits at a time; the window fields are read on the UI thread,
	// and the caller (a settings handler, a sync, the clipboard) is not held up
	// by it, nor can it hold the UI thread up.
	if !p.redrawPending.CompareAndSwap(false, true) {
		return
	}
	go mygo.RunOnMain(func() {
		p.redrawPending.Store(false)
		if p.win != nil && p.win.IsVisible() {
			p.win.Update(func() {})
		}
		if p.prefsWin != nil && p.prefsWin.IsVisible() {
			p.prefsWin.Update(func() {})
		}
		if p.updWins != nil {
			p.updWins.Sync()
		}
	})
}

// mandatoryPending says whether a mandatory update is known: until it is
// installed (or the program quit) the rest of the program stays shut, and the
// update window is what shows.
func (p *program) mandatoryPending() bool {
	if p.app == nil || p.app.Updates == nil {
		return false
	}
	if p.app.Updates.MustUpdate() == nil {
		return false
	}
	p.updates()
	p.updWins.Reshow() // also when its window was closed: it is the only way on
	return true
}

// gateMandatory is called when the state of the program changed: a mandatory
// update found by the quiet check puts its window up at once. Any goroutine.
func (p *program) gateMandatory() {
	if p.app == nil || p.app.Updates == nil {
		return
	}
	if p.app.Updates.MustUpdate() == nil {
		// The mandatory update is gone (withdrawn, or installed): the gate opens
		// again and the shelf comes back as the settings have it.
		if p.mandatoryShown.Swap(false) && p.followShelf != nil {
			p.followShelf()
		}
		return
	}
	if p.mandatoryShown.CompareAndSwap(false, true) {
		mygo.RunOnMain(func() {
			// Whatever is open is shut: preferences, the shelves, the shelf
			// trigger; the update window is all that is left.
			p.hide()
			if p.prefsWin != nil {
				p.prefsWin.Hide()
			}
			p.stopShelf()
			p.updates()
			p.updWins.Sync()
		})
	}
}

// hide hides the main window, and the windows that belong to it.
func (p *program) hide() {
	p.win.Hide()
	p.model.Closed() // and no preview comes back for the row the pointer is on
	p.preview.Hide()
	p.about.Close()
}

// applyTheme sets the look of the windows from the setting: "System",
// "Dark" or "Light".
func applyTheme(v string) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "dark":
		mygo.Theme.SetSource(mygo.ThemeDark)
	case "light":
		mygo.Theme.SetSource(mygo.ThemeLight)
	default:
		mygo.Theme.SetSource(mygo.ThemeSystem)
	}
}

// updates makes the windows of the update on first use.
func (p *program) updates() *updatewin.Model {
	if p.upd == nil {
		p.upd = updatewin.New(p.app.Updates, updHost{p})
		p.upd.Quitting = p.quitting.Load
		p.updWins = &updatewin.Manager{Model: p.upd}
	}
	return p.upd
}

// showUpdate is the update button of the main window: the new version is
// downloaded at once (with a window for the progress).
func (p *program) showUpdate() {
	p.hide()
	p.updates().ApplyNow()
	p.updWins.Reshow() // asked for: its window comes back if it was closed
}

// checkUpdates is the user asking (the tray, the preferences): the windows
// show the check and what it finds.
func (p *program) checkUpdates() {
	p.updates().CheckNow()
	p.updWins.Reshow() // asked for: a window closed while a download goes on comes back
}

// showPrefs opens the preferences window, made on first use and then only
// hidden when closed.
func (p *program) showPrefs() {
	if p.mandatoryPending() {
		return
	}
	if p.prefsWin == nil {
		p.prefs = prefswin.New(p.app.Prefs, p.app.Account, prefsHost{p})
		p.prefs.Post = func(f func()) { p.prefsWin.Update(f) }
		p.prefs.Redraw = p.redraw
		p.prefs.SyncStatus = p.app.SyncStatus
		p.prefsWin = mygo.NewWindow(mygo.WindowOptions{
			Title:         "设置",
			Width:         prefswin.Width,
			Height:        prefswin.Height,
			DisableResize: true,
			Content:       ui.View(p.prefs.View),
		})
		p.prefsWin.OnClose(func(e *mygo.CloseEvent) {
			if p.quitting.Load() {
				return
			}
			e.PreventDefault()
			p.prefsWin.Hide()
		})
	}
	p.hide()
	p.prefs.Open()
	p.prefsWin.Show()
	p.prefsWin.Focus()
}

// show opens the window near the cursor (the shortcut), a little below and
// to the right of it, kept inside the work area of its display.
func (p *program) show() {
	if p.mandatoryPending() {
		return // the update window is up: nothing else until it is dealt with
	}
	cur := mygo.Screen.CursorScreenPoint()
	wa := mygo.Screen.DisplayNearestPoint(cur).WorkArea
	w, h := p.win.Size()
	x := max(wa.X, min(cur.X+12, wa.X+wa.Width-w))
	y := max(wa.Y, min(cur.Y+12, wa.Y+wa.Height-h))
	p.open(x, y)
}

// showCentered opens the window in the middle of the display the cursor is on
// (the tray).
func (p *program) showCentered() {
	if p.mandatoryPending() {
		return // the update window is up: nothing else until it is dealt with
	}
	wa := mygo.Screen.DisplayNearestPoint(mygo.Screen.CursorScreenPoint()).WorkArea
	w, h := p.win.Size()
	p.open(wa.X+(wa.Width-w)/2, wa.Y+(wa.Height-h)/2)
}

func (p *program) open(x, y int) {
	p.win.SetPosition(x, y)
	p.model.Opened()
	p.win.Show()
	p.win.Focus()
}

// shortcut is the global shortcut: it opens the window near the cursor, also
// when it is open already (ToggleWindowNearCursor does not close it).
func (p *program) shortcut() { p.show() }

// host is what the panel asks of the program.
type host struct{ p *program }

func (h host) Apply(it model.Item) error         { return h.p.app.Apply(it) }
func (h host) Hide()                             { h.p.hide() }
func (h host) Delete(id string)                  { h.p.app.Delete(id) }
func (h host) ClearAll()                         { h.p.app.ClearAll() }
func (h host) Reorder(id string, before *string) { h.p.app.Reorder(id, before) }
func (h host) Sync() app.SyncView                { return h.p.app.Sync() }
func (h host) Version() string                   { return "v" + version.Version }
func (h host) ShowPrefs()                        { h.p.showPrefs() }
func (h host) ShowUpdate()                       { h.p.showUpdate() }
func (h host) Quit()                             { mygo.App.Quit() }

func (h host) UpdateAvailable() string {
	st := h.p.app.Updates.State()
	if st.Info != nil && (st.Phase == update.Available || st.Phase == update.Downloading || st.Phase == update.Applying) {
		return st.Info.Version
	}
	return ""
}

func (h host) UpdateApplying() bool {
	st := h.p.app.Updates.State()
	return st.Phase == update.Downloading || st.Phase == update.Applying
}

// screen turns a rectangle of the main window into one of the screen.
func (h host) screen(r ui.Rect) (x, y, w, hh int) {
	wx, wy := h.p.win.Position()
	return wx + int(r.X), wy + int(r.Y), int(r.W), int(r.H)
}

func (h host) ShowAbout(anchor ui.Rect) {
	x, y, w, hh := h.screen(anchor)
	h.p.about.Toggle(x+w/2, y+hh)
}

func (h host) ShowPreview(it model.Item, anchor ui.Rect) {
	x, y, w, hh := h.screen(anchor)
	h.p.preview.Show(it, previewwin.Rect{X: x, Y: y, W: w, H: hh})
}

func (h host) HidePreview()            { h.p.preview.Hide() }
func (h host) Preview() (string, bool) { return h.p.preview.Preview() }

// prefsHost is what the preferences window asks of the program.
type prefsHost struct{ p *program }

func (h prefsHost) OpenStorage()           { _ = mygo.Shell.OpenPath(h.p.layout.Root) }
func (h prefsHost) CheckUpdates() func()   { return h.p.checkUpdates }
func (h prefsHost) SyncNow()               { h.p.app.SyncNow() }
func (h prefsHost) AccountChanged(in bool) { h.p.app.AccountChanged(in) }
func (h prefsHost) Close()                 { h.p.prefsWin.Hide() }

// updHost is what the update windows ask of the program.
type updHost struct{ p *program }

func (h updHost) Quit() { mygo.App.Quit() }
