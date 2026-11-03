package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"klip/internal/update"
)

// The program started as the helper of an update, as the running program
// starts it: a copy of the real binary, given a plan. It must put the new
// files in place, leave the user's data alone, and start the new program.
func TestHelperModeEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the new program of this test is a shell script")
	}
	bin := filepath.Join(t.TempDir(), "klip-under-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	install := t.TempDir()
	root := filepath.Join(t.TempDir(), "updates")
	marker := filepath.Join(t.TempDir(), "started")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(install, "maccy.exe"), []byte("old"), 0o755))
	must(os.WriteFile(filepath.Join(install, "settings.json"), []byte(`{"MaxItems":7}`), 0o644))
	must(os.WriteFile(filepath.Join(install, "maccy.dll"), []byte("net"), 0o644))

	newExe := "#!/bin/sh\necho started > '" + marker + "'\n"
	files := map[string]string{"maccy.exe": newExe, "mygo-sqlite3.dll": "sqlite"}
	pm := update.PackageManifest{AppID: "maccy", Version: "1.0.11", Runtime: "win-x64", LegacyFiles: []string{"maccy.dll"}}
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	for name, content := range files {
		h := sha256.Sum256([]byte(content))
		pm.Files = append(pm.Files, update.PackageFile{Path: name, SHA256: strings.ToUpper(hex.EncodeToString(h[:])), Size: int64(len(content))})
		w, _ := zw.Create(name)
		w.Write([]byte(content))
	}
	w, _ := zw.Create("maccy-package.json")
	b, _ := json.Marshal(pm)
	w.Write(b)
	zw.Close()
	zp := filepath.Join(t.TempDir(), "p.zip")
	must(os.WriteFile(zp, zbuf.Bytes(), 0o644))
	prep, err := update.PreparePackage(update.Info{Version: "1.0.11", PackageRuntime: "win-x64"}, zp, filepath.Join(root, "staging"))
	must(err)

	planPath := filepath.Join(root, "apply.json")
	must(update.WritePlan(planPath, update.Plan{
		InstallDir: install, StagingDir: prep.StagingPath, PackageManifest: prep.PackageManifest,
		InstalledManifest: filepath.Join(root, "installed-package-files.json"),
		BackupDir:         filepath.Join(root, "backups", "b"), LogPath: filepath.Join(root, "last_light_update.log"),
	}))

	out, err := exec.Command(bin, "--apply-update", planPath).CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s\nlog: %s", err, out, readFile(filepath.Join(root, "last_light_update.log")))
	}
	if got := readFile(filepath.Join(install, "mygo-sqlite3.dll")); got != "sqlite" {
		t.Errorf("sqlite dll = %q", got)
	}
	if got := readFile(filepath.Join(install, "maccy.exe")); got != newExe {
		t.Errorf("maccy.exe = %q", got)
	}
	if got := readFile(filepath.Join(install, "settings.json")); got != `{"MaxItems":7}` {
		t.Errorf("settings.json = %q", got)
	}
	if _, err := os.Stat(filepath.Join(install, "maccy.dll")); err == nil {
		t.Errorf("the legacy file stayed")
	}
	// The program was started again (the helper does not wait for it).
	deadline := time.Now().Add(30 * time.Second) // a loaded machine starts a program slowly
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the new program was not started; log: %s", readFile(filepath.Join(root, "last_light_update.log")))
}

func TestHelperModeFailureExitsNonZero(t *testing.T) {
	name := "klip-under-test"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	err := exec.Command(bin, "--apply-update", filepath.Join(t.TempDir(), "no-such-plan.json")).Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("missing plan must exit 2, got %v", err)
	}
}

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}
