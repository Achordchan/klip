package history_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"klip/internal/history"
	"klip/internal/model"
	"klip/internal/paths"
	"klip/internal/syncer"
)

// The history is what the syncer synchronizes.
var _ syncer.History = (*history.Service)(nil)

type rig struct {
	t      *testing.T
	l      paths.Layout
	svc    *history.Service
	events atomic.Int32
	n      int
	now    time.Time
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, l: paths.Layout{Root: t.TempDir()}, now: time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)}
	r.svc = history.NewService(r.l)
	r.svc.Now = func() time.Time { return r.now }
	r.svc.OnChanged(func() { r.events.Add(1) })
	return r
}

func (r *rig) text(text, hash string) model.Item {
	r.n++
	r.now = r.now.Add(time.Second)
	it := model.Item{ID: guid(r.n), Kind: model.KindText, CapturedAt: model.Time{Time: r.now}, ApproxBytes: int64(len(text)) * 2, Text: &text, CopyCount: 1}
	if hash != "" {
		it.ContentHash = &hash
	}
	return it
}

func (r *rig) image(hash string) model.Item {
	r.n++
	r.now = r.now.Add(time.Second)
	os.MkdirAll(r.l.Images(), 0o755)
	p := filepath.Join(r.l.Images(), guid(r.n)+".png")
	os.WriteFile(p, []byte("png"), 0o644)
	h := hash
	return model.Item{ID: guid(r.n), Kind: model.KindImage, CapturedAt: model.Time{Time: r.now}, ApproxBytes: 3, ImageFilePath: &p, CopyCount: 1, ContentHash: &h}
}

func (r *rig) files(hash string) model.Item {
	r.n++
	r.now = r.now.Add(time.Second)
	id := guid(r.n)
	dir := filepath.Join(r.l.Files(), strings.ReplaceAll(id, "-", ""))
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "0000_a.txt"), []byte("abc"), 0o644)
	h := hash
	return model.Item{ID: id, Kind: model.KindFileList, CapturedAt: model.Time{Time: r.now}, ApproxBytes: 3, FilePaths: []string{"/src/a.txt"}, CopyCount: 1, ContentHash: &h}
}

func guid(n int) string {
	return "00000000-0000-0000-0000-0000000000" + string(rune('0'+n/10%10)) + string(rune('0'+n%10))
}

func ids(items []model.Item) string {
	var out []string
	for _, it := range items {
		out = append(out, it.ID[len(it.ID)-2:])
	}
	return strings.Join(out, ",")
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestAddPutsNewestOnTop(t *testing.T) {
	r := newRig(t)
	r.svc.Add(r.text("a", "h-a"))
	r.svc.Add(r.text("b", "h-b"))
	r.svc.Add(r.text("c", ""))
	if got := ids(r.svc.Items()); got != "03,02,01" {
		t.Errorf("order %s", got)
	}
	if r.events.Load() != 3 {
		t.Errorf("events %d", r.events.Load())
	}
}

func TestDuplicateIsMovedUpAndCounted(t *testing.T) {
	r := newRig(t)
	first := r.text("one", "H1")
	first.Note, first.Pinned = model.Ptr("keep me"), true
	r.svc.Add(first)
	r.svc.Add(r.text("two", "H2"))

	again := r.text("one, newer", "h1") // the hash is compared without regard to case
	r.svc.Add(again)
	items := r.svc.Items()
	if len(items) != 2 || ids(items) != "01,02" {
		t.Fatalf("items %s", ids(items))
	}
	got := items[0]
	if got.ID != first.ID || got.CopyCount != 2 || !got.Pinned || model.Str(got.Note) != "keep me" ||
		model.Str(got.Text) != "one, newer" || !got.CapturedAt.Equal(again.CapturedAt.Time) ||
		got.FirstCapturedAt == nil || !got.FirstCapturedAt.Equal(first.CapturedAt.Time) || model.Str(got.ContentHash) != "H1" {
		t.Errorf("merged: %+v", got)
	}

	// Another kind with the same hash is another item; so is a blank hash.
	img := r.image("H1")
	r.svc.Add(img)
	r.svc.Add(r.text("x", ""))
	r.svc.Add(r.text("x", ""))
	if r.svc.Len() != 5 {
		t.Errorf("%d items", r.svc.Len())
	}

	// With merging off, a copy is a new item.
	lim := r.svc.Limits()
	lim.MergeDuplicates = false
	r.svc.SetLimits(lim)
	r.svc.Add(r.text("one", "h1"))
	if r.svc.Len() != 6 {
		t.Errorf("%d items", r.svc.Len())
	}
}

func TestDuplicateImageAndFileCaptureDeleteTheirExtraFiles(t *testing.T) {
	r := newRig(t)
	oldImg := r.image("IMG")
	r.svc.Add(oldImg)
	newImg := r.image("img")
	r.svc.Add(newImg)
	if !exists(model.Str(oldImg.ImageFilePath)) || exists(model.Str(newImg.ImageFilePath)) {
		t.Error("the new capture's file goes, the old item keeps its own")
	}
	if got := r.svc.Items()[0]; model.Str(got.ImageFilePath) != model.Str(oldImg.ImageFilePath) || got.CopyCount != 2 {
		t.Errorf("%+v", got)
	}

	oldFiles := r.files("F")
	r.svc.Add(oldFiles)
	newFiles := r.files("f")
	r.svc.Add(newFiles)
	if !exists(filepath.Join(r.l.Files(), oldFiles.IDN())) || exists(filepath.Join(r.l.Files(), newFiles.IDN())) {
		t.Error("the new capture's cached files go, the old item keeps its own")
	}
}

func TestLimits(t *testing.T) {
	r := newRig(t)
	n, b := 3, int64(1<<30)
	r.svc.SetLimits(history.Limits{MaxItems: &n, MaxBytes: &b, MergeDuplicates: true, ExcludePinnedFromLimits: true})
	pinned := r.text("pinned", "p")
	pinned.Pinned = true
	r.svc.Add(pinned) // the oldest
	for i := 0; i < 5; i++ {
		r.svc.Add(r.text(strings.Repeat("x", i+1), ""))
	}
	// Three that are not pinned, and the pinned one beside them.
	items := r.svc.Items()
	if len(items) != 4 || !items[3].Pinned {
		t.Fatalf("items %s", ids(items))
	}
	if ids(items) != "06,05,04,01" {
		t.Errorf("kept %s", ids(items))
	}

	// Counted, pinned items fill the room but are never removed.
	r2 := newRig(t)
	n2 := 2
	r2.svc.SetLimits(history.Limits{MaxItems: &n2, MergeDuplicates: true, ExcludePinnedFromLimits: false})
	for i := 0; i < 3; i++ {
		it := r2.text("p", "")
		it.Pinned = true
		r2.svc.Add(it)
	}
	if r2.svc.Len() != 3 {
		t.Errorf("pinned items are never removed: %d", r2.svc.Len())
	}
	r2.svc.Add(r2.text("n", ""))
	if got := ids(r2.svc.Items()); got != "03,02,01" {
		t.Errorf("the unpinned one is the one that goes: %s", got)
	}

	// The size limit counts bytes; and the files of what goes are deleted.
	r3 := newRig(t)
	mb := int64(7)
	r3.svc.SetLimits(history.Limits{MaxBytes: &mb, MergeDuplicates: true, ExcludePinnedFromLimits: true})
	img, fl := r3.image("a"), r3.files("b")
	img.ApproxBytes, fl.ApproxBytes = 4, 4
	r3.svc.Add(fl)
	r3.svc.Add(img)
	if r3.svc.Len() != 1 || exists(filepath.Join(r3.l.Files(), fl.IDN())) || !exists(model.Str(img.ImageFilePath)) {
		t.Errorf("over the size: %d items", r3.svc.Len())
	}
	r3.svc.Add(r3.text("zz", "")) // 4 bytes more: the image goes
	if exists(model.Str(img.ImageFilePath)) {
		t.Error("the image of the item that went is deleted")
	}
}

func TestDefaultsMatchCSharp(t *testing.T) {
	l := history.DefaultLimits()
	if *l.MaxItems != 200 || *l.MaxBytes != 300*1024*1024 || !l.MergeDuplicates || !l.ExcludePinnedFromLimits {
		t.Errorf("%+v", l)
	}
	b, err := os.ReadFile("../../../klip/Services/ClipboardHistoryService.cs")
	if err != nil {
		t.Skipf("C# source not available: %v", err)
	}
	for _, want := range []string{`MaxItems { get; set; } = 200;`, `MaxBytes { get; set; } = 300L * 1024 * 1024;`,
		`MergeDuplicates { get; set; } = true;`, `ExcludePinnedFromLimits { get; set; } = true;`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("C# changed: %s", want)
		}
	}
	// The one place that removes by the limits only removes what is not pinned.
	if !regexp.MustCompile(`Items\.LastOrDefault\(x => !x\.Pinned\)`).Match(b) {
		t.Error("C# EnforceLimits changed")
	}
}

func TestApplyLimits(t *testing.T) {
	r := newRig(t)
	st := model.DefaultSettings()
	st.MaxItems, st.MaxMegabytes, st.MergeDuplicates, st.ExcludePinnedFromLimits = 5, 2, false, false
	r.svc.ApplyLimits(st)
	l := r.svc.Limits()
	if *l.MaxItems != 5 || *l.MaxBytes != 2*1024*1024 || l.MergeDuplicates || l.ExcludePinnedFromLimits {
		t.Errorf("%+v", l)
	}
}

func TestNoteTouchPinMoveRemove(t *testing.T) {
	r := newRig(t)
	a, b, c := r.text("a", ""), r.text("b", ""), r.text("c", "")
	img := r.image("i")
	for _, it := range []model.Item{a, b, c, img} {
		r.svc.Add(it)
	}
	// items now: img(04), c(03), b(02), a(01)

	r.svc.UpdateNote(b.ID, "hello ")
	r.svc.UpdateNote(c.ID, "note")
	r.svc.UpdateNote(c.ID, "  ") // blank clears it
	items := r.svc.Items()
	if model.Str(items[2].Note) != "hello " || items[1].Note != nil {
		t.Errorf("notes: %+v %+v", items[2].Note, items[1].Note)
	}

	r.now = r.now.Add(time.Hour)
	r.svc.Touch(a.ID)
	got := r.svc.Items()[0]
	if got.ID != a.ID || got.CopyCount != 2 || !got.CapturedAt.Equal(r.now) || got.FirstCapturedAt == nil || !got.FirstCapturedAt.Equal(a.CapturedAt.Time) {
		t.Errorf("touched: %+v", got)
	}

	r.svc.TogglePinned(b.ID) // b is last now: a, img, c, b
	if !r.svc.Items()[3].Pinned {
		t.Error("pinned")
	}
	r.svc.TogglePinned(b.ID)
	if r.svc.Items()[3].Pinned {
		t.Error("unpinned")
	}

	// a(01), img(04), c(03), b(02)
	r.svc.MoveBefore(b.ID, &a.ID)
	if got := ids(r.svc.Items()); got != "02,01,04,03" {
		t.Errorf("moved before: %s", got)
	}
	r.svc.MoveBefore(b.ID, nil)
	if got := ids(r.svc.Items()); got != "01,04,03,02" {
		t.Errorf("moved to the end: %s", got)
	}
	missing := "no-such-id"
	r.svc.MoveBefore(a.ID, &missing)
	if got := ids(r.svc.Items()); got != "04,03,02,01" {
		t.Errorf("before an unknown id: %s", got)
	}
	r.svc.MoveBefore(a.ID, &a.ID) // dropped on itself: nothing moves
	if got := ids(r.svc.Items()); got != "04,03,02,01" {
		t.Errorf("before itself: %s", got)
	}

	before := r.events.Load()
	r.svc.Remove("nope")
	r.svc.UpdateNote("nope", "x")
	r.svc.Touch("nope")
	r.svc.TogglePinned("nope")
	if r.events.Load() != before {
		t.Error("nothing changed, nothing is announced")
	}

	r.svc.Remove(img.ID)
	if exists(model.Str(img.ImageFilePath)) || r.svc.Len() != 3 {
		t.Error("remove deletes the image")
	}
}

func TestReplaceAllKeepsReferencedFilesOnly(t *testing.T) {
	r := newRig(t)
	keepImg, dropImg := r.image("k"), r.image("d")
	keepFiles, dropFiles := r.files("kf"), r.files("df")
	for _, it := range []model.Item{keepImg, dropImg, keepFiles, dropFiles} {
		r.svc.Add(it)
	}

	// The new list names the image with other letter case, as Windows may.
	upper := strings.ToUpper(model.Str(keepImg.ImageFilePath))
	incoming := keepImg
	incoming.ImageFilePath = &upper
	fresh := r.text("fresh", "")
	if err := r.svc.ReplaceAll(context.Background(), []model.Item{fresh, incoming, keepFiles}); err != nil {
		t.Fatal(err)
	}
	if got := ids(r.svc.Items()); got != "05,01,03" {
		t.Errorf("list %s", got)
	}
	if !exists(model.Str(keepImg.ImageFilePath)) || exists(model.Str(dropImg.ImageFilePath)) {
		t.Error("images: the one still in the list stays, the other goes")
	}
	if !exists(filepath.Join(r.l.Files(), keepFiles.IDN())) || exists(filepath.Join(r.l.Files(), dropFiles.IDN())) {
		t.Error("cached files: those of ids still in the list stay")
	}

	// And it is trimmed to the limits.
	n := 1
	r.svc.SetLimits(history.Limits{MaxItems: &n, MergeDuplicates: true, ExcludePinnedFromLimits: true})
	r.svc.ReplaceAll(context.Background(), []model.Item{r.text("a", ""), r.text("b", "")})
	if r.svc.Len() != 1 {
		t.Errorf("%d items", r.svc.Len())
	}
}

func TestClearAll(t *testing.T) {
	r := newRig(t)
	img, fl := r.image("i"), r.files("f")
	r.svc.Add(img)
	r.svc.Add(fl)
	r.svc.ClearAll()
	if r.svc.Len() != 0 || exists(model.Str(img.ImageFilePath)) || exists(filepath.Join(r.l.Files(), fl.IDN())) {
		t.Error("everything goes")
	}
	before := r.events.Load()
	r.svc.ClearAll()
	if r.events.Load() != before {
		t.Error("clearing an empty list is no change")
	}
}

func TestPersister(t *testing.T) {
	r := newRig(t)
	p := history.NewPersister(r.svc, r.l)
	p.Delay = 30 * time.Millisecond

	// A burst of changes is saved once, after the delay, with all of them.
	for i := 0; i < 5; i++ {
		r.svc.Add(r.text(strings.Repeat("x", i+1), ""))
	}
	if exists(r.l.History()) {
		t.Error("saved before the delay")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !exists(r.l.History()) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	loaded, err := history.Load(r.l)
	if err != nil || len(loaded) != 5 {
		t.Fatalf("saved %d items: %v", len(loaded), err)
	}

	// A change after that is saved too; Close saves what is pending.
	r.svc.Add(r.text("late", ""))
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, _ = history.Load(r.l)
	if len(loaded) != 6 || model.Str(loaded[0].Text) != "late" {
		t.Errorf("after Close: %d items", len(loaded))
	}

	// Load puts the file back in a new service; what lost its content is dropped.
	img := r.image("i")
	r.svc.Add(img)
	p2 := history.NewPersister(r.svc, r.l)
	p2.Flush(context.Background())
	os.Remove(model.Str(img.ImageFilePath))

	svc2 := history.NewService(r.l)
	p3 := history.NewPersister(svc2, r.l)
	p3.Delay = time.Hour
	if err := p3.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if svc2.Len() != 6 {
		t.Errorf("loaded %d items, the image item should be dropped", svc2.Len())
	}
}

func TestSharedImageSurvivesRemovalOfOneItem(t *testing.T) {
	r := newRig(t)
	a := r.image("img:A")
	b := r.image("img:B")
	b.ImageFilePath = a.ImageFilePath // two items, one picture (as after a sync)
	if err := r.svc.ReplaceAll(context.Background(), []model.Item{a, b}); err != nil {
		t.Fatal(err)
	}
	r.svc.Remove(a.ID)
	if !exists(model.Str(b.ImageFilePath)) {
		t.Fatal("the picture the other item still uses was deleted")
	}
	r.svc.Remove(b.ID)
	if exists(model.Str(b.ImageFilePath)) {
		t.Fatal("the last item gone, the picture goes")
	}
}

func TestFailedSaveIsTriedAgain(t *testing.T) {
	r := newRig(t)
	// Block the destination with a directory: Save intentionally repairs
	// directory permissions, so chmod is no longer a reliable failure seam.
	if err := os.MkdirAll(r.l.History(), 0o700); err != nil {
		t.Fatal(err)
	}
	p := history.NewPersister(r.svc, r.l)
	p.Delay = 20 * time.Millisecond
	defer p.Close()
	r.svc.Add(r.text("kept", ""))
	time.Sleep(200 * time.Millisecond)
	if st, err := os.Stat(r.l.History()); err != nil || !st.IsDir() {
		t.Fatal("blocked save unexpectedly succeeded")
	}
	if err := os.Remove(r.l.History()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !exists(r.l.History()) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !exists(r.l.History()) {
		t.Fatal("the failed save was never tried again")
	}
}

func TestReplaceAllCancelledBeforeChangesNothing(t *testing.T) {
	r := newRig(t)
	r.svc.Add(r.text("keep", ""))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.svc.ReplaceAll(ctx, nil); err == nil {
		t.Fatal("a cancelled replacement says so")
	}
	if r.svc.Len() != 1 {
		t.Fatal("nothing may change when it was cancelled before")
	}
}
