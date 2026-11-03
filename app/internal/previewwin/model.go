// Package previewwin is the window that opens beside a row of the main
// window when the pointer rests on it (ImagePreviewWindow): the picture, the
// text or the files of the item, and what is known of it. It never takes the
// focus.
package previewwin

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"klip/internal/model"
	"klip/internal/panel"
)

// Model is the content of the window.
type Model struct {
	Item model.Item
	// Bitmap is the picture of an image item, Width and Height its size.
	Bitmap        *ui.Bitmap
	Width, Height int
	// MetaLine is Meta, worked out off the UI thread: it reads the files,
	// which may be on a network share that answers slowly or not at all.
	MetaLine string
	// Open opens the original of an image.
	Open func(path string)
	// Left is called when the pointer, which was over the window, left it.
	Left func()

	over, wasOver bool
	icon          *ui.Bitmap
	// files is the list of the files, which builds only the rows in view (an
	// item may hold thousands); filesOf the item it shows.
	files   ui.ListState
	filesOf string
}

// Over says that the pointer is over the window.
func (m *Model) Over() bool { return m.over }

// Window sizes (MainWindow.PositionAndShowPreview).
const (
	TextWidth, TextHeight = 360, 320
	FileWidth, FileHeight = 320, 170

	chrome    = 20
	infoH     = 88
	minWindow = 260
	maxWindow = 980
)

// fileRowH is the height of a line of the list of files, fileRows how many
// lines show before it scrolls, fileGap the line under the list.
const fileRowH, fileRows, fileGap = 18, 6, 17

// noteMaxH is the height a note takes at most (three lines) before it
// scrolls.
const noteMaxH = 54

// Size is the size of the window for an item: fixed for text and files, and
// following the picture for an image, which is shown at least as large as a
// short side that fits the smallest window, and no larger than the largest.
func Size(it model.Item, pw, ph int) (w, h int) {
	switch it.Kind {
	case model.KindFileList:
		n := min(len(Files(it)), fileRows)
		if n == 0 {
			return FileWidth, FileHeight
		}
		return FileWidth, FileHeight + n*fileRowH + fileGap
	case model.KindImage:
		if pw <= 0 || ph <= 0 {
			return minWindow, minWindow
		}
		maxImage := float64(max(1, maxWindow-chrome-infoH))
		minShort := float64(max(1, minWindow-chrome-infoH))
		maxScale := math.Min(maxImage/float64(pw), maxImage/float64(ph))
		minScale := minShort / float64(max(1, min(pw, ph)))
		scale := math.Min(maxScale, math.Max(1.0, minScale))
		sw, sh := int(math.Round(float64(pw)*scale)), int(math.Round(float64(ph)*scale))
		return clamp(sw+chrome, minWindow, maxWindow), clamp(sh+chrome+infoH, minWindow, maxWindow)
	}
	return TextWidth, TextHeight
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }

// Rect is a rectangle in screen pixels.
type Rect struct{ X, Y, W, H int }

// Place is where the window goes: to the right of the row, 12 pixels from
// it, or to the left when it would not fit; centred on the row's height, and
// kept inside the work area of the display. A window larger than the area is
// made smaller first.
func Place(anchor Rect, w, h int, wa Rect) (x, y, ww, hh int) {
	ww, hh = min(w, max(200, wa.W-24)), min(h, max(200, wa.H-24))
	const gap = 12
	x = anchor.X + anchor.W + gap
	if x+ww > wa.X+wa.W {
		x = anchor.X - gap - ww
	}
	y = anchor.Y + anchor.H/2 - hh/2
	x = max(wa.X, min(x, max(wa.X, wa.X+wa.W-ww)))
	y = max(wa.Y, min(y, max(wa.Y, wa.Y+wa.H-hh)))
	return
}

// FormatBytes is ImagePreviewWindow.FormatBytes.
func FormatBytes(n int64) string {
	if n <= 0 {
		return "0 KB"
	}
	kb := float64(n) / 1024
	if kb < 1024 {
		return fmt.Sprintf("%d KB", max(1, int64(math.Round(kb))))
	}
	mb := kb / 1024
	if mb < 1024 {
		return trimZero(fmt.Sprintf("%.1f", mb)) + " MB"
	}
	return trimZero(fmt.Sprintf("%.1f", mb/1024)) + " GB"
}

func trimZero(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// Meta is the first line under the content: how often it was copied and
// what size it has. It reads the files of the item: not on the UI thread.
func (m *Model) Meta() string {
	it := m.Item
	meta := fmt.Sprintf("复制次数：%d", max(1, int(it.CopyCount)))
	switch it.Kind {
	case model.KindImage:
		if p := model.Str(it.ImageFilePath); p != "" {
			if st, err := os.Stat(p); err == nil {
				kb := max(1, st.Size()/1024)
				if m.Bitmap != nil && m.Width > 0 {
					meta += fmt.Sprintf(" · %d×%d · %d KB", m.Width, m.Height, kb)
				} else {
					meta += fmt.Sprintf(" · %d KB", kb)
				}
			}
		}
	case model.KindFileList:
		count, size := 0, int64(0)
		for _, p := range it.FilePaths {
			if strings.TrimSpace(p) == "" {
				continue
			}
			count++
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				size += st.Size()
			}
		}
		if size <= 0 {
			size = max(0, it.ApproxBytes)
		}
		meta += fmt.Sprintf(" · %d 个文件 · %s", max(1, count), FormatBytes(size))
	}
	return meta
}

// Times is the second and third line: the first and the last copy.
func (m *Model) Times() string {
	first := m.Item.CapturedAt.Time
	if m.Item.FirstCapturedAt != nil {
		first = m.Item.FirstCapturedAt.Time
	}
	// In the user's time: what is stored, and what other devices send, may be
	// in UTC.
	const layout = "2006-01-02 15:04:05"
	return "首次：" + first.Local().Format(layout) + "\n最后：" + m.Item.CapturedAt.Time.Local().Format(layout)
}

// NoteLine is the note, or its absence.
func (m *Model) NoteLine() string {
	n := model.Str(m.Item.Note)
	if strings.TrimSpace(n) == "" {
		n = "（无备注）"
	}
	count := 0
	for i := range n {
		if count == textRunes {
			n = n[:i] + "…"
			break
		}
		count++
	}
	return "备注：" + n
}

// Source is the program the item came from, or "" when that is unknown.
func (m *Model) Source() string {
	name, path := strings.TrimSpace(model.Str(m.Item.SourceAppName)), strings.TrimSpace(model.Str(m.Item.SourceAppPath))
	switch {
	case name != "":
		return name
	case path != "":
		base := filepath.Base(strings.ReplaceAll(path, "\\", "/"))
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
	return ""
}

// textRunes is how much of a text the window lays out: a scroll area does not
// make a single text element cheaper, and a log of many megabytes would be
// measured whole on the UI thread.
const textRunes = 20000

// Text is what the text area shows: the beginning of a very long text.
func (m *Model) Text() string {
	t := model.Str(m.Item.Text)
	n := textRunes
	for i := range t {
		if n == 0 {
			return t[:i] + "…"
		}
		n--
	}
	return t
}

// View is the window's content.
func (m *Model) View(c *ui.Context) {
	p := panel.PaletteFor(c.Theme().Dark)
	c.Root().Background(ui.Transparent)
	it := m.Item

	root := ui.Column(c).Fill().Padding(10).Background(p.PreviewBg).Border(1, p.Divider).Radius(12).Clip()
	m.over = root.Hovered()
	if m.wasOver && !m.over && m.Left != nil {
		m.Left()
	}
	m.wasOver = m.over
	root.Children(func() {
		files := Files(it)
		if it.Kind != model.KindFileList || len(files) > 0 {
			ui.Box(c).Grow(1).MinHeight(0).Children(func() {
				switch it.Kind {
				case model.KindFileList:
					m.fileList(c, p, files)
				case model.KindImage:
					if m.Bitmap != nil {
						ui.Image(c, m.Bitmap).Fill().Fit(ui.Contain)
					}
				default:
					ui.Scroll(c).Fill().Children(func() {
						ui.Text(c, m.Text()).FontSize(13).TextColor(p.Text).Wrap().FillWidth()
					})
				}
			})
			ui.Box(c).FillWidth().Height(1).Background(p.Divider).Margin(8, 0)
		}
		ui.Column(c).FillWidth().Gap(4).Children(func() {
			if it.Kind == model.KindImage && m.openable() {
				b := ui.ButtonBase(c)
				bg := p.PreviewBtn
				if b.Hovered() {
					bg = p.PreviewBtn.Alpha(0.5)
				}
				b.Padding(4, 10).Radius(8).Border(1, p.Divider).Background(bg).Label("查看原图").Children(func() {
					ui.Text(c, "查看原图").FontSize(12).TextColor(p.Text)
				})
				if b.Clicked() && m.Open != nil {
					m.Open(model.Str(it.ImageFilePath))
				}
			}
			if src := m.Source(); src != "" || model.Str(it.SourceAppPath) != "" {
				if src == "" {
					src = "(未知来源)"
				}
				ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
					if m.icon != nil {
						ui.Image(c, m.icon).Size(18, 18).Fit(ui.Cover)
					}
					ui.Text(c, src).FontSize(11).TextColor(p.Muted).Wrap()
				})
			}
			ui.Text(c, m.MetaLine).FontSize(11).TextColor(p.Muted).Wrap()
			ui.Text(c, m.Times()).FontSize(11).TextColor(p.Muted2).Wrap()
			// A note may run to many lines: it scrolls past a few, so that the
			// item stays in view.
			ui.Scroll(c).FillWidth().MaxHeight(noteMaxH).Children(func() {
				ui.Text(c, m.NoteLine()).FontSize(12).TextColor(p.Text).Wrap().FillWidth()
			})
		})
	})
}

// Files are the files of a file item, as listed in the window: the row of the
// main window names only the first.
func Files(it model.Item) []string {
	var out []string
	for _, p := range it.FilePaths {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// fileList is the list of the files: the name, then the folder it is in. It
// starts at the top for each item.
func (m *Model) fileList(c *ui.Context, p panel.Palette, files []string) {
	if m.filesOf != m.Item.ID {
		m.filesOf = m.Item.ID
		m.files.ScrollTo(0, ui.Start)
	}
	m.files.Label = func(i int) string { return files[i] }
	ui.List(c, &m.files, len(files), func(i int) {
		f := files[i]
		ui.Row(c).FillWidth().Height(fileRowH).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Text(c, filepath.Base(f)).FontSize(12).TextColor(p.Text).SingleLine().Ellipsis("…").MinWidth(0)
			ui.Text(c, filepath.Dir(f)).FontSize(11).TextColor(p.Muted).SingleLine().Ellipsis("…").Grow(1).MinWidth(0)
		})
	}).Fill()
}

// openable says that the original of the picture is there to open: it was
// read for the window just now (the window draws often, and does not look at
// the disk each time).
func (m *Model) openable() bool {
	return m.Bitmap != nil && strings.TrimSpace(model.Str(m.Item.ImageFilePath)) != ""
}

var _ = time.Now
