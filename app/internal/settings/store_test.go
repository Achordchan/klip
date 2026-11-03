package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"klip/internal/model"
)

func readJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A file written by the C# client loads, and what is saved reads the same:
// every property, the nulls, and the property this version does not know.
func TestCSharpFileRoundTrip(t *testing.T) {
	orig, err := os.ReadFile("../../testdata/settings_csharp.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, orig, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Theme != "Dark" || !s.StartWithWindows || s.MaxItems != 500 || s.CaptureImages ||
		s.ExcludePinnedFromLimits || s.ShelfTriggerModifier != "Alt" || s.AuthExpiresAtUnixMs != 1780000000000 {
		t.Errorf("loaded wrongly: %+v", s)
	}
	if model.Str(s.AuthUserEmail) != "user@example.com" || s.AuthIdToken != nil {
		t.Errorf("nullable strings: %+v", s)
	}

	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(path)
	if !reflect.DeepEqual(readJSON(t, orig), readJSON(t, saved)) {
		t.Errorf("round trip changed the file\n was: %s\n now: %s", orig, saved)
	}
	// The file is also byte for byte what System.Text.Json writes for the
	// known properties, with the unknown one last.
	if !strings.HasPrefix(string(saved), `{"Theme":"Dark","StartWithWindows":true,"AuthAccessToken":"eyJhbGciOi.access",`) ||
		!strings.HasSuffix(string(saved), `"ShelfTriggerModifier":"Alt","FutureFlag":{"nested":[1,2,3]}}`) {
		t.Errorf("shape: %s", saved)
	}
}

func TestDefaultsWhenMissingOrBroken(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(filepath.Join(dir, "none.json"))
	if err != nil || !reflect.DeepEqual(s, model.DefaultSettings()) {
		t.Errorf("missing file: %v %+v", err, s)
	}

	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"MaxItems": "many"}`), 0o644)
	s, err = Load(bad)
	if err == nil || s.MaxItems != 200 {
		t.Errorf("wrong type should fail and give defaults: %v %+v", err, s)
	}

	partial := filepath.Join(dir, "partial.json")
	os.WriteFile(partial, []byte(`{"Theme":"Light"}`), 0o644)
	s, err = Load(partial)
	if err != nil || s.Theme != "Light" || s.MaxItems != 200 || !s.ShelfEnabled {
		t.Errorf("partial file: %v %+v", err, s)
	}

	// C# matches names exactly: "theme" is another property.
	lower := filepath.Join(dir, "lower.json")
	os.WriteFile(lower, []byte(`{"theme":"Light"}`), 0o644)
	s, _ = Load(lower)
	if s.Theme != "System" || s.Extra["theme"] == nil {
		t.Errorf("names are case-sensitive: %+v", s)
	}
}

func TestLegacySyncURLIsMigratedAndWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"NasAgentBaseUrl":"http://47.122.83.145:17655"}`), 0o644)
	s, err := Load(path)
	if err != nil || s.NasAgentBaseUrl != model.OfficialSyncBaseUrl {
		t.Fatalf("%v %+v", err, s)
	}
	saved, _ := os.ReadFile(path)
	if readJSON(t, saved)["NasAgentBaseUrl"] != model.OfficialSyncBaseUrl {
		t.Errorf("not written back: %s", saved)
	}
}

func TestMarshalOfDefaults(t *testing.T) {
	// What new AppSettings() serializes to in C#.
	want := `{"Theme":"System","StartWithWindows":false,"AuthAccessToken":null,"AuthRefreshToken":null,"AuthIdToken":null,` +
		`"AuthExpiresAtUnixMs":0,"AuthUserEmail":null,"NasAgentBaseUrl":"https://maccy.baymaxgroup.com",` +
		`"SyncLastLocalFingerprint":null,"SyncLastRemoteVersion":null,"SyncLastRemoteUpdatedAt":null,"SyncLastRemoteSha256":null,` +
		`"MaxItems":200,"MaxMegabytes":300,"CaptureText":true,"CaptureImages":true,"CaptureFiles":true,` +
		`"CaptureFileExtensions":".pdf,.ppt,.pptx,.doc,.docx,.xls,.xlsx,.txt,.md,.csv,.zip,.rar,.7z,.png,.jpg,.jpeg,.gif,.bmp,.webp",` +
		`"CaptureFileMaxMegabytes":20,"MergeDuplicates":true,"ExcludePinnedFromLimits":true,"ShelfEnabled":true,"ShelfTriggerModifier":"Ctrl"}`
	got, _ := model.DefaultSettings().Marshal()
	if string(got) != want {
		t.Errorf("\n got: %s\nwant: %s", got, want)
	}
}

// The file holds the tokens: it and its folder are private to this user, and
// a save tightens what an older version left world-readable.
func TestSavedSettingsArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "Klip")
	path := filepath.Join(dir, "settings.json")
	s := model.DefaultSettings()
	s.AuthAccessToken = model.Ptr("access")
	s.AuthRefreshToken = model.Ptr("refresh")
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	checkMode(t, dir, 0o700)
	checkMode(t, path, 0o600)

	// What 1.0.10 with a 022 umask left behind: reading the file tightens it
	// even before the next save.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	checkMode(t, dir, 0o700)
	checkMode(t, path, 0o600)

	// And so does a save.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	checkMode(t, dir, 0o700)
	checkMode(t, path, 0o600)
}

func checkMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != want {
		t.Errorf("%s: mode %04o, want %04o", path, got, want)
	}
}
