package shelfwin

import (
	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
)

var dropOptions = transfer.DropOptions{
	Formats:    []transfer.Format{transfer.FileList, transfer.URIList},
	Operations: transfer.Copy,
}

// The look of ShelfWindow.axaml: a frosted light card whatever the theme is,
// with translucent white pieces on it and black text of different strengths.
// (Avalonia writes #AARRGGBB, MyGo #RRGGBBAA.)
var (
	chromeBg    = ui.Hex("#F2F2F2BF")
	chromeEdge  = ui.Hex("#FFFFFF40")
	cardBg      = ui.Hex("#FFFFFF66")
	cardEdge    = ui.Hex("#FFFFFF26")
	pillBg      = ui.Hex("#FFFFFF33")
	pillHover   = ui.Hex("#FFFFFF4A")
	pillPressed = ui.Hex("#FFFFFF22")
	pillEdge    = ui.Hex("#FFFFFF2A")
	closeBg     = ui.Hex("#FFFFFF2E")
	textStrong  = ui.Hex("#000000AA")
	textSoft    = ui.Hex("#00000066")
	glyphColor  = ui.Hex("#00000088")
	pickBg      = ui.Hex("#4DA3FF22")
	pickEdge    = ui.Hex("#4DA3FF66")
	dropBg      = ui.Hex("#4DA3FF22")
	dropEdge    = ui.Hex("#4DA3FF88")
	dropText    = ui.Hex("#000000CC")
)

// View is the window's content.
func (m *Model) View(c *ui.Context) {
	c.Root().Background(ui.Transparent)

	root := ui.Box(c).Fill().Padding(14).Radius(18).Background(chromeBg).Border(1, chromeEdge).Clip().DragWindow()
	_, over := ui.DataDragOver(root, dropOptions)
	m.dragOver = over && !m.draggingOut
	if drop, ok := ui.DropData(root, dropOptions); ok && !m.draggingOut {
		if files, err := drop.Data.Files(); err == nil {
			m.AddFiles(files)
		}
	}

	root.Children(func() {
		// a soft light from the top
		ui.Box(c).Absolute().Left(-14).Right(-14).Top(-14).Bottom(-14).PassThrough().
			LinearGradient(ui.LinearGradient{From: ui.Hex("#FFFFFF55"), To: ui.Hex("#FFFFFF00"), Angle: 180})

		if m.Expanded {
			m.expanded(c)
		} else {
			m.compact(c)
		}
		if m.dragOver {
			ui.Column(c).Absolute().Left(-14).Right(-14).Top(-14).Bottom(-14).PassThrough().Radius(18).
				Background(dropBg).Border(2, dropEdge).Center().Children(func() {
				ui.Text(c, "松开以放入").FontSize(12).FontWeight(600).TextColor(dropText)
			})
		}
		m.closeButton(c)
	})
}

// closeButton is the × in the corner: a round pill of 28.
func (m *Model) closeButton(c *ui.Context) {
	b := ui.ButtonBase(c)
	b.Absolute().Top(0).Right(0).Size(28, 28).Radius(14).Background(closeBg).Border(1, pillEdge).Center().Label("关闭").Children(func() {
		ui.Text(c, "×").FontSize(18).TextColor(textStrong)
	})
	if b.Clicked() {
		m.Close()
	}
}

// pill is a small round button of the header.
func pill(c *ui.Context, label string, w float32) bool {
	b := ui.ButtonBase(c)
	bg := pillBg
	switch {
	case b.Pressed():
		bg = pillPressed
	case b.Hovered():
		bg = pillHover
	}
	b.Size(w, 28).Radius(14).Background(bg).Border(1, pillEdge).Center().Label(label).Children(func() {
		ui.Text(c, label).FontSize(12).TextColor(textStrong).SingleLine()
	})
	return b.Clicked()
}

func (m *Model) compact(c *ui.Context) {
	handle := ui.Column(c).Fill().Padding(12).Gap(10).Radius(18).Background(cardBg).Border(1, cardEdge).Center()
	if m.Pinned && len(m.Items) > 0 {
		// The whole shelf can be dragged out from here.
		handle.DragDataFrom(func() transfer.Data { return m.dragData(m.AllPaths()) }, m.dragOptions())
	}
	handle.Children(func() {
		switch len(m.Items) {
		case 0:
		case 1:
			m.icon(c, m.Items[0], 56)
			ui.Text(c, m.Items[0].Name).FontSize(12).TextColor(textStrong).SingleLine().Ellipsis("…").MaxWidth(190)
			if pill(c, "展开", 40) { // the details and the choice of grid or list
				m.ToggleExpanded()
			}
		default:
			// A stack: the newest file and how many there are.
			last := m.Items[len(m.Items)-1]
			m.icon(c, last, 56)
			ui.Text(c, m.TotalSize()).FontSize(12).FontWeight(600).TextColor(textStrong)
			if pill(c, m.CountBadge(), 70) {
				m.FoldStack()
			}
		}
	})
}

func (m *Model) expanded(c *ui.Context) {
	ui.Column(c).Fill().Gap(12).Children(func() {
		ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).PaddingEnd(38).Children(func() {
			if len(m.Items) <= 1 {
				if pill(c, "收起", 40) {
					m.ToggleExpanded()
				}
			} else if pill(c, "折叠", 40) {
				m.FoldStack()
			}
			ui.Column(c).Grow(1).Gap(2).AlignItems(ui.Center).Children(func() {
				ui.Text(c, m.TotalSize()).FontSize(12).FontWeight(600).TextColor(textStrong)
				ui.Text(c, m.Title()).FontSize(11).TextColor(textSoft).SingleLine().Ellipsis("…").MaxWidth(240)
			})
			if len(m.Items) <= 1 {
				label := "列表"
				if !m.Grid {
					label = "网格"
				}
				if pill(c, label, 40) {
					m.ToggleView()
				}
			}
		})

		if m.Grid && len(m.Items) <= 1 {
			ui.Scroll(c).Grow(1).Children(func() {
				ui.Row(c).Wrap().Children(func() {
					for i := range m.Items {
						m.item(c, i, 210, 96)
					}
				})
			})
			return
		}
		// Dropping thousands of files must not build a row for each of them on
		// every frame: the list makes the visible ones only.
		m.list.Key = func(i int) any { return m.Items[i].Path }
		m.list.Label = func(i int) string { return m.Items[i].Name }
		ui.List(c, &m.list, len(m.Items), func(i int) { m.item(c, i, 0, 0) }).Grow(1)
	})
}

// item is one file of the expanded shelf: a press chooses it (Ctrl adds), a
// drag takes it, or all the chosen ones, out.
func (m *Model) item(c *ui.Context, i, w, h int) {
	it := m.Items[i]
	grid := w > 0
	btn := ui.ButtonBase(c)
	pad, radius, tile := float32(10), float32(14), float32(34)
	if grid {
		pad, radius, tile = 12, 16, 44
	}
	btn.Padding(pad).Gap(10).AlignItems(ui.Center).Radius(radius).Background(cardBg).Border(1, cardEdge).
		Margin(0, 12, 12, 0).Label(it.Name)
	if grid {
		btn.Width(float32(w)).Height(float32(h))
	} else {
		btn.FillWidth()
	}
	if it.Selected {
		btn.Background(pickBg).Border(1, pickEdge)
	}
	btn.DragDataFrom(func() transfer.Data {
		if !m.Items[i].Selected {
			m.Select(i, false)
		}
		return m.dragData(m.PathsOf(i))
	}, m.dragOptions())
	btn.Children(func() {
		m.tile(c, it, tile)
		ui.Column(c).Grow(1).MinWidth(0).Gap(2).Children(func() {
			ui.Text(c, it.Name).FontSize(12).TextColor(textStrong).SingleLine().Ellipsis("…")
			if meta := it.Meta(); meta != "" {
				ui.Text(c, meta).FontSize(11).TextColor(textSoft).SingleLine().Ellipsis("…")
			}
		})
	})
	if btn.Clicked() {
		m.Select(i, btn.ClickModifiers()&ui.Ctrl != 0)
	}
}

// tile is the square of an item: the colour of its type, faint, under the
// icon of the system.
func (m *Model) tile(c *ui.Context, it Item, size float32) {
	r, g, b := it.TypeColor()
	ui.Box(c).Size(size, size).Radius(size / 3.6).Background(ui.RGBA(r, g, b, 0.18)).Center().Children(func() {
		if it.Icon != nil {
			ui.Image(c, it.Icon).Size(size*0.82, size*0.82)
			return
		}
		ui.Text(c, it.Badge()).FontWeight(700).FontSize(size / 3.2).TextColor(glyphColor)
	})
}

// icon is the icon of the system when there is one, else a badge with the
// extension in the color of its type.
func (m *Model) icon(c *ui.Context, it Item, size float32) {
	if it.Icon != nil {
		ui.Image(c, it.Icon).Size(size, size)
		return
	}
	r, g, b := it.TypeColor()
	ui.Column(c).Size(size, size).Radius(10).Background(ui.RGB(r, g, b)).AlignItems(ui.Center).Justify(ui.Center).Children(func() {
		ui.Text(c, it.Badge()).Bold().FontSize(size / 4).TextColor(ui.RGB(255, 255, 255))
	})
}

// dragData makes the files a drag takes along, and notes that the drag is
// ours, so that the shelf does not take them in again.
func (m *Model) dragData(paths []string) transfer.Data {
	m.draggingOut = true
	d, err := transfer.FileData(paths...)
	if err != nil {
		m.draggingOut = false
		return transfer.Data{}
	}
	return d
}

func (m *Model) dragOptions() transfer.DragOptions {
	return transfer.DragOptions{Operations: transfer.Copy, Done: func(transfer.Result) { m.draggingOut = false }}
}
