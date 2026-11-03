// Package shelfwin is the shelf window: a small always-on-top window that
// files are dropped on and dragged out of again (ShelfWindow of the C#
// client). It does not take the keyboard focus, so it can appear under a
// drag that is in progress without disturbing the program being dragged from.
package shelfwin

import (
	"image"
	"os"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo/ui"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"
)

// Sizes of the window: small with one thing or nothing, large when the
// files are listed.
const (
	CompactSize  = 220
	ExpandedSize = 420
)

// Item is a file on the shelf.
type Item struct {
	Path string
	Name string
	Size int64
	// Width and Height are the pixels of an image, 0 for other files.
	Width, Height int
	Icon          *ui.Bitmap
	Selected      bool
}

// Host is what the shelf asks of the window around it.
type Host interface {
	// Close closes the shelf window.
	Close()
	// Resize sets the size of the window.
	Resize(size int)
}

// Model is the state of one shelf (ShelfWindowViewModel).
type Model struct {
	Host Host
	// IconOf returns the icon of the system for a file, nil when there is
	// none (the window draws a badge with the extension then).
	IconOf func(path string) *ui.Bitmap

	Items    []Item
	Pinned   bool // there is something on it
	Expanded bool
	Grid     bool

	// draggingOut is true while files are dragged from this window, so that
	// they are not dropped on it again.
	draggingOut bool
	// Post runs a function on the UI thread (and draws a frame); nil means the
	// file information is read at once, as the tests do.
	Post     func(func())
	dragOver bool
	list     ui.ListState
}

// New returns an empty shelf.
func New(h Host) *Model { return &Model{Host: h, Grid: true} }

// AddFiles puts files on the shelf: blank and repeated paths (any case) are
// left out. With more than one file the shelf lists them, expanded.
func (m *Model) AddFiles(paths []string) {
	seen := map[string]bool{}
	for _, it := range m.Items {
		seen[strings.ToLower(it.Path)] = true
	}
	added := false
	for _, p := range paths {
		if strings.TrimSpace(p) == "" || seen[strings.ToLower(p)] {
			continue
		}
		seen[strings.ToLower(p)] = true
		name := filepath.Base(strings.ReplaceAll(p, "\\", "/"))
		if strings.TrimSpace(name) == "" || name == "." || name == "/" {
			name = p
		}
		it := Item{Path: p, Name: name}
		if m.Post == nil {
			m.fill(&it)
		} else {
			// The size, the picture size and the icon ask the file system, which
			// may be a network share that does not answer: the item is on the
			// shelf at once and gets them when they come.
			go func(path string) {
				// A few at a time: a drop of thousands of files on a share that
				// does not answer must not park a thread in each of them.
				metaSlots <- struct{}{}
				var meta Item
				meta.Path = path
				m.fill(&meta)
				<-metaSlots
				m.Post(func() {
					for i := range m.Items {
						if m.Items[i].Path == path {
							m.Items[i].Size, m.Items[i].Width, m.Items[i].Height, m.Items[i].Icon = meta.Size, meta.Width, meta.Height, meta.Icon
						}
					}
				})
			}(p)
		}
		m.Items = append(m.Items, it)
		added = true
	}
	if !added {
		return
	}
	m.Pinned = true
	if len(m.Items) > 1 {
		m.Grid = false
		m.setExpanded(true)
	}
}

// metaSlots bounds the file-system lookups running for dropped files.
var metaSlots = make(chan struct{}, 4)

// fill reads what the file system says about the file of it.
func (m *Model) fill(it *Item) {
	if st, err := os.Stat(it.Path); err == nil && !st.IsDir() {
		it.Size = st.Size()
		it.Width, it.Height = imageSize(it.Path)
	}
	if m.IconOf != nil {
		it.Icon = m.IconOf(it.Path)
	}
}

func (m *Model) setExpanded(on bool) {
	m.Expanded = on
	if m.Host != nil {
		if on {
			m.Host.Resize(ExpandedSize)
		} else {
			m.Host.Resize(CompactSize)
		}
	}
}

// ToggleExpanded opens or folds the shelf. With several files it stays open
// by its own rules: the list is the only view (the C# command does nothing
// then, and a click on the stack toggles it, which is Fold).
func (m *Model) ToggleExpanded() {
	if len(m.Items) > 1 {
		return
	}
	m.setExpanded(!m.Expanded)
}

// FoldStack is a click on the stack of several files: open or fold.
func (m *Model) FoldStack() {
	if len(m.Items) > 1 {
		m.setExpanded(!m.Expanded)
	}
}

// ToggleView switches between the grid and the list (one file only).
func (m *Model) ToggleView() {
	if len(m.Items) > 1 {
		return
	}
	m.Grid = !m.Grid
}

// Title is TitleText: what to put there, a file's name, or how many.
func (m *Model) Title() string {
	switch len(m.Items) {
	case 0:
		return "放置你的内容项"
	case 1:
		return m.Items[0].Name
	}
	return itoa(len(m.Items)) + " 个文件"
}

// TotalSize is TotalSizeText.
func (m *Model) TotalSize() string {
	var total int64
	for _, it := range m.Items {
		if it.Size > 0 {
			total += it.Size
		}
	}
	if total <= 0 {
		return ""
	}
	return FormatBytes(total)
}

// CountBadge is CountBadgeText.
func (m *Model) CountBadge() string {
	if len(m.Items) <= 1 {
		return ""
	}
	return itoa(len(m.Items)) + " 档 >"
}

// AllPaths are the files of the whole shelf: what dragging the stack takes.
func (m *Model) AllPaths() []string {
	out := make([]string, 0, len(m.Items))
	for _, it := range m.Items {
		out = append(out, it.Path)
	}
	return out
}

// PathsOf are the files that dragging item i takes: the ones chosen, or that
// item when none is.
func (m *Model) PathsOf(i int) []string {
	var out []string
	for _, it := range m.Items {
		if it.Selected {
			out = append(out, it.Path)
		}
	}
	if len(out) == 0 && i >= 0 && i < len(m.Items) {
		out = append(out, m.Items[i].Path)
	}
	return out
}

// Select chooses one item; with add, as Ctrl-click does, it toggles it and
// keeps the others.
func (m *Model) Select(i int, add bool) {
	for j := range m.Items {
		switch {
		case j == i && add:
			m.Items[j].Selected = !m.Items[j].Selected
		case j == i:
			m.Items[j].Selected = true
		case !add:
			m.Items[j].Selected = false
		}
	}
}

// Close is the × button: the shelf is emptied and closed.
func (m *Model) Close() {
	m.Items, m.Pinned = nil, false
	if m.Host != nil {
		m.Host.Close()
	}
}

func imageSize(path string) (w, h int) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".bmp", ".gif", ".webp":
	default:
		return 0, 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// FormatBytes is FormatBytes of ShelfWindowViewModel: 1024-based, one decimal.
func FormatBytes(n int64) string {
	switch {
	case n < 1024:
		return itoa(int(n)) + " B"
	case n < 1<<20:
		return trim(float64(n)/1024) + " KB"
	case n < 1<<30:
		return trim(float64(n)/(1<<20)) + " MB"
	}
	return trim(float64(n)/(1<<30)) + " GB"
}

// Meta is MetaText of an item: its size and, for an image, its pixels.
func (it Item) Meta() string {
	var parts []string
	if it.Size > 0 {
		parts = append(parts, FormatBytes(it.Size))
	}
	if it.Width > 0 && it.Height > 0 {
		parts = append(parts, itoa(it.Width)+"x"+itoa(it.Height))
	}
	return strings.Join(parts, " · ")
}

// TypeColor is the color of an item's badge by file type (as the C# window).
func (it Item) TypeColor() (r, g, b uint8) {
	switch strings.ToLower(filepath.Ext(it.Path)) {
	case ".xlsx", ".xls", ".csv":
		return 0x2E, 0x7D, 0x32
	case ".png", ".jpg", ".jpeg", ".bmp", ".gif", ".webp":
		return 0x15, 0x65, 0xC0
	case ".pdf":
		return 0xC6, 0x28, 0x28
	case ".mp3", ".wav", ".m4a", ".aac", ".flac", ".ogg":
		return 0x61, 0x61, 0x61
	}
	return 0x75, 0x75, 0x75
}

// Badge is the short text on the badge: the extension without the dot.
func (it Item) Badge() string {
	e := strings.TrimPrefix(strings.ToUpper(filepath.Ext(it.Path)), ".")
	if e == "" {
		return "文件"
	}
	if len([]rune(e)) > 4 {
		e = string([]rune(e)[:4])
	}
	return e
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// trim writes a number with one decimal, without a trailing ".0" ({0:0.#}).
func trim(v float64) string {
	s := itoa(int(v*10+0.5) / 10)
	if frac := int(v*10+0.5) % 10; frac != 0 {
		s += "." + itoa(frac)
	}
	return s
}
