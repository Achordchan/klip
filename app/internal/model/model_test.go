package model

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestTimeRoundTrip(t *testing.T) {
	// What System.Text.Json writes must come back byte for byte.
	for _, s := range []string{
		"2026-05-30T20:22:32.1234567+08:00",
		"2026-05-30T12:22:32+00:00",
		"2026-05-30T12:22:32.5+00:00",
		"2026-05-30T12:22:32.0000001-05:30",
	} {
		v, err := ParseTime(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if got := FormatSTJ(v); got != s {
			t.Errorf("STJ round trip: %s -> %s", s, got)
		}
	}
	// A time read with Z is written +00:00.
	v, _ := ParseTime("2026-05-30T12:22:32Z")
	if got := FormatSTJ(v); got != "2026-05-30T12:22:32+00:00" {
		t.Errorf("Z -> %s", got)
	}
	// Go times are finer than .NET ticks.
	ns := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	if got := FormatSTJ(ns); got != "2026-01-02T03:04:05.1234567+00:00" {
		t.Errorf("ns -> %s", got)
	}
	// "O" always has seven digits and never Z.
	if got := FormatO(ns); got != "2026-01-02T03:04:05.1234567+00:00" {
		t.Errorf("O -> %s", got)
	}
	whole := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := FormatO(whole); got != "2026-01-02T03:04:05.0000000+00:00" {
		t.Errorf("O whole -> %s", got)
	}
}

// csharp reads a C# source file of the neighbouring client. The tests below
// compare this package with it, so that the two cannot drift apart. They
// skip once the C# client is gone from the repository.
func csharp(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../../klip/" + rel)
	if err != nil {
		t.Skipf("C# source not available: %v", err)
	}
	return string(b)
}

func TestSettingsMatchCSharpClass(t *testing.T) {
	src := csharp(t, "Services/AppSettings.cs")
	re := regexp.MustCompile(`public (\w+\??) (\w+) \{ get; set; \}(?: = (.+);)?`)
	m := re.FindAllStringSubmatch(src, -1)

	s := DefaultSettings()
	var names []string
	for _, f := range s.fields() {
		names = append(names, f.name)
	}
	var csNames []string
	for _, x := range m {
		csNames = append(csNames, x[2])
	}
	if strings.Join(names, ",") != strings.Join(csNames, ",") {
		t.Fatalf("property list differs\n go: %v\n C#: %v", names, csNames)
	}

	// Defaults, for the properties that have a literal one.
	out, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	doc := string(out)
	for _, x := range m {
		name, def := x[2], x[3]
		var want string
		switch {
		case def == "":
			want = "null" // nullable string or number default
			if x[1] == "long" || x[1] == "int" || x[1] == "bool" {
				want = map[string]string{"long": "0", "int": "0", "bool": "false"}[x[1]]
			}
		case strings.HasPrefix(def, `"`):
			want = def
		case def == "true" || def == "false" || regexp.MustCompile(`^\d+$`).MatchString(def):
			want = def
		default:
			continue // ServerDefaults.OfficialSyncBaseUrl: checked below
		}
		if !strings.Contains(doc, `"`+name+`":`+want) {
			t.Errorf("default of %s: C# says %s, Go wrote %s", name, want, doc)
		}
	}

	d := csharp(t, "Services/ServerDefaults.cs")
	if !strings.Contains(d, `OfficialSyncBaseUrl = "`+OfficialSyncBaseUrl+`"`) {
		t.Error("OfficialSyncBaseUrl differs from ServerDefaults.cs")
	}
}

func TestItemMatchesCSharpRecord(t *testing.T) {
	src := csharp(t, "Models/ClipboardItem.cs")
	re := regexp.MustCompile(`\n    [\w<>?]+ (\w+)(?: = [^,\n]+)?[,\n]`)
	var cs []string
	for _, x := range re.FindAllStringSubmatch(src[strings.Index(src, "("):], -1) {
		cs = append(cs, x[1])
	}
	var tags []string
	for _, tag := range []string{"Id", "Kind", "CapturedAt", "ApproxBytes", "Text", "ImageFilePath", "FilePaths", "Pinned",
		"CopyCount", "FirstCapturedAt", "Note", "ContentHash", "SourceAppName", "SourceAppPath"} {
		tags = append(tags, tag)
	}
	if strings.Join(cs, ",") != strings.Join(tags, ",") {
		t.Fatalf("record parameters differ\n go: %v\n C#: %v", tags, cs)
	}

	kinds := csharp(t, "Models/ClipboardItemKind.cs")
	if !regexp.MustCompile(`Text,\s*Image,\s*FileList,`).MatchString(kinds) {
		t.Error("ClipboardContentKind order changed: Text, Image, FileList")
	}
}

// RestorePortable undoes only the settings that are still what was
// installed.
func TestRestorePortable(t *testing.T) {
	old := DefaultSettings()
	installed := old
	installed.MaxItems, installed.Theme, installed.CaptureImages = 500, "Light", false
	cur := installed
	cur.Theme = "Dark" // changed since by someone else
	cur.RestorePortable(installed, old)
	if cur.MaxItems != old.MaxItems || cur.CaptureImages != old.CaptureImages {
		t.Fatalf("not undone: %d %v", cur.MaxItems, cur.CaptureImages)
	}
	if cur.Theme != "Dark" {
		t.Fatalf("a later change was undone: %q", cur.Theme)
	}
}
