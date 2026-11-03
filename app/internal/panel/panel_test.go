package panel

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"klip/internal/app"
	"klip/internal/history"
	"klip/internal/model"
	"klip/internal/paths"
)

func ti(id, text string) model.Item {
	t := model.Time{Time: time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)}
	return model.Item{ID: id, Kind: model.KindText, CapturedAt: t, Text: model.Ptr(text), CopyCount: 1,
		ContentHash: model.Ptr("text:" + text)}
}

func TestMatch(t *testing.T) {
	it := ti("1", "Hello World")
	it.Note = model.Ptr("买菜清单")
	it.FilePaths = []string{`C:\Docs\Report.PDF`}
	it.ImageFilePath = model.Ptr("/x/img/abc.png")
	for q, want := range map[string]bool{
		"hello": true, "WORLD": true, "清单": true, "report.pdf": true, "ABC.png": true, "nope": false,
	} {
		if got := Match(it, q); got != want {
			t.Errorf("Match(%q) = %v, want %v", q, got, want)
		}
	}
}

// The main list keeps the order of the history: favourites are not moved
// to the top (they are in their own folder, 常用剪贴).
func TestFilterKeepsHistoryOrder(t *testing.T) {
	a, b, c, d := ti("a", "alpha"), ti("b", "beta"), ti("c", "gamma alpha"), ti("d", "delta")
	b.Pinned, d.Pinned = true, true
	ids := func(in []model.Item) string {
		s := ""
		for _, it := range in {
			s += it.ID
		}
		return s
	}
	items := []model.Item{a, b, c, d}
	if got := ids(Filter(items, "")); got != "abcd" {
		t.Fatalf("history order: %q", got)
	}
	if got := ids(Filter(items, "  alpha ")); got != "ac" {
		t.Fatalf("query is trimmed: %q", got)
	}
	if got := ids(Filter(items, "   ")); got != "abcd" {
		t.Fatalf("blank query matches all: %q", got)
	}
	if got := ids(Pinned(Filter(items, ""))); got != "bd" {
		t.Fatalf("favourites: %q", got)
	}
}

func TestRowTexts(t *testing.T) {
	if got := SingleLine("  line one\r\n\n   line   two\t"); got != "line one    line   two" {
		t.Fatalf("SingleLine = %q", got)
	}
	if got := SingleLine("   "); got != "   " {
		t.Fatalf("blank is kept: %q", got)
	}
	for in, want := range map[string]string{
		`C:\a\b.txt`: "b.txt", "/x/y/c.pdf": "c.pdf",
	} {
		if got := FileSummary([]string{in}); got != want {
			t.Errorf("FileSummary(%q) = %q", in, got)
		}
	}
	if got := FileSummary([]string{`C:\a\b.txt`, "/x/y/c.pdf", "  "}); got != "b.txt (+1)" {
		t.Fatalf("two files = %q", got)
	}
	if FileSummary(nil) != "" || FileSummary([]string{" "}) != "" {
		t.Fatal("no files")
	}
	for paths, want := range map[string]string{"/a/b.tar.gz": "GZ", `C:\x\Report.PDF`: "PDF", "/a/README": "FILE", "/a/.bashrc": "BASHRC"} {
		if got := ExtensionTag([]string{paths}); got != want {
			t.Errorf("ExtensionTag(%q) = %q, want %q", paths, got, want)
		}
	}
	if Tag(model.Item{Kind: model.KindImage}) != "IMG" || Tag(ti("1", "x")) != "" {
		t.Fatal("tags")
	}
	if got := DragText(model.Item{Kind: model.KindFileList, FilePaths: []string{"a", "b"}}); got != "2 个文件" {
		t.Fatalf("drag files = %q", got)
	}
	if DragText(model.Item{Kind: model.KindFileList}) != "文件" || DragText(model.Item{Kind: model.KindImage}) != "图片" ||
		DragText(ti("1", "  \n ")) != "文本" {
		t.Fatal("drag text")
	}
	long := ""
	for i := 0; i < 60; i++ {
		long += "字"
	}
	if got := []rune(DragText(ti("1", long))); len(got) != 43 || got[42] != '…' {
		t.Fatalf("drag text cut to %d runes", len(got))
	}
}

func TestImageInfo(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "a.png")
	// a 1x1 PNG
	data := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\xff\xff?\x00\x05\xfe\x02\xfe\xa7\x9a\xa0\xa0\x00\x00\x00\x00IEND\xaeB`\x82")
	if err := os.WriteFile(png, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ImageInfo(png); got != "IMG · 1×1 · 1 KB" {
		t.Fatalf("ImageInfo = %q", got)
	}
	bad := filepath.Join(dir, "b.png")
	_ = os.WriteFile(bad, []byte("not an image"), 0o644)
	if got := ImageInfo(bad); got != "IMG" {
		t.Fatalf("unreadable image = %q", got)
	}
	if ImageInfo(filepath.Join(dir, "gone.png")) != "" || ImageInfo("") != "" {
		t.Fatal("a file that is gone has no info")
	}
}

func TestPalettes(t *testing.T) {
	d, l := PaletteFor(true), PaletteFor(false)
	if d.Background != ui.Hex("#1E1E1E") || l.Background != ui.Hex("#F6F6F6") || d.Panel != ui.Hex("#171717") {
		t.Fatalf("backgrounds: %+v %+v", d.Background, l.Background)
	}
	// App.axaml writes #EE111111: alpha first.
	if d.PreviewBg.A != 0xEE || d.PreviewBg.R != 0x11 || d.ToastBg.A != 0xCC || d.ToastBg.R != 0x33 {
		t.Fatalf("translucent colours: %+v %+v", d.PreviewBg, d.ToastBg)
	}
	if l.ToastBg.A != 0xCC || l.ToastBg.R != 0x11 || l.Text != ui.Hex("#1A1A1A") {
		t.Fatalf("light: %+v", l)
	}
}

type fakeHost struct {
	applied  []string
	applyErr error
	hidden   int
	deleted  []string
	cleared  int
	moved    [][2]string
	prefs    int
	about    int
	quit     int
	update   string
	applyng  bool
	updates  int
	sync     app.SyncView

	previewed []string
	hid       int
	shownID   string
	over      bool
}

func (h *fakeHost) Apply(it model.Item) error {
	if h.applyErr != nil {
		return h.applyErr
	}
	h.applied = append(h.applied, model.Str(it.Text))
	return nil
}
func (h *fakeHost) Hide()            { h.hidden++ }
func (h *fakeHost) Delete(id string) { h.deleted = append(h.deleted, id) }
func (h *fakeHost) ClearAll()        { h.cleared++ }
func (h *fakeHost) Reorder(id string, before *string) {
	b := ""
	if before != nil {
		b = *before
	}
	h.moved = append(h.moved, [2]string{id, b})
}
func (h *fakeHost) Sync() app.SyncView      { return h.sync }
func (h *fakeHost) Version() string         { return "v1.0.11" }
func (h *fakeHost) UpdateAvailable() string { return h.update }
func (h *fakeHost) UpdateApplying() bool    { return h.applyng }
func (h *fakeHost) ShowUpdate()             { h.updates++ }
func (h *fakeHost) ShowPrefs()              { h.prefs++ }
func (h *fakeHost) ShowAbout(ui.Rect)       { h.about++ }
func (h *fakeHost) Quit()                   { h.quit++ }
func (h *fakeHost) ShowPreview(it model.Item, _ ui.Rect) {
	h.previewed = append(h.previewed, model.Str(it.Text))
	h.shownID = it.ID
}
func (h *fakeHost) HidePreview()            { h.hid++; h.shownID = "" }
func (h *fakeHost) Preview() (string, bool) { return h.shownID, h.over }

type rig struct {
	t  *testing.T
	h  *history.Service
	m  *Model
	tt *ui.Tester
	ho *fakeHost
}

const w, hgt = 360, 620

func newRig(t *testing.T, texts ...string) *rig {
	h := history.NewService(paths.Layout{Root: t.TempDir()})
	for i := len(texts) - 1; i >= 0; i-- {
		h.Add(ti(fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1), texts[i]))
	}
	ho := &fakeHost{}
	m := New(h, ho)
	m.Now = func() time.Time { return time.Date(2026, 5, 30, 18, 0, 0, 0, time.UTC) }
	tt := ui.NewTester(m.View, w, hgt)
	return &rig{t: t, h: h, m: m, tt: tt, ho: ho}
}

func (r *rig) texts() []string {
	var out []string
	for _, it := range r.h.Items() {
		out = append(out, model.Str(it.Text))
	}
	return out
}

func TestViewLayoutFollowsTheOldWindow(t *testing.T) {
	r := newRig(t, "first", "second")
	for _, s := range []string{"About", "常用剪贴", "first", "second", "设置", "清除", "v1.0.11"} {
		if !r.tt.HasText(s) {
			t.Fatalf("missing %q in %q", s, r.tt.Texts())
		}
	}
	if r.tt.HasText("立即同步") {
		t.Fatalf("the old bar has no sync button: %q", r.tt.Texts())
	}
	// No hint in an empty list, as in the old window.
	e := newRig(t)
	if e.tt.HasText("复制点什么，它会出现在这里") {
		t.Fatalf("empty list: %q", e.tt.Texts())
	}
}

func TestViewSearch(t *testing.T) {
	r := newRig(t, "apple pie", "banana", "apple tart")
	r.m.Reset()
	r.tt.Frame()
	r.tt.Type("apple")
	if r.tt.HasText("banana") || !r.tt.HasText("apple pie") || !r.tt.HasText("apple tart") {
		t.Fatalf("after searching: %q", r.tt.Texts())
	}
	r.tt.Key(ui.Ctrl, ui.KeyBackspace) // clears the whole query
	if r.m.query != "" || !r.tt.HasText("banana") {
		t.Fatalf("Ctrl+Backspace: query=%q texts=%q", r.m.query, r.tt.Texts())
	}
	r.tt.Key(0, ui.KeyEscape)
	if r.ho.hidden != 1 {
		t.Fatalf("escape hides: hidden=%d", r.ho.hidden)
	}
}

func TestViewClickApplies(t *testing.T) {
	r := newRig(t, "one", "two", "three")
	if err := r.tt.Click("two"); err != nil {
		t.Fatal(err)
	}
	if len(r.ho.applied) != 1 || r.ho.applied[0] != "two" || r.ho.hidden != 1 {
		t.Fatalf("applied=%v hidden=%d", r.ho.applied, r.ho.hidden)
	}
	if r.m.toast != "已写回剪贴板" {
		t.Fatalf("toast = %q", r.m.toast)
	}
}

func TestViewKeyboard(t *testing.T) {
	r := newRig(t, "one", "two", "three")
	r.tt.Frame()
	r.tt.Key(0, ui.KeyDown)
	r.tt.Key(0, ui.KeyDown)
	r.tt.Key(0, ui.KeyDown) // past the end stays on the last
	r.tt.Key(0, ui.KeyUp)
	r.tt.Key(0, ui.KeyEnter)
	if len(r.ho.applied) != 1 || r.ho.applied[0] != "two" {
		t.Fatalf("applied = %v", r.ho.applied)
	}
}

func TestPinnedStayInPlaceAndShowInFolder(t *testing.T) {
	r := newRig(t, "one", "two", "three")
	if err := r.tt.RightClick("two"); err != nil {
		t.Fatal(err)
	}
	menu := r.tt.Menu()
	for _, want := range []string{"加入收藏/取消收藏", "添加备注", "删除"} {
		found := false
		for _, m := range menu {
			if m == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("menu %q lacks %q", menu, want)
		}
	}
	if err := r.tt.ChooseMenuItem("加入收藏/取消收藏"); err != nil {
		t.Fatal(err)
	}
	if items := r.h.Items(); !items[1].Pinned || model.Str(items[1].Text) != "two" {
		t.Fatalf("not pinned: %+v", items)
	}
	r.tt.Frame()
	if !r.tt.HasText("★") {
		t.Fatalf("no star: %q", r.tt.Texts())
	}
	// Still second in the list.
	if rows := r.m.rows(); model.Str(rows[0].Text) != "one" || model.Str(rows[1].Text) != "two" {
		t.Fatalf("pinned moved: %v", rows)
	}
	// The folder holds only the favourites, and has a way back.
	if err := r.tt.Click("常用剪贴"); err != nil {
		t.Fatal(err)
	}
	r.tt.Frame()
	if r.m.sec != sectionFavorites || r.tt.HasText("one") || !r.tt.HasText("two") || !r.tt.HasText("返回") {
		t.Fatalf("folder: sec=%v texts=%q", r.m.sec, r.tt.Texts())
	}
	if err := r.tt.Click("返回"); err != nil {
		t.Fatal(err)
	}
	r.tt.Frame()
	if r.m.sec != sectionAll || !r.tt.HasText("one") {
		t.Fatalf("back: %v %q", r.m.sec, r.tt.Texts())
	}
}

func TestDeleteLocalAndWithCloud(t *testing.T) {
	r := newRig(t, "one", "two")
	if err := r.tt.RightClick("one"); err != nil {
		t.Fatal(err)
	}
	if err := r.tt.ChooseMenuItem("删除"); err != nil {
		t.Fatal(err)
	}
	if len(r.ho.deleted) != 1 || r.m.dlg != nil {
		t.Fatalf("no account: deleted=%v dlg=%v", r.ho.deleted, r.m.dlg)
	}

	// With an account the cloud copy goes too: ask first.
	r.ho.sync.Cloud = true
	r.ho.deleted = nil
	r.tt.Frame()
	if err := r.tt.RightClick("two"); err != nil {
		t.Fatal(err)
	}
	if err := r.tt.ChooseMenuItem("删除"); err != nil {
		t.Fatal(err)
	}
	r.tt.Frame()
	if len(r.ho.deleted) != 0 || !r.tt.HasText("删除记录") || !r.tt.HasText("此操作将同时删除云端备份，且不可撤销。\n\n是否继续？") && !r.tt.HasText("是否继续？") {
		t.Fatalf("dialog: deleted=%v texts=%q", r.ho.deleted, r.tt.Texts())
	}
	if err := r.tt.Click("取消"); err != nil {
		t.Fatal(err)
	}
	if len(r.ho.deleted) != 0 || r.m.dlg != nil {
		t.Fatal("cancel deleted")
	}
	if err := r.tt.RightClick("two"); err != nil {
		t.Fatal(err)
	}
	_ = r.tt.ChooseMenuItem("删除")
	r.tt.Frame()
	if err := r.tt.Click("确定"); err != nil {
		t.Fatal(err)
	}
	if len(r.ho.deleted) != 1 {
		t.Fatalf("deleted = %v", r.ho.deleted)
	}
}

func TestClearAsksFirst(t *testing.T) {
	r := newRig(t, "one")
	if err := r.tt.Click("清除"); err != nil {
		t.Fatal(err)
	}
	r.tt.Frame()
	if !r.tt.HasText("清除全部记录") || r.ho.cleared != 0 {
		t.Fatalf("dialog: %q cleared=%d", r.tt.Texts(), r.ho.cleared)
	}
	r.tt.Key(0, ui.KeyEscape)
	r.tt.Frame()
	if r.m.dlg != nil || r.ho.cleared != 0 || r.ho.hidden != 0 {
		t.Fatalf("escape closes the box only: dlg=%v cleared=%d hidden=%d", r.m.dlg, r.ho.cleared, r.ho.hidden)
	}
	_ = r.tt.Click("清除")
	r.tt.Frame()
	// the button of the box is also called 清除: the second one
	r.m.finish(r.m.dlg)
	if r.ho.cleared != 1 {
		t.Fatalf("cleared = %d", r.ho.cleared)
	}
}

func TestViewNoteEditing(t *testing.T) {
	r := newRig(t, "one", "two")
	if err := r.tt.RightClick("one"); err != nil {
		t.Fatal(err)
	}
	if err := r.tt.ChooseMenuItem("添加备注"); err != nil {
		t.Fatal(err)
	}
	r.tt.Frame()
	if !r.tt.HasText("保存") || !r.tt.HasText("请输入备注（可用于搜索）：") {
		t.Fatalf("no note box: %q", r.tt.Texts())
	}
	r.tt.Type("买菜")
	if err := r.tt.Click("保存"); err != nil {
		t.Fatal(err)
	}
	var note string
	for _, it := range r.h.Items() {
		if model.Str(it.Text) == "one" {
			note = model.Str(it.Note)
		}
	}
	if note != "买菜" {
		t.Fatalf("note = %q", note)
	}
	r.tt.Frame()
	if r.tt.HasText("保存") {
		t.Fatalf("box still open: %q", r.tt.Texts())
	}
	// A note is found by search.
	r.m.query = "买菜"
	r.tt.Frame()
	if r.tt.HasText("two") || !r.tt.HasText("one") {
		t.Fatalf("search by note: %q", r.tt.Texts())
	}
}

func TestViewNoteCancel(t *testing.T) {
	r := newRig(t, "one")
	r.h.UpdateNote(r.h.Items()[0].ID, "old")
	r.tt.Frame()
	_ = r.tt.RightClick("one")
	if err := r.tt.ChooseMenuItem("添加备注"); err != nil {
		t.Fatal(err)
	}
	r.tt.Frame()
	r.tt.Type("zz")
	if err := r.tt.Click("取消"); err != nil {
		t.Fatal(err)
	}
	if got := model.Str(r.h.Items()[0].Note); got != "old" {
		t.Fatalf("cancel changed the note: %q", got)
	}
}

func TestViewFollowsHistoryChanges(t *testing.T) {
	r := newRig(t, "one")
	redraws := 0
	r.m.Redraw = func() { redraws++ }
	r.h.Add(ti("00000000-0000-0000-0000-0000000000ff", "fresh from the clipboard"))
	if redraws == 0 {
		t.Fatal("a change did not ask for a frame")
	}
	r.tt.Frame()
	if !r.tt.HasText("fresh from the clipboard") {
		t.Fatalf("new item not shown: %q", r.tt.Texts())
	}
}

func TestViewButtons(t *testing.T) {
	r := newRig(t, "one")
	if err := r.tt.Click("About"); err != nil {
		t.Fatal(err)
	}
	if r.ho.about != 1 {
		t.Fatal("About")
	}
	if err := r.tt.Click("设置"); err != nil {
		t.Fatal(err)
	}
	if r.ho.prefs != 1 {
		t.Fatal("preferences")
	}
}

func TestResetChoosesFirstRowOfAll(t *testing.T) {
	r := newRig(t, "one", "two", "three")
	r.tt.Frame()
	r.tt.Key(0, ui.KeyDown)
	r.tt.Key(0, ui.KeyDown)
	r.m.sec = sectionFavorites
	r.m.Reset()
	r.tt.Frame()
	if r.m.sec != sectionAll {
		t.Fatal("Reset keeps the folder")
	}
	r.tt.Key(0, ui.KeyEnter)
	if len(r.ho.applied) != 1 || r.ho.applied[0] != "one" {
		t.Fatalf("applied %v after Reset, want the first row", r.ho.applied)
	}
}

func TestSearchBoxKeepsFocusAndDrivesList(t *testing.T) {
	r := newRig(t, "one", "two", "three")
	r.m.Reset()
	r.tt.Frame()
	r.tt.Key(0, ui.KeyDown)
	r.tt.Key(0, ui.KeyDown)
	r.tt.Key(0, ui.KeyUp)
	r.tt.Key(0, ui.KeyEnter)
	if len(r.ho.applied) != 1 || r.ho.applied[0] != "two" {
		t.Fatalf("arrows and Enter in the search box: applied %v", r.ho.applied)
	}
	r.m.Reset()
	r.tt.Frame()
	r.tt.Type("thr")
	if r.m.query != "thr" || len(r.m.rows()) != 1 {
		t.Fatalf("query=%q rows=%d", r.m.query, len(r.m.rows()))
	}
}

func TestKeyboardPinAndDelete(t *testing.T) {
	r := newRig(t, "one", "two", "three")
	r.m.Reset()
	r.tt.Frame()
	r.tt.Key(0, ui.KeyDown) // "two"
	r.tt.Key(ui.Ctrl, ui.KeyP)
	if it := r.h.Items()[1]; model.Str(it.Text) != "two" || !it.Pinned {
		t.Fatalf("Ctrl+P: %+v", r.h.Items())
	}
	r.tt.Frame()
	r.tt.Key(0, ui.KeyDelete)
	if len(r.ho.deleted) != 1 || r.ho.deleted[0] != r.h.Items()[1].ID {
		t.Fatalf("Delete: %v", r.ho.deleted)
	}
	// With a query the Delete key belongs to the search box.
	r.ho.deleted = nil
	r.m.query = "t"
	r.tt.Frame()
	r.tt.Key(0, ui.KeyDelete)
	if len(r.ho.deleted) != 0 {
		t.Fatalf("Delete with a query deleted %v", r.ho.deleted)
	}
}

func TestCtrlFFocusesSearch(t *testing.T) {
	r := newRig(t, "one")
	r.tt.Frame()
	r.tt.Key(ui.Ctrl, ui.KeyF)
	r.tt.Frame()
	r.tt.Type("on")
	if r.m.query != "on" {
		t.Fatalf("query = %q", r.m.query)
	}
}

func TestUpdateButton(t *testing.T) {
	r := newRig(t, "one")
	if r.tt.HasText("更新") {
		t.Fatal("a button without an update")
	}
	r.ho.update = "1.0.12"
	r.tt.Frame()
	if err := r.tt.Click("更新"); err != nil {
		t.Fatalf("%v (%q)", err, r.tt.Texts())
	}
	if r.ho.updates != 1 {
		t.Fatal("not opened")
	}
	r.ho.applyng = true
	r.tt.Frame()
	if !r.tt.HasText("更新中...") {
		t.Fatalf("while applying: %q", r.tt.Texts())
	}
}

func TestSyncBar(t *testing.T) {
	r := newRig(t, "one")
	r.ho.sync = app.SyncView{Cloud: true, Syncing: true, Progress: true, Text: "上传快照", Percent: 84}
	r.tt.Frame()
	if !r.tt.HasText("上传快照 84%") {
		t.Fatalf("progress: %q", r.tt.Texts())
	}
	r.ho.sync = app.SyncView{Cloud: true}
	r.tt.Frame()
	if r.tt.HasText("上传快照 84%") {
		t.Fatal("progress stays")
	}
}

func TestToastFades(t *testing.T) {
	r := newRig(t, "one")
	r.m.ShowToast("请先登录")
	r.tt.Frame()
	if !r.tt.HasText("请先登录") {
		t.Fatalf("toast: %q", r.tt.Texts())
	}
	now := r.m.Now()
	r.m.Now = func() time.Time { return now.Add(time.Second) }
	r.tt.Frame()
	if r.tt.HasText("请先登录") {
		t.Fatal("toast stays")
	}
}

func TestHoverPreviewDelayAndGrace(t *testing.T) {
	r := newRig(t, "one", "two")
	now := time.Date(2026, 5, 30, 18, 0, 0, 0, time.UTC)
	r.m.Now = func() time.Time { return now }
	rect := ui.Rect{X: 0, Y: 100, W: 360, H: 30}
	it := r.h.Items()[0]
	r.m.hovering(&it, rect)
	if len(r.ho.previewed) != 0 {
		t.Fatal("shown at once")
	}
	now = now.Add(300 * time.Millisecond)
	r.m.hovering(&it, rect)
	if len(r.ho.previewed) != 0 {
		t.Fatal("shown before the delay")
	}
	now = now.Add(60 * time.Millisecond)
	r.m.hovering(&it, rect)
	if len(r.ho.previewed) != 1 || r.ho.previewed[0] != "one" {
		t.Fatalf("not shown after the delay: %v", r.ho.previewed)
	}
	r.m.hovering(&it, rect)
	if len(r.ho.previewed) != 1 {
		t.Fatal("shown again while shown")
	}
	// The pointer leaves: it stays for the grace, then goes...
	r.m.hovering(nil, rect)
	hid := r.ho.hid
	now = now.Add(100 * time.Millisecond)
	r.m.hovering(nil, rect)
	if r.ho.hid != hid {
		t.Fatal("closed inside the grace")
	}
	now = now.Add(150 * time.Millisecond)
	r.m.hovering(nil, rect)
	if r.ho.hid != hid+1 {
		t.Fatal("not closed after the grace")
	}
	// ...unless it went over to the preview.
	r.ho.shownID, r.ho.over = it.ID, true
	r.m.hovering(&it, rect)
	r.m.hovering(nil, rect)
	now = now.Add(time.Second)
	r.m.hovering(nil, rect)
	if r.ho.shownID == "" {
		t.Fatal("closed with the pointer on it")
	}
}

func TestDragReorder(t *testing.T) {
	r := newRig(t, "one", "two", "three")
	r.tt.Frame()
	from, ok1 := r.tt.Find("three")
	to, ok2 := r.tt.Find("one")
	if !ok1 || !ok2 {
		t.Fatalf("rows not found: %q", r.tt.Texts())
	}
	r.tt.Press(from.X+from.W/2, from.Y+from.H/2)
	r.tt.Move(from.X+from.W/2, from.Y+from.H/2-20)
	r.tt.Move(to.X+to.W/2, to.Y+to.H/2)
	r.tt.Release(to.X+to.W/2, to.Y+to.H/2)
	r.tt.Frame()
	if len(r.ho.moved) != 1 || r.ho.moved[0][0] != "00000000-0000-0000-0000-000000000003" || r.ho.moved[0][1] != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("moved = %v", r.ho.moved)
	}
	if len(r.ho.applied) != 0 {
		t.Fatal("a drag is no click")
	}
}

func TestThumbnailsAreSmallAndMadeInTheBackground(t *testing.T) {
	big := image.NewRGBA(image.Rect(0, 0, 1600, 900))
	for i := range big.Pix {
		big.Pix[i] = 200
	}
	path := filepath.Join(t.TempDir(), "big.png")
	var b bytes.Buffer
	_ = png.Encode(&b, big)
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	var c thumbCache
	if c.get(path, func() { done <- struct{}{} }) != nil {
		t.Fatal("the first look must not wait for the picture")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the thumbnail was never made")
	}
	bm := c.get(path, nil)
	if bm == nil {
		t.Fatal("no thumbnail")
	}
	if w, h := bm.Size(); w > thumbSize || h > thumbSize || w < 10 {
		t.Fatalf("thumbnail is %dx%d, want it cut down to fit %d", w, h, thumbSize)
	}
}

func TestSmallBitmapDoesNotReadHugeFiles(t *testing.T) {
	dir := t.TempDir()
	// A file of a format nobody can tell the size of beforehand: too big to be
	// read just to find out.
	big := filepath.Join(dir, "big.dat")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Truncate(thumbMaxUnknownBytes + 1)
	f.Close()
	if smallBitmap(big) != nil {
		t.Fatal("a file over the limit was turned into a thumbnail")
	}
	// A bitmap whose header says 20000 x 20000: refused from the header alone,
	// the file itself is a few bytes.
	hdr := make([]byte, 54)
	copy(hdr, "BM")
	le := func(b []byte, v uint32) { b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24) }
	le(hdr[2:], 54)
	le(hdr[10:], 54)
	le(hdr[14:], 40)
	le(hdr[18:], 20000)
	le(hdr[22:], 20000)
	hdr[26], hdr[28] = 1, 32
	huge := filepath.Join(dir, "huge.bmp")
	_ = os.WriteFile(huge, hdr, 0o644)
	if smallBitmap(huge) != nil {
		t.Fatal("a picture over the pixel limit was decoded")
	}
}

func TestRowTextOfAHugeTextIsCheap(t *testing.T) {
	big := strings.Repeat("word ", 5_000_000) // 25 MB
	start := time.Now()
	got := RowText(model.Item{Kind: model.KindText, Text: model.Ptr(big)})
	if !strings.HasPrefix(got, "word word") || len([]rune(got)) > rowTextRunes {
		t.Fatalf("%.40q (%d runes)", got, len([]rune(got)))
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("a long text took %v for its row", time.Since(start))
	}
	if headRunes("字字字", 2) != "字字" || headRunes("ab", 5) != "ab" || headRunes("", 0) != "" {
		t.Fatal("headRunes")
	}
}

// The window comes back as it was left: the query and the folder stay, the
// first row is chosen (PrepareForOpen).
func TestOpenedKeepsQueryAndFolder(t *testing.T) {
	r := newRig(t, "apple pie", "banana", "apple tart")
	r.m.Reset()
	r.tt.Frame()
	r.tt.Type("apple")
	r.tt.Key(0, ui.KeyDown)
	if r.m.chosen != 1 {
		t.Fatalf("chosen = %d", r.m.chosen)
	}
	r.m.Opened()
	r.tt.Frame()
	if r.m.query != "apple" || r.m.chosen != 0 || r.tt.HasText("banana") {
		t.Fatalf("query=%q chosen=%d texts=%q", r.m.query, r.m.chosen, r.tt.Texts())
	}
	r.m.sec = sectionFavorites
	r.m.dlg = &dialog{kind: "confirm", title: "x"}
	r.m.Opened()
	if r.m.sec != sectionFavorites || r.m.dlg != nil {
		t.Fatalf("sec=%v dlg=%v", r.m.sec, r.m.dlg)
	}
}

func TestOpenedChoosesTheNewestItemCopiedWhileHidden(t *testing.T) {
	r := newRig(t, "old one", "older one")
	r.m.Reset()
	r.tt.Frame()
	// Hidden, the program goes on collecting; the rows on show are the old ones.
	r.h.Add(ti("00000000-0000-0000-0000-0000000000ff", "copied while hidden"))
	r.m.Opened()
	rows := r.m.rows()
	if r.m.chosen != 0 || len(rows) == 0 || model.Str(rows[0].Text) != "copied while hidden" {
		t.Fatalf("chosen=%d rows=%d", r.m.chosen, len(rows))
	}
	r.tt.Frame()
	if rows := r.m.rows(); r.m.chosen != 0 || model.Str(rows[r.m.chosen].Text) != "copied while hidden" {
		t.Fatalf("after the frame: chosen=%d row=%q", r.m.chosen, model.Str(rows[r.m.chosen].Text))
	}
}

// Dropped below the last row, a row goes to the end.
func TestDragToTheEnd(t *testing.T) {
	r := newRig(t, "one", "two", "three")
	r.tt.Frame()
	from, ok1 := r.tt.Find("one")
	last, ok2 := r.tt.Find("three")
	if !ok1 || !ok2 {
		t.Fatalf("rows not found: %q", r.tt.Texts())
	}
	x, y := last.X+last.W/2, last.Y+last.H-8 // the strip at the foot of the last row
	r.tt.Press(from.X+from.W/2, from.Y+from.H/2)
	r.tt.Move(from.X+from.W/2, from.Y+from.H/2+20)
	r.tt.Frame()
	r.tt.Move(x, y)
	r.tt.Frame()
	r.tt.Release(x, y)
	r.tt.Frame()
	if len(r.ho.moved) != 1 || r.ho.moved[0][0] != "00000000-0000-0000-0000-000000000001" || r.ho.moved[0][1] != "" {
		t.Fatalf("moved = %v", r.ho.moved)
	}
}

func TestImageInfoIsReadInTheBackground(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.png")
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 3, 2)))
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 1)
	var c infoCache
	if got := c.get(path, func() { done <- struct{}{} }); got != "" {
		t.Fatalf("the first look must not read the file: %q", got)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("never read")
	}
	if got := c.get(path, nil); got != "IMG · 3×2 · 1 KB" {
		t.Fatalf("info = %q", got)
	}
}

// While the note dialog shows, the keys stay in it: no Tab and Enter reaches
// a row behind it (which would apply the item and hide the window); Escape
// closes it.
func TestNoteDialogKeepsTheKeys(t *testing.T) {
	open := func() *rig {
		r := newRig(t, "one", "two", "three")
		if err := r.tt.RightClick("two"); err != nil {
			t.Fatal(err)
		}
		if err := r.tt.ChooseMenuItem("添加备注"); err != nil {
			t.Fatal(err)
		}
		r.tt.Frame()
		r.tt.Frame()
		if !r.tt.HasText("添加备注") {
			t.Fatalf("no dialog: %q", r.tt.Texts())
		}
		return r
	}
	for tabs := range 12 {
		r := open()
		for range tabs {
			r.tt.Key(0, ui.KeyTab)
			r.tt.Frame()
		}
		r.tt.Key(0, ui.KeyEnter)
		r.tt.Frame()
		if len(r.ho.applied) != 0 || r.ho.hidden != 0 {
			t.Fatalf("after %d tabs Enter reached a row behind the dialog: applied %v", tabs, r.ho.applied)
		}
	}
	r := open()
	r.tt.Key(0, ui.KeyEscape)
	r.tt.Frame()
	if r.tt.HasText("请输入备注（可用于搜索）：") {
		t.Fatal("Escape did not close the dialog")
	}
}

// Escape over a row with its preview open hides the window; the rest of that
// frame, with the pointer still on the row, does not open the preview again.
func TestNoPreviewAfterTheWindowIsHidden(t *testing.T) {
	r := newRig(t, "one", "two")
	now := time.Date(2026, 5, 30, 18, 0, 0, 0, time.UTC)
	r.m.Now = func() time.Time { return now }
	rect := ui.Rect{X: 0, Y: 100, W: 360, H: 30}
	it := r.h.Items()[0]
	r.m.hovering(&it, rect)
	now = now.Add(time.Second)
	r.m.hovering(&it, rect)
	if len(r.ho.previewed) != 1 {
		t.Fatalf("not shown: %v", r.ho.previewed)
	}
	r.m.Closed() // what hiding the window does
	r.ho.shownID = ""
	for range 3 {
		now = now.Add(time.Second)
		r.m.hovering(&it, rect)
	}
	if len(r.ho.previewed) != 1 {
		t.Fatalf("shown again while hidden: %v", r.ho.previewed)
	}
	r.m.Opened()
	r.m.hovering(&it, rect)
	now = now.Add(time.Second)
	r.m.hovering(&it, rect)
	if len(r.ho.previewed) != 2 {
		t.Fatalf("not shown once open again: %v", r.ho.previewed)
	}
}

// Going into a folder chooses its first row, and shows it: the list does not
// keep the place it was scrolled to in the other folder.
func TestEnteringAFolderShowsItsFirstRow(t *testing.T) {
	var texts []string
	for i := range 120 {
		texts = append(texts, fmt.Sprintf("entry %03d", i))
	}
	r := newRig(t, texts...)
	for i, it := range r.h.Items() {
		if i%2 == 0 {
			r.h.TogglePinned(it.ID)
		}
	}
	r.tt.Frame()
	for range 30 {
		r.tt.Scroll(w/2, hgt/2, 0, 400)
		r.tt.Frame()
	}
	if err := r.tt.Click("常用剪贴"); err != nil {
		t.Fatal(err)
	}
	r.tt.Frame()
	r.tt.Frame()
	first := RowText(r.m.rows()[0])
	if b, ok := r.tt.Find(first); !ok || b.H <= 0 || b.Y < 0 || b.Y > hgt {
		t.Fatalf("the first row %q is not in view: %v %+v", first, ok, b)
	}
}

func TestApplyFailureKeepsPanelOpen(t *testing.T) {
	r := newRig(t, "item")
	r.ho.applyErr = errors.New("clipboard busy")
	r.m.run(action{kind: "apply", item: r.h.Items()[0]})
	if r.ho.hidden != 0 || len(r.ho.applied) != 0 || r.m.toast != "写入剪贴板失败，请重试" {
		t.Fatal("failure reported as success")
	}
}
