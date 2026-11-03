package panel

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	_ "image/gif"
	_ "image/jpeg"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"

	"github.com/egoist/mygo/ui"

	"klip/internal/app"
	"klip/internal/history"
	"klip/internal/model"
)

// Host is what the panel asks of the program around it.
type Host interface {
	// Apply puts the item on the clipboard.
	Apply(it model.Item) error
	// Hide hides the panel.
	Hide()
	// Delete removes an item, ClearAll empties the history (both also from
	// the cloud when there is an account), Reorder moves an item before
	// another one (nil: to the end).
	Delete(id string)
	ClearAll()
	Reorder(id string, before *string)
	// Sync is how the sync is doing.
	Sync() app.SyncView
	// Version is the version for the bar, as "v1.0.11".
	Version() string
	// UpdateAvailable is the version of a newer release that is known, or "".
	// UpdateApplying says that it is being installed.
	UpdateAvailable() string
	UpdateApplying() bool
	// ShowUpdate opens the update window, ShowPrefs the preferences, ShowAbout
	// the window about the author, which goes under anchor (the About chip,
	// in the window's coordinates).
	ShowUpdate()
	ShowPrefs()
	ShowAbout(anchor ui.Rect)
	// Quit ends the program.
	Quit()
	// ShowPreview opens the window beside a row that the pointer rests on,
	// where anchor is the row in the window's coordinates; HidePreview closes
	// it. Preview says which item it shows ("" for none) and whether the
	// pointer is over it.
	ShowPreview(it model.Item, anchor ui.Rect)
	HidePreview()
	Preview() (id string, hovered bool)
}

// Timings of the hover preview (MainWindow): it opens a moment after the
// pointer rests on a row, and stays a moment after it left, so that the
// pointer can cross to it.
const (
	previewShowDelay   = 350 * time.Millisecond
	previewAnchorGrace = 220 * time.Millisecond
	toastFor           = 900 * time.Millisecond
	dropFlash          = 180 * time.Millisecond
)

// section is what the list shows: all the items, or the favourites only.
type section int

const (
	sectionAll section = iota
	sectionFavorites
)

// dragRow is the value a row carries while it is dragged to a new place.
type dragRow struct{ ID string }

// dialog is the box over the window: a note being written, or a question.
type dialog struct {
	kind   string // note, confirm
	title  string
	hint   string
	text   string
	ok     string
	danger bool
	item   model.Item
	onOK   func()
	focus  bool
}

// Model is the state of the panel. History is the list it shows and edits.
type Model struct {
	History *history.Service
	Host    Host
	// Redraw asks for a new frame from any goroutine (Window.Update).
	Redraw func()
	// Now is time.Now unless a test sets it.
	Now func() time.Time

	query  string
	all    []model.Item
	fav    []model.Item
	sec    section
	chosen int
	list   ui.ListState
	input  ui.Handle
	dirty  atomic.Bool
	last   string // query the rows were built for

	dlg      *dialog
	noteHand ui.Handle

	toast      string
	toastUntil time.Time

	// hover preview
	hoverID    string
	closed     bool // the window is hidden (Closed), until Opened
	hoverSince time.Time
	hoverLeft  time.Time

	flashID    string
	flashUntil time.Time

	thumbs thumbCache
	infos  infoCache
}

// New returns a Model and hooks it to the history.
func New(h *history.Service, host Host) *Model {
	m := &Model{History: h, Host: host, Now: time.Now}
	m.dirty.Store(true)
	h.OnChanged(func() {
		m.dirty.Store(true)
		m.redraw()
	})
	return m
}

func (m *Model) redraw() {
	if m.Redraw != nil {
		m.Redraw()
	}
}

// after asks for a frame d from now, for what the frame has to do then.
func (m *Model) after(d time.Duration) {
	time.AfterFunc(d, m.redraw)
}

// PreviewLeft is for the preview window: the pointer went from it to
// nowhere, so it can close without waiting.
func (m *Model) PreviewLeft() {
	if m.hoverID == "" {
		m.hoverLeft = m.Now().Add(-previewAnchorGrace)
		m.redraw()
	}
}

// Closed is for each time the window is hidden: the row the pointer was on is
// forgotten, and no preview opens until the window is shown again (the frame
// that hid it, as Escape does, goes on with the pointer where it was). Call it
// on the UI thread.
func (m *Model) Closed() {
	m.closed = true
	m.dropHover()
}

// ShowToast shows a short message at the bottom of the window.
func (m *Model) ShowToast(msg string) {
	m.toast = msg
	m.toastUntil = m.Now().Add(toastFor)
	m.after(toastFor + 20*time.Millisecond)
	m.redraw()
}

// Reset is for each time the panel is shown: no query, the list of all the
// items, the first row chosen, the search box focused. Call it on the UI
// thread.
func (m *Model) Reset() {
	m.query, m.dlg, m.sec, m.closed = "", nil, sectionAll, false
	m.dirty.Store(true)
	m.all, m.fav, m.chosen = nil, nil, 0
	m.dropHover()
	m.list.ScrollIntoView(0) // and the list shows it, whatever it was scrolled to
	m.input.Focus()
}

// Opened is for each time the window is shown again (PrepareForOpen): the
// first row is chosen and the search box has the focus. The query and the
// folder stay as they were left, as in the .NET client. Call it on the UI
// thread.
func (m *Model) Opened() {
	m.dlg, m.closed = nil, false
	// The rows may be those of before the window was hidden; an item copied
	// since is the first one now, and the one chosen must be that.
	m.refresh()
	m.chosen = 0
	if len(m.rows()) > 0 {
		m.list.ScrollIntoView(0)
	}
	m.dropHover()
	m.input.Focus()
}

// refresh rebuilds the rows when the history or the query changed, and keeps
// the chosen item chosen.
func (m *Model) refresh() {
	if !m.dirty.Swap(false) && m.last == m.query {
		return
	}
	var keep string
	if cur := m.rows(); m.chosen >= 0 && m.chosen < len(cur) && m.last == m.query {
		keep = cur[m.chosen].ID
	}
	m.all = Filter(m.History.Items(), m.query)
	m.fav = Pinned(m.all)
	m.last = m.query
	m.chosen = 0
	if keep != "" {
		for i, it := range m.rows() {
			if it.ID == keep {
				m.chosen = i
			}
		}
	}
}

// rows is the list on show.
func (m *Model) rows() []model.Item {
	if m.sec == sectionFavorites {
		return m.fav
	}
	return m.all
}

func (m *Model) choose(i int) {
	m.chosen = max(0, min(i, len(m.rows())-1))
	if len(m.rows()) > 0 {
		m.list.ScrollIntoView(m.chosen)
	}
}

// current is the chosen item.
func (m *Model) current() (model.Item, bool) {
	r := m.rows()
	if m.chosen < 0 || m.chosen >= len(r) {
		return model.Item{}, false
	}
	return r[m.chosen], true
}

func (m *Model) enter(s section) {
	m.sec = s
	m.chosen = 0
	m.list.ScrollIntoView(0) // the first row is chosen: it is the one Enter applies, so it shows
	m.dropHover()
	m.redraw()
}

// ---- actions

type action struct {
	kind  string // apply, pin, note, delete, clear
	item  model.Item
	from  string // reorder: the moved item
	toEnd bool   // reorder: to the end, rather than before item
}

func (m *Model) run(a action) {
	switch a.kind {
	case "apply":
		m.dropHover()
		if err := m.Host.Apply(a.item); err != nil {
			m.ShowToast("写入剪贴板失败，请重试")
			return
		}
		m.ShowToast("已写回剪贴板")
		m.Host.Hide()
	case "pin":
		m.History.TogglePinned(a.item.ID)
	case "note":
		m.openNote(a.item)
	case "delete":
		m.askDelete(a.item)
	case "clear":
		m.askClear()
	case "move":
		flash := a.item.ID
		if a.toEnd {
			m.Host.Reorder(a.from, nil)
			flash = a.from
		} else {
			before := a.item.ID
			m.Host.Reorder(a.from, &before)
		}
		m.flashID, m.flashUntil = flash, m.Now().Add(dropFlash)
		m.after(dropFlash + 20*time.Millisecond)
	}
}

func (m *Model) cloud() bool { return m.Host.Sync().Cloud }

func (m *Model) openNote(it model.Item) {
	m.dropHover()
	m.dlg = &dialog{kind: "note", title: "添加备注", hint: "请输入备注（可用于搜索）：",
		text: model.Str(it.Note), ok: "保存", item: it, focus: true}
}

func (m *Model) askDelete(it model.Item) {
	m.dropHover()
	del := func() { m.Host.Delete(it.ID) }
	if m.cloud() {
		m.dlg = &dialog{kind: "confirm", title: "删除记录", hint: "此操作将同时删除云端备份，且不可撤销。\n\n是否继续？",
			ok: "确定", danger: true, onOK: del}
		return
	}
	del()
}

func (m *Model) askClear() {
	m.dropHover()
	clear := func() { m.Host.ClearAll() }
	hint := "确定要清除本工具记录的全部剪贴记录吗？\n此操作不可撤销。"
	if m.cloud() {
		hint = "此操作将同时删除云端备份，且不可撤销。\n\n是否继续？"
	}
	m.dlg = &dialog{kind: "confirm", title: "清除全部记录", hint: hint, ok: "清除", danger: true, onOK: clear}
}

// ---- hover preview

// dropHover closes the preview and forgets the row the pointer was on.
func (m *Model) dropHover() {
	m.hoverID, m.hoverLeft = "", time.Time{}
	if m.Host != nil {
		m.Host.HidePreview()
	}
}

// hovering is told, once a frame after the rows are built, which row the
// pointer is on (nil for none) and where that row is. The preview opens
// after the delay, and closes after the grace unless the pointer went over
// to it.
func (m *Model) hovering(it *model.Item, rect ui.Rect) {
	now := m.Now()
	if m.dlg != nil || m.closed {
		it = nil
	}
	shown, over := m.Host.Preview()
	if it != nil {
		m.hoverLeft = time.Time{}
		switch {
		case m.hoverID != it.ID:
			m.hoverID, m.hoverSince = it.ID, now
			m.after(previewShowDelay + 10*time.Millisecond)
		case shown != it.ID && now.Sub(m.hoverSince) >= previewShowDelay:
			m.Host.ShowPreview(*it, rect)
		}
		return
	}
	if m.hoverID != "" {
		m.hoverID, m.hoverLeft = "", now
		m.after(previewAnchorGrace + 10*time.Millisecond)
	}
	if shown != "" && !m.hoverLeft.IsZero() && !over && now.Sub(m.hoverLeft) >= previewAnchorGrace {
		m.Host.HidePreview()
	}
}

// ---- thumbnails

// thumbCache keeps small pictures of the rows in view. The pictures are made
// in the background, one at a time, and cut down to the size they are drawn
// at: a screenshot is never held in memory as a whole.
type thumbCache struct {
	mu       sync.Mutex
	failedAt map[string]time.Time
	items    map[string]*ui.Bitmap // nil: could not be made
	pending  map[string]bool
	order    []string
	slot     chan struct{} // one picture is decoded at a time
	redraw   func()
}

const (
	thumbLimit = 64
	thumbSize  = 72 // twice the 36 points it is drawn at, for sharp screens
	// thumbMaxPixels is the largest picture that is decoded for a thumbnail
	// (an 8K screen is 33 million): decoding costs four bytes a pixel.
	thumbMaxPixels = 40_000_000
	// thumbMaxBytes is the largest file that is read for one: a bitmap of
	// thumbMaxPixels is about this big; a format that only MyGo knows (its size
	// cannot be told beforehand) gets a tenth of it.
	thumbMaxBytes        = 256 << 20
	thumbMaxUnknownBytes = 32 << 20
	// thumbMaxUnknownPixels is the largest picture of such a format that is
	// kept: MyGo's bitmap cannot be cut down, so the cache would hold it whole
	// (thumbLimit of them), and its size is only known once it is decoded.
	thumbMaxUnknownPixels = 2_000_000
)

// get returns the thumbnail, or nil while it is being made (the window is
// redrawn when it is ready) or when the picture cannot be read.
func (t *thumbCache) get(path string, redraw func()) *ui.Bitmap {
	t.mu.Lock()
	defer t.mu.Unlock()
	if bm, ok := t.items[path]; ok {
		if bm != nil || time.Since(t.failedAt[path]) < 5*time.Second {
			return bm
		}
		delete(t.items, path)
	}
	if t.pending[path] || path == "" {
		return nil
	}
	if t.pending == nil {
		t.pending = map[string]bool{}
		t.slot = make(chan struct{}, 1)
	}
	t.pending[path] = true
	go t.make(path, redraw)
	return nil
}

func (t *thumbCache) make(path string, redraw func()) {
	t.slot <- struct{}{}
	bm := smallBitmap(path)
	<-t.slot
	t.mu.Lock()
	delete(t.pending, path)
	if t.items == nil {
		t.items = map[string]*ui.Bitmap{}
	}
	t.items[path] = bm
	if bm == nil {
		if t.failedAt == nil {
			t.failedAt = map[string]time.Time{}
		}
		t.failedAt[path] = time.Now()
	}
	t.order = append(t.order, path)
	if len(t.order) > thumbLimit {
		delete(t.items, t.order[0])
		t.order = t.order[1:]
	}
	t.mu.Unlock()
	if redraw != nil {
		redraw()
	}
}

// smallBitmap reads a picture and cuts it down to fit thumbSize.
func smallBitmap(path string) *ui.Bitmap {
	bm, _, _ := ScaledBitmap(path, thumbSize)
	return bm
}

// ScaledBitmap reads a picture and cuts it down to fit maxSide (a picture that
// is smaller is left as it is). It also returns the size the picture has in its
// file. It is bounded in what it reads and decodes (see the thumbMax limits),
// and slow: call it away from the UI thread. nil: it could not be made.
func ScaledBitmap(path string, maxSide int) (*ui.Bitmap, int, int) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0
	}
	defer f.Close()
	// A small file can hold a huge picture, and a big one can be a huge bitmap:
	// the header says which, before anything is read into memory.
	known := false
	var ow, oh int
	if cfg, _, err := image.DecodeConfig(f); err == nil {
		if cfg.Width*cfg.Height > thumbMaxPixels {
			return nil, 0, 0
		}
		known, ow, oh = true, cfg.Width, cfg.Height
	}
	limit := int64(thumbMaxBytes)
	if !known {
		limit = thumbMaxUnknownBytes
	}
	st, err := f.Stat()
	if err != nil || st.Size() > limit {
		return nil, 0, 0
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, 0
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, 0, 0
	}
	var img image.Image
	if known {
		img, _, err = image.Decode(bytes.NewReader(data))
	}
	if !known || err != nil {
		bm, _ := ui.DecodeBitmap(data) // a format only MyGo knows
		if bm == nil {
			return nil, 0, 0
		}
		w, h := bm.Size()
		if w*h > thumbMaxUnknownPixels {
			return nil, 0, 0
		}
		return bm, w, h
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, 0, 0
	}
	scale := float64(maxSide) / float64(max(w, h))
	if scale > 1 {
		scale = 1
	}
	tw, th := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	small := image.NewNRGBA(image.Rect(0, 0, tw, th))
	for y := 0; y < th; y++ {
		for x := 0; x < tw; x++ {
			// the average of the source block that this pixel stands for
			x0, x1 := b.Min.X+x*w/tw, b.Min.X+max(x*w/tw+1, (x+1)*w/tw)
			y0, y1 := b.Min.Y+y*h/th, b.Min.Y+max(y*h/th+1, (y+1)*h/th)
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					pr, pg, pb, pa := img.At(xx, yy).RGBA()
					r, g, bl, a, n = r+uint64(pr), g+uint64(pg), bl+uint64(pb), a+uint64(pa), n+1
				}
			}
			if n == 0 || a == 0 {
				continue
			}
			small.SetNRGBA(x, y, color.NRGBA{
				R: uint8((r * 0xffff / a) >> 8), G: uint8((g * 0xffff / a) >> 8), B: uint8((bl * 0xffff / a) >> 8), A: uint8((a / n) >> 8),
			})
		}
	}
	var out bytes.Buffer
	if png.Encode(&out, small) != nil {
		return nil, 0, 0
	}
	bm, _ := ui.DecodeBitmap(out.Bytes())
	return bm, ow, oh
}
