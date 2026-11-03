package shelfwin

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
)

type fakeHost struct {
	closed int
	sizes  []int
}

func (h *fakeHost) Close()          { h.closed++ }
func (h *fakeHost) Resize(size int) { h.sizes = append(h.sizes, size) }

func file(t *testing.T, dir, name string, size int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func pngFile(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAddFiles(t *testing.T) {
	dir := t.TempDir()
	h := &fakeHost{}
	m := New(h)
	if m.Title() != "放置你的内容项" || m.TotalSize() != "" || m.CountBadge() != "" || m.Pinned {
		t.Fatalf("empty shelf: %q %q %q %v", m.Title(), m.TotalSize(), m.CountBadge(), m.Pinned)
	}
	a := file(t, dir, "report.pdf", 2048)
	m.AddFiles([]string{a, "", "  ", strings.ToUpper(a), a})
	if len(m.Items) != 1 || !m.Pinned || m.Items[0].Name != "report.pdf" || m.Items[0].Size != 2048 {
		t.Fatalf("%+v", m.Items)
	}
	if m.Expanded || len(h.sizes) != 0 {
		t.Fatal("one file: the shelf stays small")
	}
	if m.Title() != "report.pdf" || m.TotalSize() != "2 KB" || m.CountBadge() != "" {
		t.Fatalf("%q %q %q", m.Title(), m.TotalSize(), m.CountBadge())
	}

	img := pngFile(t, dir, "pic.png", 30, 20)
	m.AddFiles([]string{img, filepath.Join(dir, "missing.txt")})
	if len(m.Items) != 3 {
		t.Fatalf("%d items (a file that is not there is still put on the shelf, as in C#)", len(m.Items))
	}
	if m.Items[1].Width != 30 || m.Items[1].Height != 20 || m.Items[1].Meta() != strings.TrimSpace(FormatBytes(m.Items[1].Size))+" · 30x20" {
		t.Fatalf("%+v meta %q", m.Items[1], m.Items[1].Meta())
	}
	if m.Items[2].Size != 0 || m.Items[2].Meta() != "" {
		t.Fatalf("%+v", m.Items[2])
	}
	if !m.Expanded || m.Grid || len(h.sizes) != 1 || h.sizes[0] != ExpandedSize {
		t.Fatalf("several files list themselves, expanded: expanded=%v grid=%v sizes=%v", m.Expanded, m.Grid, h.sizes)
	}
	if m.Title() != "3 个文件" || m.CountBadge() != "3 档 >" {
		t.Fatalf("%q %q", m.Title(), m.CountBadge())
	}
	// Nothing new: nothing changes.
	m.AddFiles([]string{a})
	if len(h.sizes) != 1 {
		t.Fatalf("resized again: %v", h.sizes)
	}
}

func TestToggles(t *testing.T) {
	dir := t.TempDir()
	h := &fakeHost{}
	m := New(h)
	m.AddFiles([]string{file(t, dir, "a.txt", 1)})
	m.ToggleExpanded()
	if !m.Expanded || h.sizes[len(h.sizes)-1] != ExpandedSize {
		t.Fatalf("%v %v", m.Expanded, h.sizes)
	}
	m.ToggleView()
	if m.Grid {
		t.Fatal("the grid was not switched off")
	}
	m.ToggleExpanded()
	if m.Expanded || h.sizes[len(h.sizes)-1] != CompactSize {
		t.Fatalf("%v %v", m.Expanded, h.sizes)
	}
	// With several files the buttons do nothing; the stack folds.
	m.AddFiles([]string{file(t, dir, "b.txt", 1)})
	grid := m.Grid
	m.ToggleView()
	m.ToggleExpanded()
	if m.Grid != grid || !m.Expanded {
		t.Fatal("the buttons act on several files")
	}
	m.FoldStack()
	if m.Expanded {
		t.Fatal("the stack did not fold")
	}
	m.FoldStack()
	if !m.Expanded {
		t.Fatal("the stack did not open")
	}
}

func TestSelectionAndWhatIsDragged(t *testing.T) {
	dir := t.TempDir()
	m := New(&fakeHost{})
	var ps []string
	for _, n := range []string{"a", "b", "c"} {
		ps = append(ps, file(t, dir, n+".txt", 1))
	}
	m.AddFiles(ps)
	if got := m.AllPaths(); len(got) != 3 || got[0] != ps[0] {
		t.Fatalf("%v", got)
	}
	// Nothing chosen: the item pressed.
	if got := m.PathsOf(1); len(got) != 1 || got[0] != ps[1] {
		t.Fatalf("%v", got)
	}
	m.Select(0, false)
	m.Select(2, true)
	if got := m.PathsOf(2); len(got) != 2 || got[0] != ps[0] || got[1] != ps[2] {
		t.Fatalf("%v", got)
	}
	m.Select(2, true) // Ctrl-click again: off
	if got := m.PathsOf(2); len(got) != 1 || got[0] != ps[0] {
		t.Fatalf("%v", got)
	}
	m.Select(1, false) // a plain click: only that one
	if got := m.PathsOf(1); len(got) != 1 || got[0] != ps[1] || m.Items[0].Selected {
		t.Fatalf("%v %+v", got, m.Items)
	}
}

func TestFormatBytesIsCSharps(t *testing.T) {
	for in, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1 KB", 1536: "1.5 KB", 1 << 20: "1 MB", 1572864: "1.5 MB",
		1<<30 - 1: "1024 MB", 1 << 30: "1 GB", 5368709120: "5 GB", 123456789: "117.7 MB"} {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestBadgeAndColor(t *testing.T) {
	for name, want := range map[string]struct {
		badge   string
		r, g, b uint8
	}{
		"a.pdf": {"PDF", 0xC6, 0x28, 0x28}, "b.XLSX": {"XLSX", 0x2E, 0x7D, 0x32}, "c.png": {"PNG", 0x15, 0x65, 0xC0},
		"d.mp3": {"MP3", 0x61, 0x61, 0x61}, "e.zip": {"ZIP", 0x75, 0x75, 0x75}, "noext": {"文件", 0x75, 0x75, 0x75}, "f.longext": {"LONG", 0x75, 0x75, 0x75},
	} {
		it := Item{Path: "/x/" + name}
		r, g, b := it.TypeColor()
		if it.Badge() != want.badge || r != want.r || g != want.g || b != want.b {
			t.Errorf("%s: %q %x %x %x", name, it.Badge(), r, g, b)
		}
	}
}

// The window, drawn without a screen.
type rig struct {
	t  *testing.T
	m  *Model
	h  *fakeHost
	tt *ui.Tester
}

func newRig(t *testing.T, paths ...string) *rig {
	h := &fakeHost{}
	m := New(h)
	m.AddFiles(paths)
	return &rig{t: t, m: m, h: h, tt: ui.NewTester(m.View, CompactSize, CompactSize)}
}

func (r *rig) frame() {
	r.tt.SetSize(map[bool]int{false: CompactSize, true: ExpandedSize}[r.m.Expanded], map[bool]int{false: CompactSize, true: ExpandedSize}[r.m.Expanded])
	r.tt.Frame()
}

func (r *rig) click(s string, mods ...ui.Modifiers) {
	r.t.Helper()
	var err error
	if len(mods) > 0 {
		err = r.tt.ClickWith(mods[0], s)
	} else {
		err = r.tt.Click(s)
	}
	if err != nil {
		r.t.Fatalf("click %q: %v (%q)", s, err, r.tt.Texts())
	}
}

func TestViewEmptyAndClose(t *testing.T) {
	r := newRig(t)
	// An empty shelf is a bare card, as in the old window: the hint is the
	// title of the opened one.
	if r.tt.HasText("放置你的内容项") || !r.tt.HasText("×") {
		t.Fatalf("%q", r.tt.Texts())
	}
	r.click("×")
	if r.h.closed != 1 || len(r.m.Items) != 0 || r.m.Pinned {
		t.Fatalf("closed %d", r.h.closed)
	}
}

func TestViewOneFile(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, file(t, dir, "notes.txt", 10))
	if !r.tt.HasText("notes.txt") || !r.tt.HasText("TXT") {
		t.Fatalf("%q", r.tt.Texts())
	}
	r.click("展开") // the way a user gets to the details of a single file
	r.frame()
	if !r.m.Expanded {
		t.Fatal("the control did not open the shelf")
	}
	for _, s := range []string{"收起", "10 B", "notes.txt", "列表"} {
		if !r.tt.HasText(s) {
			t.Fatalf("missing %q: %q", s, r.tt.Texts())
		}
	}
	r.click("列表")
	r.frame()
	if r.m.Grid || !r.tt.HasText("网格") {
		t.Fatalf("%v %q", r.m.Grid, r.tt.Texts())
	}
	r.click("收起")
	if r.m.Expanded {
		t.Fatal("not folded")
	}
}

func TestViewSeveralFiles(t *testing.T) {
	dir := t.TempDir()
	r := newRig(t, file(t, dir, "a.pdf", 1024), file(t, dir, "b.csv", 2048), file(t, dir, "c.zip", 1024))
	r.frame()
	for _, s := range []string{"a.pdf", "b.csv", "c.zip", "3 个文件", "4 KB", "1 KB", "2 KB"} {
		if !r.tt.HasText(s) {
			t.Fatalf("missing %q: %q", s, r.tt.Texts())
		}
	}
	if r.tt.HasText("列表") || r.tt.HasText("网格") {
		t.Fatal("the view buttons are for one file")
	}
	// Click chooses; Ctrl-click adds.
	r.click("a.pdf")
	r.click("c.zip", ui.Ctrl)
	if !r.m.Items[0].Selected || r.m.Items[1].Selected || !r.m.Items[2].Selected {
		t.Fatalf("%+v", r.m.Items)
	}
	r.click("b.csv")
	if r.m.Items[0].Selected || !r.m.Items[1].Selected || r.m.Items[2].Selected {
		t.Fatalf("%+v", r.m.Items)
	}
	// Fold to the stack, and open it again with its badge.
	r.click("折叠")
	r.frame()
	if r.m.Expanded || !r.tt.HasText("3 档 >") {
		t.Fatalf("%v %q", r.m.Expanded, r.tt.Texts())
	}
	r.click("3 档 >")
	if !r.m.Expanded {
		t.Fatal("not opened")
	}
}

func TestOwnDragIsNotDroppedBack(t *testing.T) {
	dir := t.TempDir()
	m := New(&fakeHost{})
	m.AddFiles([]string{file(t, dir, "a.txt", 1)})
	d := m.dragData(m.AllPaths())
	if !m.draggingOut {
		t.Fatal("a drag from the shelf must be marked")
	}
	if fs, err := d.Files(); err != nil || len(fs) != 1 || fs[0] != m.Items[0].Path {
		t.Fatalf("%v %v", fs, err)
	}
	m.dragOptions().Done(transfer.Result{Canceled: true})
	if m.draggingOut {
		t.Fatal("the mark stayed after the drag ended")
	}
}

func TestSourceMatchesCSharp(t *testing.T) {
	vm, err := os.ReadFile("../../../klip/ViewModels/ShelfWindowViewModel.cs")
	if err != nil {
		t.Skip("C# source not here")
	}
	view, _ := os.ReadFile("../../../klip/Views/ShelfWindow.axaml.cs")
	for _, s := range []string{`"放置你的内容项"`, `"{Items.Count} 个文件"`, `"{Items.Count} 档 >"`, `{bytes / 1024.0:0.#} KB`, `"#C62828"`, `"#2E7D32"`, `"#1565C0"`} {
		if !strings.Contains(string(vm), s) {
			t.Errorf("ShelfWindowViewModel.cs no longer contains %s", s)
		}
	}
	for _, s := range []string{"CompactSize = 220", "ExpandedSize = 420"} {
		if !strings.Contains(string(view), s) {
			t.Errorf("ShelfWindow.axaml.cs no longer contains %s", s)
		}
	}
}

func TestDroppedFilesAppearAtOnceAndGetTheirDetailsLater(t *testing.T) {
	dir := t.TempDir()
	p := file(t, dir, "big.bin", 123)
	m := New(nil)
	posted := make(chan func(), 4)
	m.Post = func(f func()) { posted <- f }
	m.AddFiles([]string{p})
	if len(m.Items) != 1 || m.Items[0].Size != 0 {
		t.Fatalf("the item is there at once, without details: %+v", m.Items)
	}
	select {
	case f := <-posted:
		f()
	case <-time.After(5 * time.Second):
		t.Fatal("the details never came")
	}
	if m.Items[0].Size != 123 {
		t.Fatalf("size = %d", m.Items[0].Size)
	}
}

func TestManyFilesAreNotAllDrawn(t *testing.T) {
	var paths []string
	for i := 0; i < 3000; i++ {
		paths = append(paths, fmt.Sprintf("/nowhere/file-%04d.txt", i))
	}
	r := newRig(t, paths...)
	r.frame()
	if !r.tt.HasText("file-0000.txt") {
		t.Fatalf("the first file is not shown: %.200q", r.tt.Texts())
	}
	if n := len(r.tt.Texts()); n > 400 {
		t.Fatalf("%d texts drawn for 3000 files: the list is not virtualized", n)
	}
}
