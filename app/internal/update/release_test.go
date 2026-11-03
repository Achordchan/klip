package update

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The release tool is Python (tools/release_go.py) and the validator is Go:
// what one writes the other must accept, as the clients in the field would.
func releaseTool(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../../tools/release_go.py")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Skip("no release tool here")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	return p
}

func TestReleaseToolPackageIsAcceptedByTheClient(t *testing.T) {
	tool := releaseTool(t)
	pub := t.TempDir()
	write(t, filepath.Join(pub, "maccy.exe"), "NEW EXE")
	write(t, filepath.Join(pub, "mygo-sqlite3.dll"), "SQLITE")
	write(t, filepath.Join(pub, "res", "a.txt"), "A")

	// The package of the earlier (.NET) program, as release.py made it.
	var old bytes.Buffer
	zw := zip.NewWriter(&old)
	oldList := map[string]any{"appId": "maccy", "version": "1.0.10", "runtime": "win-x64", "files": []map[string]any{
		{"path": "maccy.exe", "sha256": "AA", "size": 1}, {"path": "maccy.dll", "sha256": "AA", "size": 1},
		{"path": "Avalonia.Base.dll", "sha256": "AA", "size": 1}, {"path": "runtimes/win-x64/native/libSkiaSharp.dll", "sha256": "AA", "size": 1},
		{"path": "settings.json", "sha256": "AA", "size": 1}, {"path": "mygo-sqlite3.dll", "sha256": "AA", "size": 1},
	}}
	w, _ := zw.Create("maccy-package.json")
	b, _ := json.Marshal(oldList)
	w.Write(b)
	zw.Close()
	oldZip := filepath.Join(t.TempDir(), "maccy-1.0.10-win-x64.zip")
	if err := os.WriteFile(oldZip, old.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	cmd := exec.Command("python3", tool, "package", "--version", "1.0.11", "--publish-dir", pub, "--out-dir", out, "--legacy-from", oldZip)
	res, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, res)
	}
	var made struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal(res, &made); err != nil {
		t.Fatalf("%v\n%s", err, res)
	}
	if filepath.Base(made.Path) != "maccy-1.0.11-win-x64.zip" {
		t.Fatalf("name: %s", made.Path)
	}
	if got, _ := SHA256File(made.Path); got != made.SHA256 {
		t.Errorf("reported sha %s, file %s", made.SHA256, got)
	}

	// The client's checks, in full.
	info := Info{Version: "1.0.11", PackageRuntime: "win-x64"}
	prep, err := PreparePackage(info, made.Path, t.TempDir())
	if err != nil {
		t.Fatalf("the client refuses the package: %v", err)
	}
	if len(prep.Manifest.Files) != 3 {
		t.Errorf("files: %+v", prep.Manifest.Files)
	}
	// The old program's files are listed for removal, but not what is still
	// needed (the same name), and never the user's data.
	got := strings.Join(prep.Manifest.LegacyFiles, ",")
	if got != "Avalonia.Base.dll,maccy.dll,runtimes/win-x64/native/libSkiaSharp.dll" {
		t.Errorf("legacy files: %s", got)
	}

	// What 1.0.10's PowerShell does with the list: it reads path and sha256
	// of each file. Names must be relative with forward slashes, and the
	// checksum upper case, as release.py wrote them.
	for _, f := range prep.Manifest.Files {
		if strings.Contains(f.Path, "\\") || strings.HasPrefix(f.Path, "/") || f.SHA256 != strings.ToUpper(f.SHA256) {
			t.Errorf("file entry %+v is not in the form 1.0.10 reads", f)
		}
	}
}

func TestReleaseToolRefusesUserData(t *testing.T) {
	tool := releaseTool(t)
	pub := t.TempDir()
	write(t, filepath.Join(pub, "maccy.exe"), "EXE")
	write(t, filepath.Join(pub, "settings.json"), "{}")
	res, err := exec.Command("python3", tool, "package", "--version", "1.0.11", "--publish-dir", pub, "--out-dir", t.TempDir()).CombinedOutput()
	if err == nil || !strings.Contains(string(res), "user data") {
		t.Fatalf("a publish folder with settings.json was packaged: %v\n%s", err, res)
	}
	empty := t.TempDir()
	write(t, filepath.Join(empty, "other.dll"), "x")
	if res, err := exec.Command("python3", tool, "package", "--version", "1.0.11", "--publish-dir", empty, "--out-dir", t.TempDir()).CombinedOutput(); err == nil || !strings.Contains(string(res), "no maccy.exe") {
		t.Fatalf("a folder without the program was packaged: %v\n%s", err, res)
	}
}

func TestReleaseToolManifestIsReadByTheClient(t *testing.T) {
	tool := releaseTool(t)
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	// The manifest of 1.0.10, as it is in the repository.
	orig, err := os.ReadFile("../../../docs/updates/manifest.json")
	if err != nil {
		t.Skip("no manifest in the repo")
	}
	write(t, manifest, string(orig))
	// The folder of the tool goes in as an argument, not as text in the script.
	script := `
import sys, json
from pathlib import Path
sys.path.insert(0, sys.argv[2])
import release
release._update_manifest(Path(sys.argv[1]), version="1.0.11", mandatory=False, notes="line one\nline two",
    base_url="https://gitee.com/Achordchan/maccy/releases/download", sha256="AB"*32, size=1234,
    package_sha256="CD"*32, package_size=5678, package_runtime="win-x64")
`
	if res, err := exec.Command("python3", "-c", script, manifest, filepath.Dir(tool)).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, res)
	}
	data, _ := os.ReadFile(manifest)
	m, err := ParseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if m.AppID != "maccy" || m.Latest.Version != "1.0.11" || m.Latest.Notes != "line one\nline two" ||
		m.Latest.Installer.URL != "https://gitee.com/Achordchan/maccy/releases/download/v1.0.11/maccy-1.0.11-setup.exe" ||
		m.Latest.Package == nil || m.Latest.Package.URL != "https://gitee.com/Achordchan/maccy/releases/download/v1.0.11/maccy-1.0.11-win-x64.zip" ||
		m.Latest.Package.Kind != "zip" || m.Latest.Package.Runtime != "win-x64" || m.Latest.Package.Size != 5678 {
		t.Fatalf("%+v", m.Latest)
	}
	// The client of 1.0.10, running, sees 1.0.11 as newer.
	c := &Client{Current: "1.0.10", Runtime: "win-x64"}
	if r := c.decide(m); r.Status != UpdateAvailable || !r.Info.HasSupportedPackage("win-x64") {
		t.Fatalf("%+v", r)
	}
	// And the keys are the ones it reads.
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	latest := raw["latest"].(map[string]any)
	for _, k := range []string{"version", "mandatory", "notes", "publishedAt", "installer", "package"} {
		if _, ok := latest[k]; !ok {
			t.Errorf("manifest lacks %q", k)
		}
	}
}

func TestReleaseToolNeedsADownloadAddress(t *testing.T) {
	tool := releaseTool(t)
	manifest := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(manifest, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := exec.Command("python3", tool, "release", "--version", "1.0.11", "--manifest", manifest).CombinedOutput()
	if err == nil || !strings.Contains(string(res), "--base-url") {
		t.Fatalf("a release with no place to be downloaded from must stop before building: %v %s", err, res)
	}
}

func TestInstallerDeletesOnlyNamedLegacyFiles(t *testing.T) {
	tool := releaseTool(t)
	dir := t.TempDir()
	pub := filepath.Join(dir, "pub")
	write(t, filepath.Join(pub, "maccy.exe"), "NEW")
	old := filepath.Join(dir, "old.zip")
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.Create("maccy-package.json")
	_, _ = w.Write([]byte(`{"appId":"maccy","version":"1.0.10","runtime":"win-x64","files":[{"path":"maccy.exe"},{"path":"maccy.dll"},{"path":"runtimes/win-x64/native/x.dll"},{"path":"settings.json"},{"path":"../history.json"},{"path":"*.dll"},{"path":"{userdocs}/x.txt"},{"path":"C:/Windows/x.dll"},{"path":"a.dll."}]}`))
	_ = zw.Close()
	if err := os.WriteFile(old, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "legacy-delete.iss")
	script := `
import sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import release_go
release_go.write_legacy_delete(Path(sys.argv[2]), sys.argv[3], Path(sys.argv[4]))
`
	if res, err := exec.Command("python3", "-c", script, filepath.Dir(tool), pub, old, out).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, res)
	}
	got, _ := os.ReadFile(out)
	text := string(got)
	for _, want := range []string{`DeleteLegacy('maccy.dll');`, `DeleteLegacy('runtimes\win-x64\native\x.dll');`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s in:\n%s", want, text)
		}
	}
	for _, bad := range []string{"*.dll", "history.json", "{userdocs}", "C:", "a.dll.", "settings.json", "maccy.exe"} {
		if strings.Contains(text, bad) {
			t.Errorf("%s must not be deleted:\n%s", bad, text)
		}
	}
}

func TestPackageIntoTheSameFolderTwiceDoesNotContainItself(t *testing.T) {
	tool := releaseTool(t)
	pub := t.TempDir()
	write(t, filepath.Join(pub, "maccy.exe"), "EXE")
	for i := 0; i < 2; i++ {
		if res, err := exec.Command("python3", tool, "package", "--version", "1.0.11", "--publish-dir", pub, "--out-dir", pub).CombinedOutput(); err != nil {
			t.Fatalf("run %d: %v\n%s", i, err, res)
		}
	}
	zr, err := zip.OpenReader(filepath.Join(pub, "maccy-1.0.11-win-x64.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".zip") {
			t.Fatalf("the package holds %s", f.Name)
		}
	}
}

func TestPackagesOfDifferentVersionsInOneFolderDoNotNest(t *testing.T) {
	tool := releaseTool(t)
	pub := t.TempDir()
	write(t, filepath.Join(pub, "maccy.exe"), "EXE")
	for _, v := range []string{"1.0.11", "1.0.12"} {
		if res, err := exec.Command("python3", tool, "package", "--version", v, "--publish-dir", pub, "--out-dir", pub).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", v, err, res)
		}
	}
	zr, err := zip.OpenReader(filepath.Join(pub, "maccy-1.0.12-win-x64.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".zip") {
			t.Fatalf("the package of 1.0.12 holds %s", f.Name)
		}
	}
}

func TestBuildDoesNotClearAFolderThatIsNotItsOwn(t *testing.T) {
	tool := releaseTool(t)
	repo := filepath.Dir(filepath.Dir(tool))
	script := `
import sys
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import release_go
try:
    release_go.check_disposable(Path(sys.argv[2]))
except RuntimeError as e:
    print("REFUSED")
else:
    print("ALLOWED")
`
	check := func(dir string) string {
		out, err := exec.Command("python3", "-c", script, filepath.Dir(tool), dir).CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if check(repo) != "REFUSED" || check(filepath.Dir(repo)) != "REFUSED" {
		t.Error("the checkout, or a folder around it, was offered for clearing")
	}
	other := t.TempDir()
	write(t, filepath.Join(other, "notes.txt"), "mine")
	if check(other) != "REFUSED" {
		t.Error("a folder of somebody's files without a build in it was offered for clearing")
	}
	inst := t.TempDir()
	write(t, filepath.Join(inst, "maccy.exe"), "EXE")
	write(t, filepath.Join(inst, "history.json"), "[]")
	if check(inst) != "REFUSED" {
		t.Error("an installation with the user's data was offered for clearing")
	}
	mixed := t.TempDir()
	write(t, filepath.Join(mixed, "maccy.exe"), "EXE")
	write(t, filepath.Join(mixed, "notes.txt"), "mine")
	if check(mixed) != "REFUSED" {
		t.Error("a build next to somebody's file was offered for clearing")
	}
	build := t.TempDir()
	write(t, filepath.Join(build, "maccy.exe"), "EXE")
	write(t, filepath.Join(build, "mygo-sqlite3.dll"), "DLL")
	if check(build) != "ALLOWED" || check(filepath.Join(build, "none")) != "ALLOWED" {
		t.Error("the leftover of a build, or an empty folder, should be fine")
	}
}
