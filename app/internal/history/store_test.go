package history

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"klip/internal/model"
	"klip/internal/paths"
)

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// fixture builds a data folder with the content the sample history refers
// to, and the history.json that the C# client wrote.
func fixture(t *testing.T) (paths.Layout, []byte) {
	t.Helper()
	l := paths.Layout{Root: t.TempDir()}
	os.MkdirAll(l.Images(), 0o755)
	img := filepath.Join(l.Images(), "1780000000000_aaaa.png")
	os.WriteFile(img, []byte("12345"), 0o644)
	src := filepath.Join(l.Root, "report.txt")
	os.WriteFile(src, []byte("1234567"), 0o644)

	tmpl, err := os.ReadFile("../../testdata/history_csharp.json.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	doc := strings.NewReplacer("{{IMAGE}}", jsonString(img), "{{SRC}}", jsonString(src)).Replace(string(tmpl))
	os.WriteFile(l.History(), []byte(doc), 0o644)
	return l, []byte(doc)
}

func TestCSharpFileRoundTrip(t *testing.T) {
	l, orig := fixture(t)
	items, err := Load(l)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("%d items", len(items))
	}

	a := items[0]
	if model.Str(a.Text) != "你好, Klip" || model.Str(a.Note) != "备注 <b>" || !a.Pinned || a.CopyCount != 3 ||
		model.Str(a.SourceAppPath) != `C:\Windows\notepad.exe` || a.FilePaths != nil {
		t.Errorf("text item: %+v", a)
	}
	if _, off := a.CapturedAt.Zone(); off != 8*3600 {
		t.Errorf("the +08:00 offset is lost: %v", a.CapturedAt)
	}
	if items[1].Kind != model.KindImage || items[1].FirstCapturedAt != nil {
		t.Errorf("image item: %+v", items[1])
	}
	if items[2].Kind != model.KindFileList || len(items[2].FilePaths) != 1 || items[2].CopyCount != 2 {
		t.Errorf("file item: %+v", items[2])
	}

	if err := Save(l, items); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(l.History())

	// Same meaning: every value of every item, nulls included.
	var was, now []map[string]any
	json.Unmarshal(orig, &was)
	json.Unmarshal(saved, &now)
	if !reflect.DeepEqual(was, now) {
		t.Errorf("round trip changed the file\n was: %s\n now: %s", orig, saved)
	}

	// Same shape as System.Text.Json: compact, the record's order.
	if bytes.ContainsAny(saved, "\n ") && !strings.Contains(string(saved), "你好, Klip") {
		t.Errorf("not compact: %s", saved)
	}
	dec := json.NewDecoder(bytes.NewReader(saved))
	dec.Token() // [
	dec.Token() // {
	var keys []string
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		dec.Decode(&skip)
	}
	want := "Id Kind CapturedAt ApproxBytes Text ImageFilePath FilePaths Pinned CopyCount FirstCapturedAt Note ContentHash SourceAppName SourceAppPath"
	if strings.Join(keys, " ") != want {
		t.Errorf("key order: %v", keys)
	}
}

func TestNormalizeDropsWhatLostItsContent(t *testing.T) {
	l, _ := fixture(t)
	items, _ := Load(l)

	// An image whose file is gone, an empty text, a file list with neither a
	// source nor a cache: gone from the list at the next start.
	os.Remove(model.Str(items[1].ImageFilePath))
	os.Remove(items[2].FilePaths[0])
	items[0].Text = model.Ptr("   ")
	for _, it := range items {
		if _, ok := Normalize(l, it); ok {
			t.Errorf("kept %v", it.Kind)
		}
	}
}

func TestNormalizeFixesSizes(t *testing.T) {
	l, _ := fixture(t)
	items, _ := Load(l)
	items[0].Text = model.Ptr("a😀") // 3 UTF-16 units
	n, _ := Normalize(l, items[0])
	if n.ApproxBytes != 6 {
		t.Errorf("text size %d, C# counts UTF-16 units times two", n.ApproxBytes)
	}

	// A file list is as big as its cache when it has one.
	cache := filepath.Join(l.Files(), items[2].IDN())
	os.MkdirAll(cache, 0o755)
	os.WriteFile(filepath.Join(cache, "0000_report.txt"), []byte("12"), 0o644)
	n, _ = Normalize(l, items[2])
	if n.ApproxBytes != 2 {
		t.Errorf("cache size %d", n.ApproxBytes)
	}
}

func TestMissingAndBrokenFile(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	if items, err := Load(l); err != nil || len(items) != 0 {
		t.Errorf("missing file: %v %v", items, err)
	}
	os.WriteFile(l.History(), []byte(`{not json`), 0o644)
	if _, err := Load(l); err == nil {
		t.Error("broken file should fail, and not be overwritten by an empty history")
	}
}

func TestEmptyHistoryIsAnEmptyList(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	Save(l, nil)
	b, _ := os.ReadFile(l.History())
	if string(b) != "[]" {
		t.Errorf("%q", b)
	}
}

// The history holds what the user copied: the file and its folder are private
// to this user, also on a fresh install where settings.json was never saved.
func TestHistoryIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	l := paths.Layout{Root: filepath.Join(t.TempDir(), "Klip")}
	if err := Save(l, []model.Item{}); err != nil {
		t.Fatal(err)
	}
	checkMode := func(path string, want os.FileMode) {
		t.Helper()
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s: mode %04o, want %04o", path, got, want)
		}
	}
	checkMode(l.Root, 0o700)
	checkMode(l.History(), 0o600)

	// What an older version left behind is tightened when it is read.
	if err := os.Chmod(l.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.History(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(l); err != nil {
		t.Fatal(err)
	}
	checkMode(l.Root, 0o700)
	checkMode(l.History(), 0o600)
}

// A file list whose files are all empty is still content.
func TestEmptyFileListSurvivesSaveAndLoad(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	id := "16fd2706-8baf-433b-82eb-8c7fada847da"
	dir := filepath.Join(l.Files(), strings.ReplaceAll(id, "-", ""))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "0000_empty.txt")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	items := []model.Item{{
		ID: id, Kind: model.KindFileList, CapturedAt: model.Time{Time: time.Now()},
		FilePaths: []string{empty}, CopyCount: 1,
	}}
	if err := Save(l, items); err != nil {
		t.Fatal(err)
	}
	got, err := Load(l)
	if err != nil || len(got) != 1 || got[0].ID != id {
		t.Errorf("empty file list: %v %+v", err, got)
	}
}

// The size comes from the cache the item references, not the old canonical
// folder.
func TestNormalizeUsesTheActiveCacheSize(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	id := "16fd2706-8baf-433b-82eb-8c7fada847da"
	idn := strings.ReplaceAll(id, "-", "")
	canonical := filepath.Join(l.Files(), idn)
	active := filepath.Join(l.Files(), idn+"-new")
	for dir, size := range map[string]int{canonical: 100, active: 5} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "0000_file.txt"), bytes.Repeat([]byte("x"), size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	it := model.Item{
		ID: id, Kind: model.KindFileList, CapturedAt: model.Time{Time: time.Now()},
		FilePaths: []string{filepath.Join(active, "0000_file.txt")}, CopyCount: 1,
	}
	got, ok := Normalize(l, it)
	if !ok || got.ApproxBytes != 5 {
		t.Errorf("size %d (ok %v), want 5", got.ApproxBytes, ok)
	}
}

// Two saves at once must still leave one valid history file.
func TestConcurrentSavesLeaveAWholeFile(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	items := []model.Item{{ID: "0f8fad5b-d9cb-469f-a165-70867728950e", Kind: model.KindText, CapturedAt: model.Time{Time: time.Now()}, Text: model.Ptr("x"), CopyCount: 1}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if err := Save(l, items); err != nil {
					t.Errorf("save: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(l.History())
	if err != nil {
		t.Fatal(err)
	}
	var got []model.Item
	if err := json.Unmarshal(data, &got); err != nil || len(got) != 1 {
		t.Errorf("the final file is not a whole history: %v %s", err, data)
	}
}
