package panel

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/egoist/mygo/ui"

	"klip/internal/model"
)

// View is the window's content.
func (m *Model) View(c *ui.Context) {
	m.refresh()
	p := PaletteFor(c.Theme().Dark)
	c.Root().Background(ui.Transparent)

	var act *action
	if m.dlg == nil {
		act = m.keys(c)
	}
	if runtime.GOOS == "darwin" {
		c.OnShortcut(ui.Cmd, ui.KeyQ, m.Host.Quit)
		c.OnShortcut(ui.Cmd, ui.KeyComma, m.Host.ShowPrefs)
	}

	var hov *model.Item
	var hovRect ui.Rect
	ui.Box(c).Fill().Children(func() {
		ui.Column(c).Fill().Background(p.Background).Border(1, p.Divider).Radius(14).Clip().Children(func() {
			m.topBar(c, p)
			m.body(c, p, &act, &hov, &hovRect)
			m.bottomBar(c, p, &act)
		})
		m.toastView(c, p)
		m.dialogView(c, p)
	})
	m.hovering(hov, hovRect)

	if act != nil {
		m.run(*act)
	}
}

// keys is the keyboard of the window while no box is open (MainWindow's
// KeyBindings): Enter and the arrows belong to the search box while it has
// the focus (see searchKey), and act on the list when it has it.
func (m *Model) keys(c *ui.Context) *action {
	var act *action
	cur, ok := m.current()
	if c.Shortcut(0, ui.KeyEscape) {
		m.Host.Hide()
	}
	if c.Shortcut(0, ui.KeyDown) {
		m.choose(m.chosen + 1)
	}
	if c.Shortcut(0, ui.KeyUp) {
		m.choose(m.chosen - 1)
	}
	if ok {
		switch {
		case c.Shortcut(0, ui.KeyEnter):
			act = &action{kind: "apply", item: cur}
		case c.Shortcut(0, ui.KeyDelete) && m.query == "":
			act = &action{kind: "delete", item: cur}
		case c.Shortcut(ui.Ctrl, ui.KeyP):
			act = &action{kind: "pin", item: cur}
		}
	}
	if c.Shortcut(ui.Ctrl, ui.KeyF) {
		m.input.Focus()
	}
	return act
}

// searchKey takes the keys that move through the list from the search box:
// the box would otherwise keep Enter and the arrows for itself. It runs on
// the UI thread, between frames.
func (m *Model) searchKey(k ui.TextKey) bool {
	m.refresh()
	cur, ok := m.current()
	switch {
	case k.Mods == ui.Ctrl && k.Key == ui.KeyBackspace:
		m.query = ""
	case k.Mods == ui.Ctrl && k.Key == ui.KeyP && m.dlg == nil:
		// The box would take it for "previous line".
		if ok {
			m.run(action{kind: "pin", item: cur})
		}
	case k.Mods == ui.Ctrl && k.Key == ui.KeyF:
		// Already here.
	case k.Mods == 0 && k.Key == ui.KeyDelete && m.query == "" && m.dlg == nil:
		if ok {
			m.run(action{kind: "delete", item: cur})
		}
	case k.Mods != 0:
		return false
	case k.Key == ui.KeyDown:
		m.choose(m.chosen + 1)
	case k.Key == ui.KeyUp:
		m.choose(m.chosen - 1)
	case k.Key == ui.KeyEnter:
		if ok && m.dlg == nil {
			m.run(action{kind: "apply", item: cur})
		}
	default:
		return false
	}
	m.redraw()
	return true
}

// ---- pieces

func hline(c *ui.Context, p Palette) {
	ui.Box(c).FillWidth().Height(1).Background(p.Divider)
}

// button is a Button of the window: the face of a control, a border, the
// hover and the pressed colour.
func button(c *ui.Context, p Palette, label string) ui.Element {
	return Button(c, p, label, 4, 10, 12)
}

// Button is the Button of the windows of Klip, with the padding and text
// size the window asks for.
func Button(c *ui.Context, p Palette, label string, padV, padH, font float32) ui.Element {
	b := ui.ButtonBase(c)
	bg := p.Control
	switch {
	case b.Pressed():
		bg = p.Pressed
	case b.Hovered():
		bg = p.Hover
	}
	return b.Padding(padV, padH).Radius(4).Border(1, p.Border).Background(bg).Center().Children(func() {
		ui.Text(c, label).FontSize(font).TextColor(p.Text).SingleLine()
	})
}

func (m *Model) topBar(c *ui.Context, p Palette) {
	ui.Row(c).FillWidth().Padding(8, 10).Gap(10).AlignItems(ui.Center).Background(p.Panel).DragWindow().Children(func() {
		about := ui.ButtonBase(c)
		bg := p.Control
		if about.Pressed() {
			bg = p.Pressed
		} else if about.Hovered() {
			bg = p.Hover
		}
		about.Padding(4, 10).Radius(8).Border(1, p.Border).Background(bg).Cursor(ui.CursorPointer).Label("关于").Justify(ui.Center).Children(func() {
			ui.Text(c, "About").FontSize(12).TextColor(p.Muted)
		})
		if about.Clicked() {
			m.Host.ShowAbout(about.Bounds())
		}
		ui.Row(c).Grow(1).Height(30).Padding(0, 8).AlignItems(ui.Center).
			Background(p.Control).Border(1, p.Border).Radius(4).Children(func() {
			ui.TextInputBase(c, &m.query).Bind(&m.input).Grow(1).FontSize(14).TextColor(p.Text).
				Placeholder("搜索").Label("搜索").OnTextKey(m.searchKey)
		})
	})
	hline(c, p)
}

func (m *Model) body(c *ui.Context, p Palette, act **action, hov **model.Item, hovRect *ui.Rect) {
	ui.Column(c).Grow(1).FillWidth().Children(func() {
		if m.sec == sectionAll {
			entry := ui.ButtonBase(c)
			bg := ui.Transparent
			switch {
			case entry.Pressed():
				bg = p.Pressed
			case entry.Hovered():
				bg = p.Hover
			}
			entry.FillWidth().Padding(7, 10).Gap(10).AlignItems(ui.Center).Background(bg).Radius(0).
				Cursor(ui.CursorPointer).Label("常用剪贴").Tooltip("打开常用剪贴").Children(func() {
				ui.Text(c, "常用剪贴").FontSize(13).TextColor(p.Text).Grow(1)
				ui.Text(c, "›").FontSize(16).TextColor(p.Muted2)
			})
			if entry.Clicked() {
				m.enter(sectionFavorites)
			}
			hline(c, p)
		} else {
			ui.Row(c).FillWidth().Padding(6, 10).Gap(8).AlignItems(ui.Center).Background(p.Panel).Children(func() {
				if button(c, p, "返回").Label("返回").Clicked() {
					m.enter(sectionAll)
				}
				ui.Text(c, "常用剪贴").TextColor(p.Text).FontSize(13)
			})
			hline(c, p)
		}

		rows := m.rows()
		// The choice is the model's own (m.chosen), drawn by the rows: the
		// list would move it with the item it remembers when the order
		// changes.
		m.list.Key = func(i int) any { return rows[i].ID }
		m.list.Label = func(i int) string { return RowText(rows[i]) }
		ui.List(c, &m.list, len(rows), func(i int) {
			if a := m.row(c, p, rows, i, hov, hovRect); a != nil {
				*act = a
			}
		}).Grow(1)
	})
}

// row builds one row and returns what the user did to it.
func (m *Model) row(c *ui.Context, p Palette, rows []model.Item, i int, hov **model.Item, hovRect *ui.Rect) *action {
	it := rows[i]
	text := p.Text
	if i%2 == 0 {
		text = p.Muted // the first row, the third, ...
	}
	var act *action
	ui.Column(c).FillWidth().Children(func() {
		btn := ui.ButtonBase(c)
		bg := ui.Transparent
		switch {
		case btn.Dragging():
		case i == m.chosen || btn.Pressed():
			bg = p.Pressed
		case btn.Hovered():
			bg = p.Hover
		}
		if _, over := ui.DragOver[dragRow](btn); over {
			bg = p.Pressed
		}
		if m.flashID == it.ID && m.Now().Before(m.flashUntil) {
			bg = p.Hover
		}
		btn.FillWidth().Padding(7, 10).Gap(10).AlignItems(ui.Center).Radius(0).Background(bg).
			Label(RowText(it)).Drag(dragRow{ID: it.ID})
		if btn.Dragging() {
			btn.Opacity(0.55)
		}
		btn.Children(func() {
			if it.Pinned {
				ui.Text(c, "★").FontSize(12).TextColor(p.Star).Width(16)
			}
			if it.Kind == model.KindImage {
				m.imageCell(c, p, it, text)
			} else {
				ui.Text(c, RowText(it)).FontSize(13).TextColor(text).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
			}
			if tag := Tag(it); tag != "" {
				ui.Row(c).Size(44, 18).Radius(4).Background(p.Control).Center().Children(func() {
					ui.Text(c, tag).FontSize(9).TextColor(p.Muted).SingleLine().Ellipsis("…")
				})
			}
		})
		btn.ContextMenu(func(menu *ui.Menu) {
			if menu.Item("加入收藏/取消收藏").Chosen() {
				act = &action{kind: "pin", item: it}
			}
			if menu.Item("添加备注").Chosen() {
				act = &action{kind: "note", item: it}
			}
			menu.Separator()
			if menu.Item("删除").Chosen() {
				act = &action{kind: "delete", item: it}
			}
		})
		if btn.RightClicked() {
			m.chosen = i
		}
		if d, ok := ui.Drop[dragRow](btn); ok && d.ID != it.ID {
			act = &action{kind: "move", item: it, from: d.ID}
		} else if btn.Clicked() {
			m.chosen = i
			act = &action{kind: "apply", item: it}
		}
		if btn.Hovered() && !btn.Dragging() {
			cp := it
			*hov, *hovRect = &cp, btn.Bounds()
		}
		hline(c, p)
		if i == len(rows)-1 {
			// Below the last row: a dragged row dropped here goes to the end
			// (on a row, it goes before that row).
			end := ui.Row(c).FillWidth().Height(16)
			if d, over := ui.DragOver[dragRow](end); over && d.ID != it.ID {
				end.Background(p.Pressed)
			}
			if d, ok := ui.Drop[dragRow](end); ok && d.ID != it.ID {
				act = &action{kind: "move", from: d.ID, toEnd: true}
			}
		}
	})
	return act
}

// imageCell is the middle of the row of an image: a small picture and what
// is known of the file.
func (m *Model) imageCell(c *ui.Context, p Palette, it model.Item, text ui.Color) {
	ui.Row(c).Grow(1).MinWidth(0).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Row(c).Size(36, 36).Radius(6).Background(p.Control).Clip().Children(func() {
			if bm := m.thumbs.get(model.Str(it.ImageFilePath), m.Redraw); bm != nil {
				ui.Image(c, bm).Size(36, 36).Fit(ui.Cover)
			}
		})
		info := m.infos.get(model.Str(it.ImageFilePath), m.Redraw)
		if info == "" {
			info = "IMG"
		}
		ui.Text(c, info).FontSize(13).TextColor(text).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
	})
}

func (m *Model) bottomBar(c *ui.Context, p Palette, act **action) {
	hline(c, p)
	sv := m.Host.Sync()
	ui.Row(c).FillWidth().Padding(8, 10).Gap(8).AlignItems(ui.Center).Background(p.Panel).DragWindow().Children(func() {
		ui.Text(c, m.Host.Version()).FontSize(11).TextColor(p.Muted2).SingleLine()

		if v := m.Host.UpdateAvailable(); v != "" {
			label, tip := "更新", "发现新版本 "+v+"，点击立即更新"
			applying := m.Host.UpdateApplying()
			if applying {
				label, tip = "更新中...", "正在下载并应用更新，请稍候"
			}
			b := ui.ButtonBase(c)
			bg := p.UpdateBg
			if b.Hovered() && !applying {
				bg = p.UpdateBg.Mix(ui.RGB(0, 0, 0), 0.08)
			}
			b.Padding(3, 8).Radius(4).Border(1, p.UpdateEdge).Background(bg).Disabled(applying).Label(label).Tooltip(tip).Children(func() {
				ui.Text(c, label).FontSize(11).FontWeight(600).TextColor(ui.RGB(255, 255, 255)).SingleLine()
			})
			if b.Clicked() {
				m.Host.ShowUpdate()
			}
		}

		ui.Row(c).Grow(1).MinWidth(0).AlignItems(ui.Center).Children(func() {
			if sv.Progress {
				ui.Row(c).Grow(1).MinWidth(0).Padding(6, 10).Gap(10).AlignItems(ui.Center).
					Background(p.Control).Border(1, p.Border).Radius(8).Clip().Children(func() {
					ui.Text(c, sv.ProgressText()).FontSize(11).TextColor(p.Muted).SingleLine().Ellipsis("…").MaxWidth(120)
					ui.Progress(c, float64(sv.Percent)/100).Grow(1).Height(12).MinWidth(0)
				})
			}
		})

		ui.Image(c, cloudIcon(sv.Icon(), p)).Size(18, 18).Tooltip(sv.Tip()).Label("云同步")

		if button(c, p, "设置").Label("设置").Clicked() {
			m.Host.ShowPrefs()
		}
		if button(c, p, "清除").Label("清除").Clicked() {
			*act = &action{kind: "clear"}
		}
	})
}

func (m *Model) toastView(c *ui.Context, p Palette) {
	if m.toast == "" || !m.Now().Before(m.toastUntil) {
		return
	}
	ui.Row(c).Absolute().Left(0).Right(0).Bottom(12).Justify(ui.Center).PassThrough().Children(func() {
		ui.Row(c).Padding(8, 12).Radius(8).Background(p.ToastBg).Children(func() {
			ui.Text(c, m.toast).FontSize(13).TextColor(p.ToastText)
		})
	})
}

// dialogView is the note box or the question, over the window.
func (m *Model) dialogView(c *ui.Context, p Palette) {
	d := m.dlg
	if d == nil {
		return
	}
	// Modal, over the window: the focus goes into the dialog and Tab goes
	// round inside it; the rows and buttons behind it (which apply an item
	// and hide the window, losing a note being written) take no keys.
	ui.Overlay(c, func() {
		back := ui.Column(c).Absolute().Left(0).Right(0).Top(0).Bottom(0).Background(ui.RGBA(0, 0, 0, 0.25)).Center().Modal()
		if back.OverlayShortcut(0, ui.KeyEscape) {
			m.dlg = nil
			m.input.Focus()
			return
		}
		back.Children(func() { m.dialogBox(c, p, d) })
	})
}

// dialogBox is the box of the dialog d.
func (m *Model) dialogBox(c *ui.Context, p Palette, d *dialog) {
	ui.Column(c).Width(328).Padding(16).Gap(0).Background(p.Panel).Border(1, p.Border).Radius(14).Children(func() {
		ui.Text(c, d.title).FontSize(15).FontWeight(600).TextColor(p.Text)
		switch d.kind {
		case "note":
			ui.Text(c, d.hint).FontSize(12).TextColor(p.Muted2).Margin(6, 0, 0)
			ui.Column(c).FillWidth().Margin(10, 0, 0).Padding(10).Background(p.Control).Border(1, p.Border).Radius(12).Children(func() {
				if d.focus {
					d.focus = false
					m.noteHand.Focus()
				}
				ui.TextAreaBase(c, &d.text).Bind(&m.noteHand).FillWidth().MinHeight(80).FontSize(13).TextColor(p.Text).Label("备注")
			})
		default:
			ui.Column(c).FillWidth().Margin(10, 0, 0).Padding(12).Background(p.Control).Border(1, p.Border).Radius(12).Children(func() {
				ui.Text(c, d.hint).FontSize(12).TextColor(p.Muted).Wrap()
			})
		}
		ui.Row(c).FillWidth().Margin(12, 0, 0).Gap(10).Justify(ui.End).Children(func() {
			cancel := dialogButton(c, p, "取消", p.Control, p.Text, true)
			ok := dialogButton(c, p, d.ok, map[bool]ui.Color{true: p.Red, false: p.Blue}[d.danger], ui.RGB(255, 255, 255), false)
			if cancel {
				m.dlg = nil
				m.input.Focus()
			} else if ok {
				m.dlg = nil
				m.finish(d)
				m.input.Focus()
			}
		})
	})
}

func dialogButton(c *ui.Context, p Palette, label string, bg, fg ui.Color, outlined bool) bool {
	b := ui.ButtonBase(c)
	face := bg
	if outlined {
		switch {
		case b.Pressed():
			face = p.Pressed
		case b.Hovered():
			face = p.Hover
		}
	} else if b.Hovered() {
		face = bg.Mix(ui.RGB(0, 0, 0), 0.08)
	}
	b.MinWidth(80).Padding(8, 14).Radius(10).Background(face).Center().Label(label)
	if outlined {
		b.Border(1, p.Border)
	}
	b.Children(func() { ui.Text(c, label).FontSize(13).TextColor(fg).SingleLine() })
	return b.Clicked()
}

// finish carries out what the box was asked for.
func (m *Model) finish(d *dialog) {
	switch d.kind {
	case "note":
		m.History.UpdateNote(d.item.ID, d.text)
	case "confirm":
		if d.onOK != nil {
			d.onOK()
		}
	}
}

// ---- the cloud of the bar

var (
	cloudMu    sync.Mutex
	cloudCache = map[string]*ui.SVG{}
)

const cloudPath = "M 7.5 18.8 C 5.0 18.8 3.0 16.8 3.0 14.4 C 3.0 12.1 4.6 10.2 6.9 9.8 C 7.5 6.9 10.0 4.8 13.0 4.8 " +
	"C 16.4 4.8 19.1 7.2 19.7 10.4 C 21.7 10.8 23.2 12.5 23.2 14.8 C 23.2 17.0 21.4 18.8 19.1 18.8 L 7.5 18.8"

func hex(c ui.Color) string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

// cloudIcon is the cloud of the bar in one of its four states: an outline
// alone when idle, with two arrows (orange) while it works, with a tick
// (green) when the last sync went well, with a cross (red) when it did not.
func cloudIcon(kind string, p Palette) *ui.SVG {
	key := kind + hex(p.Muted2)
	cloudMu.Lock()
	defer cloudMu.Unlock()
	if s, ok := cloudCache[key]; ok {
		return s
	}
	path := func(color ui.Color, w float64, d string) string {
		return fmt.Sprintf(`<path d="%s" fill="none" stroke="%s" stroke-width="%g" stroke-linecap="round"/>`, d, hex(color), w)
	}
	var body string
	switch kind {
	case "working":
		body = path(p.WorkingIcon, 1.8, cloudPath) +
			path(p.WorkingIcon, 1.7, "M 8.0 13.2 C 9.0 11.3 11.1 10.3 13.1 10.6 L 12.1 9.5 M 13.2 10.6 L 12.1 9.5 M 16.0 11.0 C 15.5 13.2 13.5 14.7 11.3 14.6 L 12.4 15.7 M 11.2 14.6 L 12.4 15.7")
	case "success":
		body = path(p.OKIcon, 1.8, cloudPath) + path(p.OKIcon, 2.0, "M 9.0 13.0 L 11.6 15.5 L 16.0 10.8")
	case "error":
		body = path(p.ErrIcon, 1.8, cloudPath) + path(p.ErrIcon, 2.0, "M 10.0 10.8 L 15.0 15.8 M 15.0 10.8 L 10.0 15.8")
	default:
		body = path(p.Muted2, 1.8, cloudPath)
	}
	s := ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24">` + body + `</svg>`))
	cloudCache[key] = s
	return s
}
