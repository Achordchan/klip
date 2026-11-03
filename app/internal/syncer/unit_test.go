package syncer

import (
	"errors"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"klip/internal/model"
	"klip/internal/syncclient"
)

func mt(t *testing.T, s string) model.Time {
	t.Helper()
	v, err := model.ParseTime(s)
	if err != nil {
		t.Fatal(err)
	}
	return model.Time{Time: v}
}

func tp(t *testing.T, s string) *time.Time {
	v := mt(t, s).Time
	return &v
}

func sampleItems(t *testing.T) []model.Item {
	first := mt(t, "2026-05-29T09:00:00+08:00")
	return []model.Item{
		{ID: "0f8fad5b-d9cb-469f-a165-70867728950e", Kind: model.KindText, CapturedAt: mt(t, "2026-05-30T20:22:32.1234567+08:00"),
			ApproxBytes: 16, Text: model.Ptr("你好, Klip\nsecond line"), Pinned: true, CopyCount: 3, FirstCapturedAt: &first,
			Note: model.Ptr("备注|x"), ContentHash: model.Ptr("abc")},
		{ID: "7c9e6679-7425-40de-944b-e07fc1f90ae7", Kind: model.KindImage, CapturedAt: mt(t, "2026-05-30T12:20:00+00:00"),
			ApproxBytes: 5, CopyCount: 1, ContentHash: model.Ptr("img")},
		{ID: "16fd2706-8baf-433b-82eb-8c7fada847da", Kind: model.KindFileList, CapturedAt: mt(t, "2026-05-30T12:10:00.5Z"),
			ApproxBytes: 7, CopyCount: 2, FilePaths: []string{`C:\src\report.pdf`, "/home/u/data.csv"}},
	}
}

// The expected values come from fp.py, an independent computation of the C#
// algorithm (ticks since 0001, the platform's line end) in Python.
func TestFingerprintGolden(t *testing.T) {
	items := sampleItems(t)
	if got := fingerprint(items, false); got != "25a2322ef7fb21b3f9f4364c41c280d28f67d3debdcccdda3a1776121b74c0b6" {
		t.Errorf("unix: %s", got)
	}
	if got := fingerprint(items, true); got != "119515c2fbf24d53112a2c21247300fd4fa0869b4b327de626d43d0f3304f0df" {
		t.Errorf("windows: %s", got)
	}
	if got := fingerprint(nil, false); got != "103b92e45253890820db806383c5f4c25ae009d8cbada5f175afac93ddd445d1" {
		t.Errorf("empty: %s", got)
	}
	if got := ticks(items[0].CapturedAt.Time); got != 639157405521234567 {
		t.Errorf("ticks %d", got)
	}
	// A different pinned flag, note or file name is a different history.
	a := fingerprint(items, false)
	items[1].Pinned = true
	if fingerprint(items, false) == a {
		t.Error("pinned is part of the fingerprint")
	}
}

// The fields of the fingerprint, in order, are those of the C# source.
func TestFingerprintFieldsMatchCSharp(t *testing.T) {
	b, err := os.ReadFile("../../../klip/Services/SyncService.cs")
	if err != nil {
		t.Skipf("C# source not available: %v", err)
	}
	src := string(b)
	body := src[strings.Index(src, "foreach (var item in _history.Items)"):]
	body = body[:strings.Index(body, "writer.WriteLine();")]
	var got []string
	for _, m := range regexp.MustCompile(`writer\.Write\((.+)\);`).FindAllStringSubmatch(body, -1) {
		if m[1] != "'|'" {
			got = append(got, m[1])
		}
	}
	want := []string{
		"item.Id", "(int)item.Kind", "item.CapturedAt.UtcDateTime.Ticks", "item.ApproxBytes", "item.Pinned ? '1' : '0'",
		"item.CopyCount", "item.FirstCapturedAt?.UtcDateTime.Ticks ?? 0", "item.Note ?? string.Empty",
		"item.ContentHash ?? string.Empty", "item.Text ?? string.Empty", `string.Join(",", item.FilePaths.Select(Path.GetFileName))`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the fingerprint fields changed in C#\n got: %q\nwant: %q", got, want)
	}
	if !strings.Contains(src, `writer.WriteLine("schema=1");`) {
		t.Error("the fingerprint header changed")
	}
}

func TestParseManifestInfo(t *testing.T) {
	m := ParseManifestInfo(`{"schema":1,"latest":{"version":"1780000000000","updatedAt":"2026-05-30T12:22:32.123456+00:00","snapshot":{"sha256":"ABC","size":1772}}}`)
	if m.Version != "1780000000000" || m.UpdatedAt == nil || m.UpdatedAt.UTC().Format(time.RFC3339Nano) != "2026-05-30T12:22:32.123456Z" ||
		m.SnapshotSHA256 != "ABC" || m.SnapshotSize == nil || *m.SnapshotSize != 1772 {
		t.Errorf("%+v", m)
	}
	// A version that is a number is its text; a size that is a string counts.
	m = ParseManifestInfo(`{"latest":{"version":17,"snapshot":{"size":"42"}}}`)
	if m.Version != "17" || *m.SnapshotSize != 42 || m.UpdatedAt != nil {
		t.Errorf("%+v", m)
	}
	for _, in := range []string{"", "  ", "not json", `{}`, `{"latest":null}`} {
		if m := ParseManifestInfo(in); m.Version != "" || m.UpdatedAt != nil || m.RawJSON != in {
			t.Errorf("%q: %+v", in, m)
		}
	}
}

func settingsWith(fp, version, updatedAt, sha string) model.Settings {
	s := model.DefaultSettings()
	if fp != "" {
		s.SyncLastLocalFingerprint = &fp
	}
	if version != "" {
		s.SyncLastRemoteVersion = &version
	}
	if updatedAt != "" {
		s.SyncLastRemoteUpdatedAt = &updatedAt
	}
	if sha != "" {
		s.SyncLastRemoteSha256 = &sha
	}
	return s
}

func TestDecide(t *testing.T) {
	remote := func(version, updated, sha string) ManifestInfo {
		m := ManifestInfo{Version: version, SnapshotSHA256: sha}
		if updated != "" {
			m.UpdatedAt = tp(t, updated)
		}
		return m
	}
	const T0 = "2026-05-30T12:00:00+00:00"
	cases := []struct {
		name     string
		manifest ManifestInfo
		fp       string
		count    int
		latest   string
		settings model.Settings
		dir      Direction
		reason   string
	}{
		{"no snapshot, empty history", remote("", "", ""), "fp", 0, "", settingsWith("", "", "", ""), None, "remote_missing"},
		{"no snapshot, some history", remote("", "", ""), "fp", 3, T0, settingsWith("", "", "", ""), Upload, "remote_missing"},
		{"first sync, both have data", remote("v1", T0, "a"), "fp", 3, T0, settingsWith("", "", "", ""), Merge, "first_sync_merge"},
		{"first sync, nothing local", remote("v1", T0, "a"), "fp", 0, "", settingsWith("", "", "", ""), Download, "first_sync_remote_exists"},
		// Having a fingerprint without a version is also "first sync".
		{"fingerprint but no version", remote("v1", T0, "a"), "fp", 3, T0, settingsWith("fp", "", "", ""), Merge, "first_sync_merge"},
		{"all as last time", remote("v1", T0, "a"), "fp", 3, T0, settingsWith("fp", "v1", T0, "a"), None, "state_matched"},
		{"sha case does not matter", remote("v1", T0, "ABC"), "fp", 3, T0, settingsWith("fp", "v1", T0, "abc"), None, "state_matched"},
		{"a missing sha counts as the same", remote("v1", T0, ""), "fp", 3, T0, settingsWith("fp", "v1", T0, "abc"), None, "state_matched"},
		{"local changed", remote("v1", T0, "a"), "fp2", 3, T0, settingsWith("fp", "v1", T0, "a"), Upload, "local_changed_only"},
		{"remote changed (version)", remote("v2", "2026-05-30T13:00:00+00:00", "b"), "fp", 3, T0, settingsWith("fp", "v1", T0, "a"), Download, "remote_changed_only"},
		{"remote changed (same version, other sha)", remote("v1", T0, "b"), "fp", 3, T0, settingsWith("fp", "v1", T0, "a"), Download, "remote_changed_only"},
		{"remote rolled back", remote("v0", "2026-05-30T11:00:00+00:00", "z"), "fp", 3, T0, settingsWith("fp", "v1", T0, "a"), Download, "remote_rolled_back"},
		{"a clock step under 2 s is no rollback", remote("v1", "2026-05-30T11:59:58.5+00:00", "a"), "fp", 3, T0, settingsWith("fp", "v1", T0, "a"), None, "state_matched"},
		// Both sides changed: the newer capture wins, by more than 2 s.
		{"both changed, local newer", remote("v2", "2026-05-30T13:00:00+00:00", "b"), "fp2", 3, "2026-05-30T14:00:00+00:00", settingsWith("fp", "v1", "2026-05-30T12:00:00+00:00", "a"), Upload, "timestamp_fallback"},
		{"both changed, remote newer", remote("v2", "2026-05-30T15:00:00+00:00", "b"), "fp2", 3, "2026-05-30T14:00:00+00:00", settingsWith("fp", "v1", "2026-05-30T12:00:00+00:00", "a"), Download, "timestamp_fallback"},
		{"both changed, within 2 s", remote("v2", "2026-05-30T14:00:01+00:00", "b"), "fp2", 3, "2026-05-30T14:00:00+00:00", settingsWith("fp", "v1", "2026-05-30T12:00:00+00:00", "a"), None, "timestamp_fallback"},
		{"both changed, local empty", remote("v2", "2026-05-30T15:00:00+00:00", "b"), "fp2", 0, "", settingsWith("fp", "v1", "2026-05-30T12:00:00+00:00", "a"), Download, "timestamp_fallback"},
	}
	for _, c := range cases {
		var latest *time.Time
		if c.latest != "" {
			latest = tp(t, c.latest)
		}
		d := Decide(c.manifest, c.fp, c.count, latest, c.settings)
		if d.Direction != c.dir || d.Reason != c.reason {
			t.Errorf("%s: %v/%s, want %v/%s", c.name, d.Direction, d.Reason, c.dir, c.reason)
		}
	}
}

func TestMergeItems(t *testing.T) {
	at := func(s string) model.Time { return mt(t, s) }
	item := func(id, hash string, kind model.Kind, captured string, mut func(*model.Item)) model.Item {
		it := model.Item{ID: id, Kind: kind, CapturedAt: at(captured), ApproxBytes: 10, CopyCount: 1}
		if hash != "" {
			it.ContentHash = model.Ptr(hash)
		}
		if mut != nil {
			mut(&it)
		}
		return it
	}
	const (
		a = "00000000-0000-0000-0000-00000000000a"
		b = "00000000-0000-0000-0000-00000000000b"
		c = "00000000-0000-0000-0000-00000000000c"
		d = "00000000-0000-0000-0000-00000000000d"
	)
	remote := []model.Item{
		item(a, "H1", model.KindText, "2026-05-30T10:00:00Z", func(i *model.Item) {
			i.Pinned, i.CopyCount, i.ApproxBytes, i.Note = true, 2, 30, model.Ptr("remote note")
			f := at("2026-05-01T00:00:00Z")
			i.FirstCapturedAt = &f
		}),
		item(b, "H2", model.KindText, "2026-05-30T09:00:00Z", nil),
		item(c, "", model.KindText, "2026-05-30T08:00:00Z", nil), // no hash: merged by id
		item(emptyGUID, "", model.KindText, "2026-05-30T07:00:00Z", nil),
	}
	local := []model.Item{
		item(d, "h1", model.KindText, "2026-05-30T11:00:00Z", func(i *model.Item) { // another id, same hash and kind, case aside
			i.CopyCount, i.ApproxBytes, i.Note = 5, 20, model.Ptr("local note")
		}),
		item(b, "H2", model.KindImage, "2026-05-30T09:30:00Z", nil), // same hash, another kind: not the same item
		item(c, "", model.KindText, "2026-05-30T12:00:00Z", func(i *model.Item) { i.Note = model.Ptr("  ") }),
		item(emptyGUID, "", model.KindText, "2026-05-30T06:00:00Z", nil),
	}
	got := MergeItems(remote, local)

	var ids []string
	for _, it := range got {
		ids = append(ids, it.ID[len(it.ID)-1:]+"@"+it.CapturedAt.UTC().Format("15"))
	}
	// Newest first: c (12h, merged by id), d (11h, the merged H1 duplicate),
	// b-image (9:30), b-text (9h), and the two items without a key, kept as
	// they are (7h, 6h).
	if want := "c@12 d@11 b@09 b@09 0@07 0@06"; strings.Join(ids, " ") != want {
		t.Errorf("order %v, want %s", ids, want)
	}

	h1 := got[1]
	if h1.ID != d || h1.CopyCount != 7 || h1.ApproxBytes != 30 || !h1.Pinned || model.Str(h1.Note) != "local note" ||
		h1.FirstCapturedAt == nil || !h1.FirstCapturedAt.Equal(at("2026-05-01T00:00:00Z").Time) {
		t.Errorf("merged duplicate: %+v", h1)
	}
	// A blank local note does not hide the other one; none stays none.
	if got[0].Note != nil || got[0].CopyCount != 2 || got[0].FirstCapturedAt == nil {
		t.Errorf("merged by id: %+v", got[0])
	}
	// Equal times sort by id.
	tie := MergeItems([]model.Item{item(b, "", model.KindText, "2026-05-30T10:00:00Z", nil), item(a, "", model.KindText, "2026-05-30T10:00:00Z", nil)}, nil)
	if tie[0].ID != a {
		t.Errorf("tie: %v", tie[0].ID)
	}
}

func TestMergedNote(t *testing.T) {
	s := model.Ptr
	if got := mergedNote(s("r"), s("l"), true); *got != "l" {
		t.Error("the local note wins")
	}
	if got := mergedNote(s("r"), s("l"), false); *got != "r" {
		t.Error("when the incoming one is the remote, the existing one stays")
	}
	if got := mergedNote(nil, s("x"), false); *got != "x" {
		t.Error("the only note")
	}
	if got := mergedNote(s(" "), s(" "), true); got != nil {
		t.Error("blank notes are none")
	}
}

func TestEnsureSettingsCanHoldItems(t *testing.T) {
	s := model.DefaultSettings() // 200 items, 300 MB
	items := make([]model.Item, 250)
	for i := range items {
		items[i].ApproxBytes = 2 * 1024 * 1024 // 500 MB in all
	}
	EnsureSettingsCanHoldItems(&s, items)
	if s.MaxItems != 250 || s.MaxMegabytes != 500 {
		t.Errorf("%d items, %d MB", s.MaxItems, s.MaxMegabytes)
	}
	// What fits is left alone.
	s = model.DefaultSettings()
	EnsureSettingsCanHoldItems(&s, items[:10])
	if s.MaxItems != 200 || s.MaxMegabytes != 300 {
		t.Errorf("%d %d", s.MaxItems, s.MaxMegabytes)
	}
}

func TestExplain(t *testing.T) {
	api := func(status int, code, body string) error {
		return &syncclient.APIError{StatusCode: status, Message: "m", Code: code, Body: body}
	}
	cases := []struct {
		err   error
		where Where
		want  string
	}{
		{ErrSubscriptionExpired, InMainWindow, "订阅已过期，请续费后重试"},
		{ErrNotLoggedIn, InMainWindow, "登录已失效，请重新登录"},
		{ErrNotLoggedIn, InPreferences, "请先登录"},
		{api(401, "", ""), InMainWindow, "登录已失效，请重新登录"},
		{api(404, "", ""), InPreferences, "云端还没有快照"},
		{api(413, "snapshot_too_large", ""), InMainWindow, "同步数据过大，请清理后重试"},
		{api(403, "storage_limit_exceeded", ""), InMainWindow, "云端存储已超限，请清理旧快照或升级套餐"},
		{api(403, "tier_not_assigned", ""), InMainWindow, "当前套餐不支持该同步操作，请升级套餐后重试"},
		{api(403, "subscription_expired", `{"error":"subscription expired"}`), InPreferences, "订阅已过期，请续费后重试"},
		{api(403, "", "nope"), InPreferences, "权限不足或订阅状态异常"},
		{api(403, "", "nope"), InMainWindow, "同步失败(403)"},
		{api(500, "", ""), InMainWindow, "同步失败(500)"},
		{errors.New("boom"), InMainWindow, "boom"},
		{errors.New("boom"), InPreferences, "同步失败：boom"},
	}
	for _, c := range cases {
		if got := Explain(c.err, c.where); got != c.want {
			t.Errorf("%v (%d): %q, want %q", c.err, c.where, got, c.want)
		}
	}
	// The texts and the order of the checks are those of the C# functions.
	b, err := os.ReadFile("../../../klip/ViewModels/MainWindowViewModel.cs")
	if err == nil {
		for _, text := range []string{"云端存储已超限，请清理旧快照或升级套餐", "当前套餐不支持该同步操作，请升级套餐后重试", "云端还没有快照", "同步数据过大，请清理后重试"} {
			if !strings.Contains(string(b), text) {
				t.Errorf("C# no longer says %q", text)
			}
		}
	}
}

// Copy counts that add up past the largest one stay at it.
func TestMergedCopyCountsDoNotWrap(t *testing.T) {
	it := func(id string, n int32) model.Item {
		return model.Item{ID: id, Kind: model.KindText, ContentHash: model.Ptr("same"), CopyCount: n, CapturedAt: model.Time{Time: time.Now()}}
	}
	got := MergeItems([]model.Item{it("a", math.MaxInt32)}, []model.Item{it("b", 1)})
	if len(got) != 1 || got[0].CopyCount != math.MaxInt32 {
		t.Fatalf("merged %v", got[0].CopyCount)
	}
	if got := addCounts(-5, 3); got != 3 {
		t.Fatalf("a negative count: %d", got)
	}
}
