package snapshot

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"klip/internal/model"
	"klip/internal/paths"
)

var ctx = context.Background()

func mustTime(t *testing.T, s string) model.Time {
	t.Helper()
	v, err := model.ParseTime(s)
	if err != nil {
		t.Fatal(err)
	}
	return model.Time{Time: v}
}

// device is a data folder with a history that has the three kinds of item.
type device struct {
	l     paths.Layout
	items []model.Item
}

func newDevice(t *testing.T) device {
	t.Helper()
	l := paths.Layout{Root: t.TempDir()}
	os.MkdirAll(l.Images(), 0o755)
	img := filepath.Join(l.Images(), "1780000000000_aaaa.png")
	os.WriteFile(img, []byte("\x89PNG-bytes"), 0o644)

	id3 := "16fd2706-8baf-433b-82eb-8c7fada847da"
	cache := filepath.Join(l.Files(), strings.ReplaceAll(id3, "-", ""))
	os.MkdirAll(cache, 0o755)
	os.WriteFile(filepath.Join(cache, "0000_report.pdf"), []byte("pdf-one"), 0o644)
	os.WriteFile(filepath.Join(cache, "0001_数据.csv"), []byte("a,b\n1,2\n"), 0o644)

	items := []model.Item{
		{ID: "0f8fad5b-d9cb-469f-a165-70867728950e", Kind: model.KindText, CapturedAt: mustTime(t, "2026-05-30T20:22:32.1234567+08:00"),
			ApproxBytes: 16, Text: model.Ptr("你好, Klip"), Pinned: true, CopyCount: 3,
			FirstCapturedAt: ptrTime(mustTime(t, "2026-05-29T09:00:00+08:00")), Note: model.Ptr("note"),
			ContentHash: model.Ptr("abc"), SourceAppName: model.Ptr("Notepad"), SourceAppPath: model.Ptr(`C:\Windows\notepad.exe`)},
		{ID: "7c9e6679-7425-40de-944b-e07fc1f90ae7", Kind: model.KindImage, CapturedAt: mustTime(t, "2026-05-30T12:20:00+00:00"),
			ApproxBytes: 10, ImageFilePath: &img, CopyCount: 1, ContentHash: model.Ptr("img")},
		{ID: id3, Kind: model.KindFileList, CapturedAt: mustTime(t, "2026-05-30T12:10:00.5+00:00"), ApproxBytes: 15,
			FilePaths: []string{`C:\src\report.pdf`, `C:\src\数据.csv`}, CopyCount: 2},
	}
	return device{l, items}
}

func ptrTime(t model.Time) *model.Time { return &t }

func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The DDL is the text in SnapshotSqliteService.cs, not a copy that can
// drift.
func TestDDLIsTheCSharpOne(t *testing.T) {
	b, err := os.ReadFile("../../../klip/Services/SnapshotSqliteService.cs")
	if err != nil {
		t.Skipf("C# source not available: %v", err)
	}
	m := regexp.MustCompile("(?s)cmd\\.CommandText = @\"\n(PRAGMA journal_mode=MEMORY;.*?)\";").FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("DDL not found in the C# source")
	}
	if strings.TrimSpace(m[1]) != strings.TrimSpace(ddl) {
		t.Errorf("DDL differs from the C# one\nC#:\n%s\nGo:\n%s", m[1], ddl)
	}
	if !regexp.MustCompile(`SchemaVersion = 2;`).Match(b) {
		t.Error("the C# schema version is no longer 2")
	}
}

func TestExportWritesWhatCSharpReads(t *testing.T) {
	d := newDevice(t)
	snap := filepath.Join(t.TempDir(), "out", "snapshot.sqlite")
	s := model.DefaultSettings()
	s.Theme = "Dark"
	s.AuthAccessToken = model.Ptr("secret-token")
	s.MaxItems = 99

	res, err := Export(ctx, d.l, snap, d.items, s)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(snap)
	if res.Size != st.Size() || len(res.SHA256) != 64 {
		t.Errorf("result %+v, file size %d", res, st.Size())
	}
	if len(res.Blobs) != 3 {
		t.Fatalf("blobs: %+v", res.Blobs)
	}

	db := openDB(t, snap)
	var schema string
	db.QueryRow("SELECT value FROM meta WHERE key='schema'").Scan(&schema)
	if schema != "2" {
		t.Errorf("schema %q", schema)
	}

	// Column types and text formats are those of the C# writer.
	var id, captured, first, filePaths, typeKind, typeBytes string
	var pos, pinned, copyCount int
	err = db.QueryRow(`SELECT position, id, capturedAt, firstCapturedAt, pinned, copyCount, typeof(kind), typeof(approxBytes) FROM items WHERE position=0`).
		Scan(&pos, &id, &captured, &first, &pinned, &copyCount, &typeKind, &typeBytes)
	if err != nil {
		t.Fatal(err)
	}
	if id != "0f8fad5b-d9cb-469f-a165-70867728950e" || captured != "2026-05-30T20:22:32.1234567+08:00" ||
		first != "2026-05-29T09:00:00.0000000+08:00" || pinned != 1 || copyCount != 3 || typeKind != "integer" {
		t.Errorf("text row: %v %v %v %v %v %v %v", id, captured, first, pinned, copyCount, typeKind, typeBytes)
	}
	var capturedUTC string
	db.QueryRow(`SELECT capturedAt FROM items WHERE position=1`).Scan(&capturedUTC)
	if capturedUTC != "2026-05-30T12:20:00.0000000+00:00" {
		t.Errorf("UTC time must be written +00:00 with seven digits, not Z: %q", capturedUTC)
	}
	db.QueryRow(`SELECT filePathsJson FROM items WHERE position=2`).Scan(&filePaths)
	var paths []string
	json.Unmarshal([]byte(filePaths), &paths)
	if len(paths) != 2 {
		t.Errorf("filePathsJson %q", filePaths)
	}
	var nullText any
	db.QueryRow(`SELECT text FROM items WHERE position=1`).Scan(&nullText)
	if nullText != nil {
		t.Errorf("null text written as %v", nullText)
	}

	// Images and files are named by hash; their bytes stay out of the file.
	var name, sha string
	var imgBytes []byte
	db.QueryRow(`SELECT fileName, blobSha256, bytes FROM item_images`).Scan(&name, &sha, &imgBytes)
	if name != "1780000000000_aaaa.png" || len(sha) != 64 || imgBytes != nil {
		t.Errorf("image row: %q %q %v", name, sha, imgBytes)
	}
	rows, _ := db.Query(`SELECT idx, fileName FROM item_files ORDER BY idx`)
	var got []string
	for rows.Next() {
		var idx int
		var n string
		rows.Scan(&idx, &n)
		got = append(got, n)
	}
	if !reflect.DeepEqual(got, []string{"report.pdf", "数据.csv"}) {
		t.Errorf("file names %v (the NNNN_ prefix is not part of the name)", got)
	}

	// The settings are the portable ones: no token, the rest as set.
	var sj string
	db.QueryRow(`SELECT json FROM settings`).Scan(&sj)
	var doc map[string]any
	json.Unmarshal([]byte(sj), &doc)
	if doc["AuthAccessToken"] != nil || doc["Theme"] != "Dark" || doc["MaxItems"] != float64(99) || len(doc) != 23 {
		t.Errorf("settings json: %s", sj)
	}
}

func TestRoundTripBetweenTwoDevices(t *testing.T) {
	a := newDevice(t)
	snap := filepath.Join(t.TempDir(), "snapshot.sqlite")
	s := model.DefaultSettings()
	s.ShelfTriggerModifier = "Alt"
	res, err := Export(ctx, a.l, snap, a.items, s)
	if err != nil {
		t.Fatal(err)
	}

	// Device B has none of the content: it fetches blobs by hash.
	b := paths.Layout{Root: t.TempDir()}
	fetched := map[string]int{}
	byHash := map[string]string{}
	for _, bl := range res.Blobs {
		byHash[bl.SHA256] = bl.SourcePath
	}
	ensure := func(sha string) (string, error) {
		fetched[sha]++
		dest := b.BlobPath(sha)
		os.MkdirAll(filepath.Dir(dest), 0o755)
		data, _ := os.ReadFile(byHash[sha])
		return dest, os.WriteFile(dest, data, 0o644)
	}
	got, applied, err := Read(ctx, b, snap, ensure)
	if err != nil {
		t.Fatal(err)
	}
	applied.Commit()
	if got.ImageCount != 1 || got.FileCount != 2 || len(got.Items) != 3 || len(fetched) != 3 {
		t.Fatalf("counts: %+v fetched %v", got, fetched)
	}
	if got.Settings == nil || got.Settings.ShelfTriggerModifier != "Alt" {
		t.Errorf("settings %+v", got.Settings)
	}

	for i, want := range a.items {
		it := got.Items[i]
		if it.ID != want.ID || it.Kind != want.Kind || it.ApproxBytes != want.ApproxBytes || it.Pinned != want.Pinned ||
			it.CopyCount != want.CopyCount || model.Str(it.Text) != model.Str(want.Text) || model.Str(it.Note) != model.Str(want.Note) ||
			model.Str(it.ContentHash) != model.Str(want.ContentHash) || model.Str(it.SourceAppPath) != model.Str(want.SourceAppPath) {
			t.Errorf("item %d differs:\n got %+v\nwant %+v", i, it, want)
		}
		if !it.CapturedAt.Equal(want.CapturedAt.Time) {
			t.Errorf("item %d time %v want %v", i, it.CapturedAt, want.CapturedAt)
		}
		if _, off := it.CapturedAt.Zone(); func() int { _, o := want.CapturedAt.Zone(); return o }() != off {
			t.Errorf("item %d lost its offset", i)
		}
	}

	// The content is on B, under B's folders.
	data, err := os.ReadFile(model.Str(got.Items[1].ImageFilePath))
	if err != nil || string(data) != "\x89PNG-bytes" || !strings.HasPrefix(model.Str(got.Items[1].ImageFilePath), b.Images()) {
		t.Errorf("image: %q %v", data, err)
	}
	if !regexp.MustCompile(`^\d+_[0-9a-f]{32}\.png$`).MatchString(filepath.Base(model.Str(got.Items[1].ImageFilePath))) {
		t.Errorf("image name %q", model.Str(got.Items[1].ImageFilePath))
	}
	files := got.Items[2].FilePaths
	idn := "16fd27068baf433b82eb8c7fada847da"
	if len(files) != 2 || filepath.Base(files[0]) != "0000_report.pdf" || filepath.Base(files[1]) != "0001_数据.csv" ||
		!strings.HasPrefix(filepath.Base(filepath.Dir(files[0])), idn+"-") || filepath.Dir(files[0]) != filepath.Dir(files[1]) {
		t.Errorf("files %v", files)
	}
	if d, _ := os.ReadFile(files[1]); string(d) != "a,b\n1,2\n" {
		t.Errorf("file content %q", d)
	}

	// With the blobs in place, reading again fetches nothing.
	fetched = map[string]int{}
	if _, _, err := Read(ctx, b, snap, ensure); err != nil || len(fetched) != 0 {
		t.Errorf("second read: %v fetched %v", err, fetched)
	}
}

// csharpSnapshot builds a snapshot with raw SQL, the way the C# code
// writes its rows, apart from Export: a check that Read does not just
// understand its own writer. schema 1 carries the bytes in the file.
func csharpSnapshot(t *testing.T, schema int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snapshot.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	exec(`CREATE TABLE settings (json TEXT NOT NULL)`)
	exec(`CREATE TABLE items (position INTEGER NOT NULL, id TEXT PRIMARY KEY, kind INTEGER NOT NULL, capturedAt TEXT NOT NULL, approxBytes INTEGER NOT NULL, text TEXT, filePathsJson TEXT, pinned INTEGER NOT NULL, copyCount INTEGER NOT NULL, firstCapturedAt TEXT, note TEXT, contentHash TEXT, sourceAppName TEXT, sourceAppPath TEXT)`)
	if schema == 1 {
		exec(`CREATE TABLE item_images (itemId TEXT PRIMARY KEY, fileName TEXT NOT NULL, bytes BLOB NOT NULL)`)
		exec(`CREATE TABLE item_files (itemId TEXT NOT NULL, idx INTEGER NOT NULL, fileName TEXT NOT NULL, bytes BLOB NOT NULL, PRIMARY KEY(itemId, idx))`)
	} else {
		exec(`CREATE TABLE item_images (itemId TEXT PRIMARY KEY, fileName TEXT NOT NULL, blobSha256 TEXT, sizeBytes INTEGER, bytes BLOB)`)
		exec(`CREATE TABLE item_files (itemId TEXT NOT NULL, idx INTEGER NOT NULL, fileName TEXT NOT NULL, blobSha256 TEXT, sizeBytes INTEGER, bytes BLOB, PRIMARY KEY(itemId, idx))`)
	}
	exec(`INSERT INTO meta VALUES ('schema', ?)`, map[int]string{1: "1", 2: "2"}[schema])
	exec(`INSERT INTO settings VALUES (?)`, `{"Theme":"Light","StartWithWindows":false,"AuthAccessToken":null,"AuthRefreshToken":null,"AuthIdToken":null,"AuthExpiresAtUnixMs":0,"AuthUserEmail":null,"NasAgentBaseUrl":"https://maccy.baymaxgroup.com","SyncLastLocalFingerprint":null,"SyncLastRemoteVersion":null,"SyncLastRemoteUpdatedAt":null,"SyncLastRemoteSha256":null,"MaxItems":50,"MaxMegabytes":100,"CaptureText":true,"CaptureImages":true,"CaptureFiles":false,"CaptureFileExtensions":".txt","CaptureFileMaxMegabytes":5,"MergeDuplicates":false,"ExcludePinnedFromLimits":true,"ShelfEnabled":false,"ShelfTriggerModifier":"Ctrl"}`)
	exec(`INSERT INTO items VALUES (0,'0f8fad5b-d9cb-469f-a165-70867728950e',0,'2026-05-30T20:22:32.1234567+08:00',16,'你好',NULL,1,2,'2026-05-29T09:00:00.0000000+08:00','n','h','App','C:\\app.exe')`)
	exec(`INSERT INTO items VALUES (1,'7c9e6679-7425-40de-944b-e07fc1f90ae7',1,'2026-05-30T12:20:00.0000000+00:00',4,NULL,NULL,0,1,NULL,NULL,NULL,NULL,NULL)`)
	exec(`INSERT INTO items VALUES (2,'16fd2706-8baf-433b-82eb-8c7fada847da',2,'2026-05-30T12:10:00.0000000+00:00',3,NULL,'["C:\\src\\a.txt"]',0,1,NULL,NULL,NULL,NULL,NULL)`)
	if schema == 1 {
		exec(`INSERT INTO item_images VALUES ('7c9e6679-7425-40de-944b-e07fc1f90ae7','pic.jpg',?)`, []byte("jpeg"))
		exec(`INSERT INTO item_files VALUES ('16fd2706-8baf-433b-82eb-8c7fada847da',0,'a.txt',?)`, []byte("abc"))
	} else {
		exec(`INSERT INTO item_images VALUES ('7c9e6679-7425-40de-944b-e07fc1f90ae7','pic.jpg',NULL,4,?)`, []byte("jpeg"))
		exec(`INSERT INTO item_files VALUES ('16fd2706-8baf-433b-82eb-8c7fada847da',0,'a:b.txt',NULL,3,?)`, []byte("abc"))
	}
	return path
}

func TestReadsSnapshotsWrittenByTheCSharpClient(t *testing.T) {
	for _, schema := range []int{1, 2} {
		l := paths.Layout{Root: t.TempDir()}
		res, applied, err := Read(ctx, l, csharpSnapshot(t, schema), nil)
		if err != nil {
			t.Fatalf("schema %d: %v", schema, err)
		}
		if len(res.Items) != 3 || res.ImageCount != 1 || res.FileCount != 1 {
			t.Fatalf("schema %d: %+v", schema, res)
		}
		applied.Commit()
		if model.Str(res.Items[0].Text) != "你好" || !res.Items[0].Pinned || res.Items[0].CopyCount != 2 ||
			res.Items[0].FirstCapturedAt == nil {
			t.Errorf("schema %d text item: %+v", schema, res.Items[0])
		}
		img := model.Str(res.Items[1].ImageFilePath)
		if filepath.Ext(img) != ".jpg" {
			t.Errorf("the image keeps its extension: %q", img)
		}
		if d, _ := os.ReadFile(img); string(d) != "jpeg" {
			t.Errorf("image bytes %q", d)
		}
		f := res.Items[2].FilePaths[0]
		wantName := "0000_a.txt"
		if schema == 2 {
			wantName = "0000_a_b.txt" // ':' is not allowed in a Windows file name
		}
		if filepath.Base(f) != wantName {
			t.Errorf("schema %d file name %q", schema, filepath.Base(f))
		}
		if d, _ := os.ReadFile(f); string(d) != "abc" {
			t.Errorf("file bytes %q", d)
		}
		if res.Settings == nil || res.Settings.Theme != "Light" || res.Settings.MaxItems != 50 || res.Settings.CaptureFiles {
			t.Errorf("settings %+v", res.Settings)
		}
	}
}

func TestErrors(t *testing.T) {
	d := newDevice(t)
	dir := t.TempDir()

	// An item whose content is gone stops the export, as in C#.
	os.Remove(model.Str(d.items[1].ImageFilePath))
	if _, err := Export(ctx, d.l, filepath.Join(dir, "a.sqlite"), d.items, model.DefaultSettings()); err == nil ||
		!strings.Contains(err.Error(), "image payload missing") {
		t.Errorf("missing image: %v", err)
	}

	// A blob that is neither local nor fetchable.
	l := paths.Layout{Root: t.TempDir()}
	d2 := newDevice(t)
	snap := filepath.Join(dir, "b.sqlite")
	if _, err := Export(ctx, d2.l, snap, d2.items, model.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(ctx, l, snap, nil); err == nil || !strings.Contains(err.Error(), "blob missing") {
		t.Errorf("missing blob: %v", err)
	}

	// An unknown schema is refused.
	bad := csharpSnapshot(t, 2)
	db, _ := sql.Open("sqlite", bad)
	db.Exec(`UPDATE meta SET value='3'`)
	db.Close()
	if _, _, err := Read(ctx, l, bad, nil); err == nil || !strings.Contains(err.Error(), "unsupported snapshot schema") {
		t.Errorf("schema 3: %v", err)
	}
	if _, _, err := Read(ctx, l, filepath.Join(dir, "none.sqlite"), nil); err == nil {
		t.Error("missing file")
	}
}

func TestDuplicateIDsAreSkippedAndPositionsStayContiguous(t *testing.T) {
	d := newDevice(t)
	items := []model.Item{d.items[0], d.items[0], d.items[1]}
	snap := filepath.Join(t.TempDir(), "s.sqlite")
	if _, err := Export(ctx, d.l, snap, items, model.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	rows, _ := openDB(t, snap).Query(`SELECT position, id FROM items ORDER BY position`)
	var n int
	for rows.Next() {
		var p int
		var id string
		rows.Scan(&p, &id)
		if p != n {
			t.Errorf("position %d, want %d", p, n)
		}
		n++
	}
	if n != 2 {
		t.Errorf("%d rows", n)
	}
}

func TestIndexPrefix(t *testing.T) {
	for name, want := range map[string]int{
		"0000_a.txt": 0, "0012_x": 12, "10000_big.txt": 10000,
		"a.txt": -1, "00_a.txt": -1, "0001a.txt": -1, "abcd_x": -1,
	} {
		if got, _ := indexPrefix(name); got != want {
			t.Errorf("%q: %d want %d", name, got, want)
		}
	}
	if _, cut := indexPrefix("10000_big.txt"); "10000_big.txt"[cut:] != "big.txt" {
		t.Errorf("cut %d", cut)
	}
}

func TestTimeIsUsableAfterExport(t *testing.T) {
	// A time captured now, with nanoseconds, survives as ticks.
	d := newDevice(t)
	now := time.Date(2026, 10, 20, 9, 30, 15, 123456789, time.FixedZone("", 8*3600))
	d.items[0].CapturedAt = model.Time{Time: now}
	snap := filepath.Join(t.TempDir(), "s.sqlite")
	if _, err := Export(ctx, d.l, snap, d.items[:1], model.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	var s string
	openDB(t, snap).QueryRow(`SELECT capturedAt FROM items`).Scan(&s)
	if s != "2026-10-20T09:30:15.1234567+08:00" {
		t.Errorf("%s", s)
	}
}

func TestSanitizeFileNameTrimsWindowsTrailingDotsAndSpaces(t *testing.T) {
	cases := map[string]string{
		"notes.":   "notes",
		"notes ":   "notes",
		"a.b.":     "a.b",
		"a:b?.txt": "a_b_.txt",
		"  ":       "file",
		"..":       "file",
	}
	for in, want := range cases {
		if got := sanitizeFileName(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}
