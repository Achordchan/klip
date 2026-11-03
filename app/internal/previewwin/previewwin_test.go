package previewwin

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"klip/internal/model"
)

func item(kind model.Kind) model.Item {
	t := model.Time{Time: time.Date(2026, 5, 30, 18, 5, 9, 0, time.UTC)}
	first := model.Time{Time: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}
	return model.Item{ID: "1", Kind: kind, CapturedAt: t, FirstCapturedAt: &first, CopyCount: 3}
}

func TestSize(t *testing.T) {
	if w, h := Size(item(model.KindText), 0, 0); w != 360 || h != 320 {
		t.Fatalf("text %dx%d", w, h)
	}
	if w, h := Size(item(model.KindFileList), 0, 0); w != 320 || h != 170 {
		t.Fatalf("files %dx%d", w, h)
	}
	img := item(model.KindImage)
	// small pictures are enlarged to a short side of 152: 100x50 -> 304x152, window 324 x 260
	if w, h := Size(img, 100, 50); w != 324 || h != 260 {
		t.Fatalf("small image %dx%d", w, h)
	}
	// large ones are scaled to the largest picture (980-20-88 = 872 on its long side)
	if w, h := Size(img, 4000, 3000); w != 892 || h != 762 {
		t.Fatalf("large image %dx%d", w, h)
	}
	if w, h := Size(img, 0, 0); w != 260 || h != 260 {
		t.Fatalf("unknown image %dx%d", w, h)
	}
}

func TestPlace(t *testing.T) {
	wa := Rect{0, 0, 1920, 1080}
	// to the right of the row, centred on its height
	x, y, w, h := Place(Rect{100, 400, 360, 30}, 360, 320, wa)
	if x != 100+360+12 || y != 400+15-160 || w != 360 || h != 320 {
		t.Fatalf("right: %d,%d %dx%d", x, y, w, h)
	}
	// no room on the right: to the left
	x, _, _, _ = Place(Rect{1500, 400, 360, 30}, 360, 320, wa)
	if x != 1500-12-360 {
		t.Fatalf("left: x=%d", x)
	}
	// kept inside the area at the top and the bottom
	_, y, _, _ = Place(Rect{100, 5, 360, 30}, 360, 320, wa)
	if y != 0 {
		t.Fatalf("top: y=%d", y)
	}
	_, y, _, _ = Place(Rect{100, 1060, 360, 30}, 360, 320, wa)
	if y != 1080-320 {
		t.Fatalf("bottom: y=%d", y)
	}
	// larger than the area: made smaller
	_, _, w, h = Place(Rect{0, 0, 10, 10}, 980, 980, Rect{0, 0, 800, 600})
	if w != 776 || h != 576 {
		t.Fatalf("shrunk to %dx%d", w, h)
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 KB", 1: "1 KB", 1500: "1 KB", 2048: "2 KB", 1 << 20: "1 MB", 3 << 19: "1.5 MB", 5 << 30: "5 GB"} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestLines(t *testing.T) {
	saved := time.Local
	time.Local = time.UTC // the times below are written as UTC shows them
	defer func() { time.Local = saved }()
	m := &Model{Item: item(model.KindText)}
	if got := m.Meta(); got != "复制次数：3" {
		t.Fatalf("meta = %q", got)
	}
	if got := m.Times(); got != "首次：2026-05-01 09:00:00\n最后：2026-05-30 18:05:09" {
		t.Fatalf("times = %q", got)
	}
	if m.NoteLine() != "备注：（无备注）" {
		t.Fatalf("note = %q", m.NoteLine())
	}
	m.Item.Note = model.Ptr("买菜")
	if m.NoteLine() != "备注：买菜" {
		t.Fatalf("note = %q", m.NoteLine())
	}
	m.Item.FirstCapturedAt = nil
	if got := m.Times(); got != "首次：2026-05-30 18:05:09\n最后：2026-05-30 18:05:09" {
		t.Fatalf("without a first time: %q", got)
	}

	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(a, make([]byte, 3000), 0o644)
	f := &Model{Item: item(model.KindFileList)}
	f.Item.FilePaths = []string{a, filepath.Join(dir, "gone.txt")}
	if got := f.Meta(); got != "复制次数：3 · 2 个文件 · 3 KB" {
		t.Fatalf("files meta = %q", got)
	}
	f.Item.FilePaths = []string{filepath.Join(dir, "gone.txt")}
	f.Item.ApproxBytes = 2 << 20
	if got := f.Meta(); got != "复制次数：3 · 1 个文件 · 2 MB" {
		t.Fatalf("files that are gone use the recorded size: %q", got)
	}

	if (&Model{}).Source() != "" {
		t.Fatal("no source")
	}
	s := &Model{Item: item(model.KindText)}
	s.Item.SourceAppPath = model.Ptr(`C:\Program Files\Notepad++\notepad++.exe`)
	if s.Source() != "notepad++" {
		t.Fatalf("source from the path = %q", s.Source())
	}
	s.Item.SourceAppName = model.Ptr("记事本")
	if s.Source() != "记事本" {
		t.Fatalf("source = %q", s.Source())
	}
}

// The times are shown in the user's time, also when they are stored in UTC.
func TestTimesAreLocal(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("UTC+8", 8*3600)
	defer func() { time.Local = saved }()
	m := &Model{Item: item(model.KindText)}
	if got := m.Times(); got != "首次：2026-05-01 17:00:00\n最后：2026-05-31 02:05:09" {
		t.Fatalf("times = %q", got)
	}
}

func TestViewAndLeft(t *testing.T) {
	m := &Model{Item: item(model.KindText)}
	m.Item.Text = model.Ptr("hello preview")
	m.MetaLine = m.Meta() // as the manager does, off the UI thread
	left := 0
	m.Left = func() { left++ }
	tt := ui.NewTester(m.View, 360, 320)
	for _, s := range []string{"hello preview", "复制次数：3", "备注：（无备注）"} {
		if !tt.HasText(s) {
			t.Fatalf("missing %q in %q", s, tt.Texts())
		}
	}
	tt.Move(100, 100)
	tt.Frame()
	if !m.Over() {
		t.Fatal("not over")
	}
	tt.Move(-5, -5)
	tt.Frame()
	if m.Over() || left != 1 {
		t.Fatalf("over=%v left=%d", m.Over(), left)
	}
}

func TestImageButtonOpens(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.png")
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for x := 0; x < 40; x++ {
		for y := 0; y < 20; y++ {
			img.Set(x, y, color.RGBA{200, 50, 50, 255})
		}
	}
	f, _ := os.Create(p)
	_ = png.Encode(f, img)
	f.Close()
	data, _ := os.ReadFile(p)
	bm, err := ui.DecodeBitmap(data)
	if err != nil {
		t.Fatal(err)
	}
	m := &Model{Item: item(model.KindImage), Bitmap: bm, Width: 40, Height: 20}
	m.Item.ImageFilePath = model.Ptr(p)
	m.MetaLine = m.Meta()
	var opened string
	m.Open = func(path string) { opened = path }
	tt := ui.NewTester(m.View, 324, 260)
	if !tt.HasText("复制次数：3 · 40×20 · 1 KB") {
		t.Fatalf("texts: %q", tt.Texts())
	}
	if err := tt.Click("查看原图"); err != nil {
		t.Fatal(err)
	}
	if opened != p {
		t.Fatalf("opened %q", opened)
	}
}

func TestTextOfAHugeEntryIsCut(t *testing.T) {
	big := strings.Repeat("字", 3_000_000)
	m := &Model{Item: model.Item{Kind: model.KindText, Text: model.Ptr(big)}}
	got := []rune(m.Text())
	if len(got) != textRunes+1 || got[len(got)-1] != '…' {
		t.Fatalf("%d runes", len(got))
	}
	short := &Model{Item: model.Item{Kind: model.KindText, Text: model.Ptr("hello")}}
	if short.Text() != "hello" {
		t.Fatal("a short text was changed")
	}
}

// The window of a file item lists every file, which the row names only the
// first of; a long list scrolls in a window of a set height.
func TestFilesAreListed(t *testing.T) {
	m := &Model{Item: item(model.KindFileList)}
	m.Item.FilePaths = []string{"/docs/a report.pdf", "", "/docs/b.xlsx", "/tmp/c.zip"}
	m.MetaLine = m.Meta()
	w, h := Size(m.Item, 0, 0)
	if w != FileWidth || h != FileHeight+3*fileRowH+fileGap {
		t.Fatalf("size %dx%d", w, h)
	}
	tt := ui.NewTester(m.View, w, h)
	for _, s := range []string{"a report.pdf", "b.xlsx", "c.zip", "/docs", "复制次数：3"} {
		if !tt.HasText(s) {
			t.Fatalf("missing %q in %q", s, tt.Texts())
		}
	}
	if b, ok := tt.Find("c.zip"); !ok || b.H <= 0 || b.Y+b.H > float32(h) {
		t.Fatalf("the last file is not in view: %+v %v", b, ok)
	}

	many := item(model.KindFileList)
	for i := range 20 {
		many.FilePaths = append(many.FilePaths, filepath.Join("/docs", "file"+string(rune('a'+i))+".txt"))
	}
	if _, h := Size(many, 0, 0); h != FileHeight+fileRows*fileRowH+fileGap {
		t.Fatalf("a long list: height %d", h)
	}
}

// A note of many lines scrolls in a few, and leaves the item in view.
func TestLongNoteScrolls(t *testing.T) {
	m := &Model{Item: item(model.KindText)}
	m.Item.Text = model.Ptr("hello preview")
	m.Item.Note = model.Ptr(strings.Repeat("a line of the note\n", 40) + "the end")
	m.MetaLine = m.Meta()
	tt := ui.NewTester(m.View, TextWidth, TextHeight)
	b, ok := tt.Find("hello preview")
	if !ok || b.H <= 0 || b.Y < 0 || b.Y+b.H > TextHeight {
		t.Fatalf("the text is out of view: %+v %v", b, ok)
	}
	if !tt.HasText("复制次数：3") {
		t.Fatalf("texts: %q", tt.Texts())
	}
	note, ok := tt.Find(m.NoteLine())
	if !ok || note.H > noteMaxH+1 || note.Y+note.H > TextHeight {
		t.Fatalf("the note takes %+v", note)
	}
}

// Of thousands of files, only the rows in view are built.
func TestManyFilesBuildOnlyTheRowsInView(t *testing.T) {
	m := &Model{Item: item(model.KindFileList)}
	for i := range 5000 {
		m.Item.FilePaths = append(m.Item.FilePaths, filepath.Join("/docs", fmt.Sprintf("file%04d.txt", i)))
	}
	m.MetaLine = m.Meta()
	w, h := Size(m.Item, 0, 0)
	tt := ui.NewTester(m.View, w, h)
	if !tt.HasText("file0000.txt") {
		t.Fatalf("texts: %q", tt.Texts())
	}
	if tt.HasText("file4999.txt") || tt.HasText("file0100.txt") {
		t.Fatal("rows out of view were built")
	}
}

// A Show still reading its files when the program quits is not shown, nor is
// a Show after.
func TestClosedManagerShowsNothing(t *testing.T) {
	mg := &Manager{}
	mg.mu.Lock()
	mg.seq++ // a Show whose files are being read
	pending := mg.seq
	mg.mu.Unlock()
	mg.Close()
	mg.mu.Lock()
	ok := mg.wanted(pending)
	mg.mu.Unlock()
	if ok {
		t.Fatal("the pending Show is still wanted")
	}
	mg.Show(item(model.KindFileList), Rect{})
	if id, _ := mg.Preview(); id != "" {
		t.Fatalf("shown after Close: %q", id)
	}
	mg.mu.Lock()
	ok = mg.wanted(mg.seq)
	mg.mu.Unlock()
	if ok {
		t.Fatal("a request is wanted after Close")
	}
}

// A picture that cannot be shown is read once while the pointer stays on its
// row, though the panel asks for it again each frame; another item, or Hide,
// lets it be tried again.
func TestFailedPictureIsNotReadAgain(t *testing.T) {
	var mu sync.Mutex
	reads := map[string]int{}
	mg := &Manager{
		decode: func(path string, _ int) (*ui.Bitmap, int, int) {
			mu.Lock()
			reads[path]++
			mu.Unlock()
			return nil, 0, 0 // broken, or too big
		},
		onMain: func(f func()) { f() },
	}
	pic := func(id string) model.Item {
		it := item(model.KindImage)
		it.ID, it.ImageFilePath = id, model.Ptr("/pics/"+id+".png")
		return it
	}
	count := func(id string) int { mu.Lock(); defer mu.Unlock(); return reads["/pics/"+id+".png"] }
	failed := func() string { mg.mu.Lock(); defer mg.mu.Unlock(); return mg.failed }
	wait := func(what string, f func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !f(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}

	mg.Show(pic("a"), Rect{})
	wait("a failed", func() bool { return failed() == "a" })
	for range 10 { // the frames that follow
		mg.Show(pic("a"), Rect{})
	}
	time.Sleep(20 * time.Millisecond)
	if n := count("a"); n != 1 {
		t.Fatalf("a was read %d times", n)
	}
	mg.Show(pic("b"), Rect{}) // another row
	wait("b failed", func() bool { return failed() == "b" })
	mg.Show(pic("a"), Rect{})
	wait("a failed again", func() bool { return failed() == "a" && count("a") == 2 })
	mg.Hide()
	mg.Show(pic("a"), Rect{})
	wait("a after Hide", func() bool { return count("a") == 3 })
}
