package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- helpers

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return strings.ToUpper(hex.EncodeToString(h[:]))
}

// zipOf makes a package zip from files, with a maccy-package.json that
// lists them (what tools/release.py does), unless raw entries say otherwise.
func zipOf(t *testing.T, version, runtime string, files map[string]string, mutate func(*PackageManifest)) []byte {
	t.Helper()
	pm := PackageManifest{AppID: "maccy", Version: version, Runtime: runtime}
	for name, content := range files {
		pm.Files = append(pm.Files, PackageFile{Path: name, SHA256: sha([]byte(content)), Size: int64(len(content))})
	}
	// a stable order
	for i := range pm.Files {
		for j := i + 1; j < len(pm.Files); j++ {
			if pm.Files[j].Path < pm.Files[i].Path {
				pm.Files[i], pm.Files[j] = pm.Files[j], pm.Files[i]
			}
		}
	}
	if mutate != nil {
		mutate(&pm)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	w, _ := zw.Create("maccy-package.json")
	b, _ := json.MarshalIndent(pm, "", "  ")
	w.Write(b)
	zw.Close()
	return buf.Bytes()
}

type server struct {
	*httptest.Server
	manifest atomic.Value // []byte
	files    map[string][]byte
	hits     atomic.Int32
	fail     atomic.Int32 // answer 503 this many times first
}

func newServer(t *testing.T) *server {
	s := &server{files: map[string][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if s.fail.Add(-1) >= 0 {
			http.Error(w, "busy", 503)
			return
		}
		if r.URL.Path == "/manifest.json" {
			m, _ := s.manifest.Load().([]byte)
			if m == nil {
				http.NotFound(w, r)
				return
			}
			w.Write(m)
			return
		}
		if b, ok := s.files[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) setManifest(t *testing.T, m Manifest) {
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	s.manifest.Store(b)
}

func fastClient(s *server, current string) *Client {
	return &Client{
		HTTP: s.Client(), ManifestURL: s.URL + "/manifest.json", Current: current, Runtime: "win-x64",
		ManifestTimeout: 300 * time.Millisecond,
		ManifestPause:   func(int) time.Duration { return time.Millisecond },
		DownloadPause:   func(int) time.Duration { return time.Millisecond },
	}
}

func release(v string) Manifest {
	return Manifest{AppID: "maccy", Latest: Release{Version: v, Notes: "notes", Installer: Installer{URL: "http://x/setup.exe"}}}
}

// ---- versions

func TestNormalizeVersion(t *testing.T) {
	for in, want := range map[string]string{
		"1.0.11": "1.0.11", "v1.0.11": "1.0.11", " V2.5 ": "2.5", "1.0.11+abc123": "1.0.11", "1.0.10.0": "1.0.10.0",
		"1.2.3.4.5": "1.2.3.4", "release": "", "": "", "  ": "", "v": "", "x1.2": "1.2", "1": "1",
	} {
		if got := NormalizeVersion(in); got != want {
			t.Errorf("NormalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVersionComparisonIsDotNets(t *testing.T) {
	cmp := func(a, b string) int {
		va, ok1 := ParseVersion(a)
		vb, ok2 := ParseVersion(b)
		if !ok1 || !ok2 {
			t.Fatalf("parse %q %q: %v %v", a, b, ok1, ok2)
		}
		return va.Compare(vb)
	}
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.0.11", "1.0.10", 1}, {"1.0.10", "1.0.10", 0}, {"1.0.9", "1.0.10", -1}, {"1.1", "1.0.99", 1},
		{"2", "1.9.9", 1}, {"1.0", "1.0.0", -1}, // System.Version: a missing number is -1
		{"1.0.11", "1.0.11.0", -1}, {"1.0.11.1", "1.0.11.0", 1}, {"10.0", "9.99.99", 1},
	} {
		if got := cmp(c.a, c.b); got != c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	for _, bad := range []string{"", "a.b", "1..2", "-1.0", "1.2.3.4.5"} {
		if _, ok := ParseVersion(bad); ok {
			t.Errorf("ParseVersion(%q) accepted", bad)
		}
	}
}

// ---- the C# lists

func TestProtectedListsMatchCSharp(t *testing.T) {
	src, err := os.ReadFile("../../../klip/Services/UpdateService.cs")
	if err != nil {
		t.Skip("C# source not here")
	}
	list := func(name string) []string {
		re := regexp.MustCompile(`(?s)` + name + `\s*=\s*\[(.*?)\];`)
		m := re.FindSubmatch(src)
		if m == nil {
			t.Fatalf("%s not found in UpdateService.cs", name)
		}
		var out []string
		for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(m[1], -1) {
			out = append(out, string(q[1]))
		}
		return out
	}
	if got, want := strings.Join(ProtectedRoots, ","), strings.Join(list("ProtectedDataRoots"), ","); got != want {
		t.Errorf("protected roots %s, C# %s", got, want)
	}
	if got, want := strings.Join(ProtectedFiles, ","), strings.Join(list("ProtectedDataFiles"), ","); got != want {
		t.Errorf("protected files %s, C# %s", got, want)
	}
	for _, s := range []string{`\d+(?:\.\d+){0,3}`, `"maccy.exe"`, `"maccy"`, "TimeSpan.FromSeconds(8)", "700 * attempt", "maxAttempts: 3"} {
		if !strings.Contains(string(src), s) {
			t.Errorf("UpdateService.cs no longer contains %q: the port needs a look", s)
		}
	}
}

func TestIsProtected(t *testing.T) {
	for p, want := range map[string]bool{
		"settings.json": true, "Settings.JSON": true, "history.json": true, "images/a.png": true, `images\a.png`: true,
		"FILES/1/x": true, "files": true, "blobs/aa/bb/x": true, "sync/x": true, "updates/y": true, "/images/x": true,
		"maccy.exe": false, "imagesx/y": false, "mygo-sqlite3.dll": false, "lib/settings.json": false, "filesystem.dll": false,
	} {
		if got := IsProtected(p); got != want {
			t.Errorf("IsProtected(%q) = %v, want %v", p, got, want)
		}
	}
}

// ---- Check

func TestCheckDecisions(t *testing.T) {
	s := newServer(t)
	c := fastClient(s, "1.0.10")

	s.setManifest(t, release("1.0.11"))
	if r := c.Check(context.Background(), nil); r.Status != UpdateAvailable || r.Info.Version != "1.0.11" || r.Info.Notes != "notes" {
		t.Fatalf("%+v", r)
	}
	for _, cur := range []string{"1.0.11", "1.0.12", "v1.0.11+abc"} {
		c.Current = cur
		if r := c.Check(context.Background(), nil); r.Status != UpToDate {
			t.Errorf("current %s: %+v", cur, r)
		}
	}
	c.Current = "1.0.10"

	for name, tc := range map[string]struct {
		m    Manifest
		want string
	}{
		"no version":   {Manifest{AppID: AppID, Latest: Release{Installer: Installer{URL: "u"}}}, "manifest missing version"},
		"junk version": {Manifest{AppID: AppID, Latest: Release{Version: "abc", Installer: Installer{URL: "u"}}}, "manifest missing version"},
		"no url":       {release2("1.0.11", ""), "manifest missing update url"},
	} {
		s.setManifest(t, tc.m)
		if r := c.Check(context.Background(), nil); r.Status != Failed || r.Error != tc.want {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// A package address alone is enough.
	m := release2("1.0.11", "")
	m.Latest.Package = &Package{Kind: "zip", Runtime: "win-x64", URL: "http://x/p.zip", SHA256: "AB"}
	s.setManifest(t, m)
	if r := c.Check(context.Background(), nil); r.Status != UpdateAvailable || !r.Info.HasSupportedPackage("win-x64") {
		t.Errorf("package only: %+v", r)
	}
	c.Current = "garbage"
	if r := c.Check(context.Background(), nil); r.Status != Failed || r.Error != "version parse failed (latest=1.0.11, current=)" {
		t.Errorf("bad current: %+v", r)
	}
}

func release2(v, installerURL string) Manifest {
	m := release(v)
	m.Latest.Installer.URL = installerURL
	return m
}

func TestCheckPicksThePackageOfTheRuntime(t *testing.T) {
	s := newServer(t)
	m := release("1.0.11")
	m.Latest.Package = &Package{Kind: "zip", Runtime: "win-x64", URL: "http://x/win.zip", SHA256: "W", Size: 1}
	m.Latest.Packages = []Package{
		{Kind: "zip", Runtime: "win-x64", URL: "http://x/win.zip", SHA256: "W", Size: 1},
		{Kind: "zip", Runtime: "linux-x64", URL: "http://x/linux.zip", SHA256: "L", Size: 2},
		{Kind: "zip", Runtime: "osx-arm64", URL: "http://x/mac.zip", SHA256: "M", Size: 3},
	}
	s.setManifest(t, m)
	for rt, want := range map[string]string{"win-x64": "win.zip", "linux-x64": "linux.zip", "osx-arm64": "mac.zip", "win-arm64": "win.zip"} {
		c := fastClient(s, "1.0.10")
		c.Runtime = rt
		r := c.Check(context.Background(), nil)
		if r.Status != UpdateAvailable || !strings.HasSuffix(r.Info.PackageURL, want) {
			t.Errorf("%s: %+v", rt, r.Info)
		}
		// win-arm64 only has the old field, which names win-x64: not supported.
		if rt == "win-arm64" && r.Info.HasSupportedPackage(rt) {
			t.Errorf("a win-x64 package must not serve win-arm64")
		}
	}
}

func TestCheckRetriesLikeCSharp(t *testing.T) {
	s := newServer(t)
	s.setManifest(t, release("1.0.11"))
	c := fastClient(s, "1.0.10")
	var said []string
	s.fail.Store(2)
	r := c.Check(context.Background(), func(m string) { said = append(said, m) })
	if r.Status != UpdateAvailable || s.hits.Load() != 3 {
		t.Fatalf("two 503s then success: %+v after %d hits", r, s.hits.Load())
	}
	if len(said) != 3 || said[0] != "正在检查是否有新版本..." || said[1] != "更新服务器响应较慢，正在第 2 次重试..." || said[2] != "更新服务器响应较慢，正在第 3 次重试..." {
		t.Errorf("progress texts: %q", said)
	}

	s.hits.Store(0)
	s.fail.Store(5)
	if r := c.Check(context.Background(), nil); r.Status != Failed || s.hits.Load() != 3 ||
		r.Error != "Response status code does not indicate success: 503 (Service Unavailable)." {
		t.Fatalf("three 503s: %+v after %d hits", r, s.hits.Load())
	}

	// 404 is not worth a second try.
	s.hits.Store(0)
	s.fail.Store(0)
	s.manifest.Store([]byte(nil))
	if r := c.Check(context.Background(), nil); r.Status != Failed || s.hits.Load() != 1 || !strings.Contains(r.Error, "404") {
		t.Fatalf("404: %+v after %d hits", r, s.hits.Load())
	}

	// A body that is not JSON.
	s.manifest.Store([]byte("<html>"))
	s.hits.Store(0)
	if r := c.Check(context.Background(), nil); r.Status != Failed || r.Error == "" {
		t.Fatalf("html: %+v", r)
	}
}

func TestCheckTimesOut(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	c := &Client{HTTP: slow.Client(), ManifestURL: slow.URL, Current: "1.0.10", ManifestTimeout: 40 * time.Millisecond,
		ManifestPause: func(int) time.Duration { return time.Millisecond }}
	start := time.Now()
	r := c.Check(context.Background(), nil)
	if r.Status != Failed || r.Error != "检查更新超时" || time.Since(start) > time.Second {
		t.Fatalf("%+v after %v", r, time.Since(start))
	}
	// Cancelling the context ends it at once.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	c.ManifestTimeout = 5 * time.Second
	start = time.Now()
	if r := c.Check(ctx, nil); r.Status != Failed || time.Since(start) > time.Second {
		t.Fatalf("cancel: %+v after %v", r, time.Since(start))
	}
}

func TestManifestKeysAreCaseInsensitive(t *testing.T) {
	m, err := ParseManifest([]byte(`{"AppId":"maccy","Latest":{"Version":"1.0.11","Mandatory":true,"Installer":{"URL":"u","SHA256":"x","Size":5},"Package":{"Kind":"zip","Runtime":"win-x64","Url":"p","Sha256":"y","Size":7}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Latest.Version != "1.0.11" || !m.Latest.Mandatory || m.Latest.Installer.Size != 5 || m.Latest.Package == nil || m.Latest.Package.SHA256 != "y" {
		t.Fatalf("%+v", m)
	}
}

func TestTheManifestInTheRepoIsRead(t *testing.T) {
	data, err := os.ReadFile("../../../docs/updates/manifest.json")
	if err != nil {
		t.Skip("no manifest in the repo")
	}
	m, err := ParseManifest(data)
	if err != nil || m.AppID != "maccy" || m.Latest.Version == "" || m.Latest.Installer.URL == "" {
		t.Fatalf("%+v %v", m, err)
	}
	if m.Latest.Package != nil && (m.Latest.Package.Kind != "zip" || m.Latest.Package.Runtime != "win-x64" || m.Latest.Package.SHA256 == "") {
		t.Fatalf("package: %+v", m.Latest.Package)
	}
}

// ---- download and package

func pkgInfo(version string, zipBytes []byte, base string) Info {
	return Info{Version: version, PackageKind: "zip", PackageRuntime: "win-x64", PackageURL: base + "/p.zip", PackageSHA256: sha(zipBytes), PackageSize: int64(len(zipBytes)),
		InstallerURL: base + "/setup.exe"}
}

func TestDownloadAndPreparePackage(t *testing.T) {
	s := newServer(t)
	z := zipOf(t, "1.0.11", "win-x64", map[string]string{"maccy.exe": "EXE", "mygo-sqlite3.dll": "DLL", "res/a.txt": "A"}, nil)
	s.files["/p.zip"] = z
	c := fastClient(s, "1.0.10")
	root := t.TempDir()
	var last int64
	p, err := c.DownloadPackage(context.Background(), pkgInfo("1.0.11", z, s.URL), root, func(r, tot int64) { last = r })
	if err != nil {
		t.Fatal(err)
	}
	if last != int64(len(z)) {
		t.Errorf("progress ended at %d of %d", last, len(z))
	}
	if b, _ := os.ReadFile(filepath.Join(p.StagingPath, "res", "a.txt")); string(b) != "A" {
		t.Errorf("not unpacked")
	}
	if _, err := os.Stat(filepath.Join(root, "packages", "maccy-1.0.11-win-x64.zip")); err != nil {
		t.Errorf("zip not kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "packages", "maccy-1.0.11-win-x64.zip.download")); err == nil {
		t.Errorf("temporary file left")
	}
}

func TestDownloadChecks(t *testing.T) {
	s := newServer(t)
	z := zipOf(t, "1.0.11", "win-x64", map[string]string{"maccy.exe": "EXE"}, nil)
	s.files["/p.zip"] = z
	c := fastClient(s, "1.0.10")
	root := t.TempDir()
	info := pkgInfo("1.0.11", z, s.URL)

	bad := info
	bad.PackageSHA256 = strings.Repeat("0", 64)
	if _, err := c.DownloadPackage(context.Background(), bad, root, nil); err == nil || err.Error() != "sha256 mismatch" {
		t.Errorf("wrong checksum: %v", err)
	}
	bad = info
	bad.PackageSize++
	if _, err := c.DownloadPackage(context.Background(), bad, root, nil); err == nil || err.Error() != "file size mismatch" {
		t.Errorf("wrong size: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "packages", "*")); len(left) != 0 {
		t.Errorf("failed downloads left %v", left)
	}

	s.hits.Store(0)
	s.fail.Store(2)
	if _, err := c.DownloadPackage(context.Background(), info, root, nil); err != nil || s.hits.Load() != 3 {
		t.Errorf("503 twice: %v after %d hits", err, s.hits.Load())
	}
	s.hits.Store(0)
	s.fail.Store(0)
	gone := info
	gone.PackageURL = s.URL + "/nope.zip"
	if _, err := c.DownloadPackage(context.Background(), gone, root, nil); err == nil || s.hits.Load() != 1 {
		t.Errorf("404: %v after %d hits", err, s.hits.Load())
	}
	other := info
	other.PackageRuntime = "linux-x64"
	if _, err := c.DownloadPackage(context.Background(), other, root, nil); err == nil || err.Error() != "manifest package is not supported by this client" {
		t.Errorf("other runtime: %v", err)
	}
}

func prepare(t *testing.T, version string, z []byte) error {
	t.Helper()
	zp := filepath.Join(t.TempDir(), "p.zip")
	if err := os.WriteFile(zp, z, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := PreparePackage(Info{Version: version, PackageRuntime: "win-x64"}, zp, t.TempDir())
	return err
}

func TestPackageValidationRules(t *testing.T) {
	good := map[string]string{"maccy.exe": "EXE", "mygo-sqlite3.dll": "DLL"}
	if err := prepare(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", good, nil)); err != nil {
		t.Fatalf("a good package: %v", err)
	}
	cases := map[string]struct {
		z    []byte
		want string
	}{
		"app id":      {zipOf(t, "1.0.11", "win-x64", good, func(m *PackageManifest) { m.AppID = "other" }), "package app id mismatch"},
		"version":     {zipOf(t, "1.0.12", "win-x64", good, nil), "package version mismatch"},
		"runtime":     {zipOf(t, "1.0.11", "linux-x64", good, nil), "package runtime mismatch"},
		"no files":    {zipOf(t, "1.0.11", "win-x64", good, func(m *PackageManifest) { m.Files = nil }), "package file list is empty"},
		"no exe":      {zipOf(t, "1.0.11", "win-x64", map[string]string{"a.dll": "x"}, nil), "package missing maccy.exe"},
		"protected":   {zipOf(t, "1.0.11", "win-x64", map[string]string{"maccy.exe": "x", "settings.json": "{}"}, nil), "package contains protected data path: settings.json"},
		"protected 2": {zipOf(t, "1.0.11", "win-x64", map[string]string{"maccy.exe": "x", "images/a.png": "x"}, nil), "package contains protected data path: images/a.png"},
		"missing": {zipOf(t, "1.0.11", "win-x64", good, func(m *PackageManifest) {
			m.Files = append(m.Files, PackageFile{Path: "ghost.dll", SHA256: "AA", Size: 1})
		}), "package file missing: ghost.dll"},
		"size":   {zipOf(t, "1.0.11", "win-x64", good, func(m *PackageManifest) { m.Files[0].Size++ }), "package file size mismatch: " + "maccy.exe"},
		"no sha": {zipOf(t, "1.0.11", "win-x64", good, func(m *PackageManifest) { m.Files[0].SHA256 = " " }), "package file sha256 missing: maccy.exe"},
		"sha":    {zipOf(t, "1.0.11", "win-x64", good, func(m *PackageManifest) { m.Files[0].SHA256 = strings.Repeat("0", 64) }), "package file sha256 mismatch: maccy.exe"},
		"escape": {zipOf(t, "1.0.11", "win-x64", good, func(m *PackageManifest) { m.Files[0].Path = "../maccy.exe" }), "package path is unsafe: ../maccy.exe"},
		"rooted": {zipOf(t, "1.0.11", "win-x64", good, func(m *PackageManifest) { m.Files[0].Path = "/etc/passwd" }), ""},
	}
	for name, c := range cases {
		err := prepare(t, "1.0.11", c.z)
		if err == nil || (c.want != "" && err.Error() != c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}

	// The file list is read when its names have another case or slashes.
	mixed := zipOf(t, "1.0.11", "win-x64", map[string]string{"maccy.exe": "EXE", "sub/x.dll": "X"}, func(m *PackageManifest) {
		for i := range m.Files {
			m.Files[i].Path = strings.ReplaceAll(m.Files[i].Path, "/", `\`)
			m.Files[i].SHA256 = strings.ToLower(m.Files[i].SHA256)
		}
		m.Files[0].Path = strings.ToUpper(m.Files[0].Path[:1]) + m.Files[0].Path[1:]
	})
	if err := prepare(t, "1.0.11", mixed); err != nil && !strings.Contains(err.Error(), "missing") {
		t.Errorf("backslashes: %v", err)
	}
}

func TestZipSlipIsRefused(t *testing.T) {
	for _, name := range []string{"../evil.dll", "a/../../evil.dll", "/abs/evil.dll"} {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create(name)
		w.Write([]byte("x"))
		zw.Close()
		if err := prepare(t, "1.0.11", buf.Bytes()); err == nil {
			t.Errorf("%q was unpacked", name)
		}
	}
}

func TestInstallerDownload(t *testing.T) {
	s := newServer(t)
	body := []byte("installer-bytes")
	s.files["/setup.exe"] = body
	c := fastClient(s, "1.0.10")
	info := Info{Version: "1.0.11", InstallerURL: s.URL + "/setup.exe", InstallerSHA256: sha(body), InstallerSize: int64(len(body))}
	p, err := c.DownloadInstaller(context.Background(), info, t.TempDir(), nil)
	if err != nil || filepath.Base(p) != "maccy-1.0.11-setup.exe" {
		t.Fatalf("%q %v", p, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "installer-bytes" {
		t.Fatal("content")
	}
	info.InstallerSHA256 = "00"
	if _, err := c.DownloadInstaller(context.Background(), info, t.TempDir(), nil); err == nil {
		t.Fatal("a wrong checksum was accepted")
	}
	if _, err := c.DownloadInstaller(context.Background(), Info{}, t.TempDir(), nil); err == nil || err.Error() != "manifest missing installer url" {
		t.Fatalf("%v", err)
	}
}

var _ = fmt.Sprint

func TestDuplicateEntriesAreRefused(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i < 2; i++ {
		w, _ := zw.Create("maccy.exe")
		w.Write([]byte("x"))
	}
	zw.Close()
	zp := filepath.Join(t.TempDir(), "p.zip")
	_ = os.WriteFile(zp, buf.Bytes(), 0o644)
	if _, err := PreparePackage(Info{Version: "1.0.11", PackageRuntime: "win-x64"}, zp, t.TempDir()); err == nil {
		t.Fatal("a name twice in one zip was unpacked (the second would replace the first)")
	}
}

func TestPackageListingTheSameFileTwiceIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "maccy.exe"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, _ := SHA256File(filepath.Join(dir, "maccy.exe"))
	m := &PackageManifest{AppID: "maccy", Version: "1.0.11", Runtime: "win-x64",
		Files: []PackageFile{{Path: "maccy.exe", SHA256: sum, Size: 1}, {Path: "MACCY.EXE", SHA256: sum, Size: 1}}}
	err := ValidatePackage(Info{Version: "1.0.11", PackageRuntime: "win-x64"}, dir, m)
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("%v", err)
	}
}

func TestPackageManifestWithABOMIsRead(t *testing.T) {
	data := append([]byte("\xef\xbb\xbf"), []byte(`{"appId":"maccy","version":"1.0.11","runtime":"win-x64","files":[{"path":"maccy.exe"}]}`)...)
	m, err := ParsePackageManifest(data)
	if err != nil || len(m.Files) != 1 {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestWindowsAliasesOfProtectedFilesAreRefused(t *testing.T) {
	for _, p := range []string{"settings.json.", "settings.json:stream", "images /a.png", "images/a.png.", "SETTIN~1.JSO", "dir/bad|name", "con?"} {
		if _, err := NormalizePath(p); err == nil {
			t.Errorf("%q must be refused", p)
		}
	}
	for _, p := range []string{"maccy.exe", "mygo-sqlite3.dll", "sub/dir/file.v2.txt"} {
		if _, err := NormalizePath(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
}

func TestPathThroughALinkIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need rights here")
	}
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "plugins")); err != nil {
		t.Skip("no symlinks")
	}
	if _, err := insideRoot(root, filepath.Join("plugins", "x.dll")); err == nil {
		t.Fatal("a write below a link would land outside the folder")
	}
	if _, err := insideRoot(root, filepath.Join("fresh", "x.dll")); err != nil {
		t.Fatalf("a plain new path: %v", err)
	}
}

func TestPackageWithAnUnsafeLegacyNameIsRefusedBeforeTheHelperRuns(t *testing.T) {
	files := map[string]string{"maccy.exe": "EXE"}
	for _, bad := range []string{"../old.dll", "..\\old.dll", "/etc/x", "C:/x.dll"} {
		z := zipOf(t, "1.0.11", "win-x64", files, func(m *PackageManifest) { m.LegacyFiles = []string{bad} })
		if err := prepare(t, "1.0.11", z); err == nil {
			t.Errorf("legacy name %q was accepted", bad)
		}
	}
	z := zipOf(t, "1.0.11", "win-x64", files, func(m *PackageManifest) { m.LegacyFiles = []string{"maccy.dll", "runtimes/win-x64/x.dll"} })
	if err := prepare(t, "1.0.11", z); err != nil {
		t.Errorf("ordinary legacy names: %v", err)
	}
}

func TestFailedPreparationLeavesNothingStaged(t *testing.T) {
	zp := filepath.Join(t.TempDir(), "p.zip")
	// valid zip, but no maccy.exe in the list: validation fails after unpacking
	z := zipOf(t, "1.0.11", "win-x64", map[string]string{"other.dll": "X"}, nil)
	if err := os.WriteFile(zp, z, 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := PreparePackage(Info{Version: "1.0.11", PackageRuntime: "win-x64"}, zp, root); err == nil {
		t.Fatal("a package without the program was accepted")
	}
	if left, _ := os.ReadDir(root); len(left) != 0 {
		t.Fatalf("a failed package left %d staging folder(s)", len(left))
	}
}

func TestManualUpdateLinkIsForThisSystem(t *testing.T) {
	info := &Info{InstallerSHA256: strings.Repeat("a", 64), InstallerURL: "https://x/setup.exe", PackageKind: "zip", PackageRuntime: "linux-x64", PackageURL: "https://x/linux.zip", PackageSHA256: "AA"}
	linux := &Coordinator{Client: &Client{Runtime: "linux-x64"}}
	if got := linux.manualURL(info); got != "https://x/linux.zip" {
		t.Errorf("a Linux user was sent to %q", got)
	}
	win := &Coordinator{Client: &Client{Runtime: "win-x64"}}
	if got := win.manualURL(info); got != "https://x/setup.exe" {
		t.Errorf("a Windows user without a package for it was sent to %q", got)
	}
}
