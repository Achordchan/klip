package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

type rig struct {
	t             *testing.T
	install, root string
	plan          Plan
	launched      []string
	alerts        []string
	waited        []int
}

// newRig sets up an installation of "1.0.10" (with the user's data in the
// same folder, as a default install has) and an unpacked package.
func newRig(t *testing.T, newFiles map[string]string, legacy []string) *rig {
	t.Helper()
	r := &rig{t: t, install: filepath.Join(t.TempDir(), "Maccy"), root: filepath.Join(t.TempDir(), "updates")}
	write(t, filepath.Join(r.install, "maccy.exe"), "OLD EXE")
	write(t, filepath.Join(r.install, "maccy.dll"), "OLD NET DLL")
	write(t, filepath.Join(r.install, "Avalonia.Base.dll"), "OLD AVALONIA")
	write(t, filepath.Join(r.install, "settings.json"), `{"MaxItems":7}`)
	write(t, filepath.Join(r.install, "history.json"), `[]`)
	write(t, filepath.Join(r.install, "images", "a.png"), "PNG")
	write(t, filepath.Join(r.install, "updates", "keep.txt"), "KEEP")

	z := zipOf(t, "1.0.11", "win-x64", newFiles, func(m *PackageManifest) { m.LegacyFiles = legacy })
	zp := filepath.Join(t.TempDir(), "p.zip")
	if err := os.WriteFile(zp, z, 0o644); err != nil {
		t.Fatal(err)
	}
	prep, err := PreparePackage(Info{Version: "1.0.11", PackageRuntime: "win-x64"}, zp, filepath.Join(r.root, "staging"))
	if err != nil {
		t.Fatal(err)
	}
	r.plan = Plan{
		PID: 4242, InstallDir: r.install, StagingDir: prep.StagingPath, PackageManifest: prep.PackageManifest,
		InstalledManifest: filepath.Join(r.root, "installed-package-files.json"),
		BackupDir:         filepath.Join(r.root, "backups", "1.0.11-x"), LogPath: filepath.Join(r.root, "last_light_update.log"),
	}
	return r
}

func (r *rig) hooks() Hooks {
	return Hooks{
		WaitExit: func(pid int) { r.waited = append(r.waited, pid) },
		Sleep:    func(time.Duration) {},
		Launch:   func(p string) error { r.launched = append(r.launched, p); return nil },
		Alert:    func(title, msg string) { r.alerts = append(r.alerts, title+": "+msg) },
		Now:      func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) },
	}
}

var newPkg = map[string]string{"maccy.exe": "NEW EXE", "mygo-sqlite3.dll": "NEW SQLITE", "res/x.txt": "X"}

func TestApplyReplacesFilesAndKeepsData(t *testing.T) {
	r := newRig(t, newPkg, nil)
	if err := RunPlan(r.plan, r.hooks()); err != nil {
		t.Fatal(err)
	}
	if len(r.waited) != 1 || r.waited[0] != 4242 {
		t.Errorf("did not wait for the program: %v", r.waited)
	}
	for rel, want := range newPkg {
		if got := read(t, filepath.Join(r.install, filepath.FromSlash(rel))); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	// The user's data is as it was.
	for rel, want := range map[string]string{"settings.json": `{"MaxItems":7}`, "history.json": "[]", "images/a.png": "PNG", "updates/keep.txt": "KEEP"} {
		if got := read(t, filepath.Join(r.install, filepath.FromSlash(rel))); got != want {
			t.Errorf("data file %s = %q, want %q", rel, got, want)
		}
	}
	// No previous list, no legacy list: the .NET files stay (as with 1.0.10).
	if read(t, filepath.Join(r.install, "maccy.dll")) != "OLD NET DLL" {
		t.Error("a file that no list names was removed")
	}
	// The old program is in the backup.
	if got := read(t, filepath.Join(r.plan.BackupDir, "replaced", "maccy.exe")); got != "OLD EXE" {
		t.Errorf("backup = %q", got)
	}
	// The list is remembered; the program is started again.
	var m PackageManifest
	if err := json.Unmarshal([]byte(read(t, r.plan.InstalledManifest)), &m); err != nil || m.Version != "1.0.11" || len(m.Files) != 3 {
		t.Errorf("installed list: %+v %v", m, err)
	}
	if len(r.launched) != 1 || r.launched[0] != filepath.Join(r.install, "maccy.exe") || len(r.alerts) != 0 {
		t.Errorf("launched %v, alerts %v", r.launched, r.alerts)
	}
	log := read(t, r.plan.LogPath)
	if !strings.Contains(log, "[2026-06-01T12:00:00] waiting for app exit") || !strings.Contains(log, "light update applied") {
		t.Errorf("log: %s", log)
	}
}

func TestApplyIsUndoneWhenTheNewProgramWillNotStart(t *testing.T) {
	r := newRig(t, newPkg, nil)
	prev := `{"appId":"maccy","version":"1.0.10","runtime":"win-x64","files":[{"path":"maccy.exe"}]}`
	write(t, r.plan.InstalledManifest, prev)
	h := r.hooks()
	starts := 0
	h.Launch = func(p string) error {
		starts++
		if starts == 1 {
			return errors.New("blocked by the virus scanner")
		}
		r.launched = append(r.launched, p)
		return nil
	}
	if err := RunPlan(r.plan, h); err == nil {
		t.Fatal("a program that cannot start is a failed update")
	}
	if got := read(t, filepath.Join(r.install, "maccy.exe")); got != "OLD EXE" {
		t.Errorf("the old program must be back: %q", got)
	}
	if got := read(t, r.plan.InstalledManifest); got != prev {
		t.Errorf("the old file list must be back: %q", got)
	}
	if len(r.alerts) != 1 || len(r.launched) != 1 {
		t.Errorf("alerts %v, the old program started %v", r.alerts, r.launched)
	}
}

func TestApplyRemovesWhatThePreviousPackageHadAndLegacyFiles(t *testing.T) {
	r := newRig(t, newPkg, []string{"maccy.dll", "Avalonia.Base.dll", "images/a.png", "settings.json", "maccy.exe", "not-there.dll"})
	// A previous light update had left a list: two files, one still wanted.
	write(t, filepath.Join(r.install, "old-plugin.dll"), "OLD PLUGIN")
	prev := PackageManifest{AppID: "maccy", Version: "1.0.10", Runtime: "win-x64", Files: []PackageFile{
		{Path: "maccy.exe"}, {Path: "old-plugin.dll"}, {Path: "settings.json"},
	}}
	b, _ := json.Marshal(prev)
	write(t, r.plan.InstalledManifest, string(b))

	if err := RunPlan(r.plan, r.hooks()); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"old-plugin.dll", "maccy.dll", "Avalonia.Base.dll"} {
		if _, err := os.Stat(filepath.Join(r.install, gone)); err == nil {
			t.Errorf("%s should be gone", gone)
		}
	}
	// Data and the program itself are never removed, whatever a list says.
	for rel, want := range map[string]string{"settings.json": `{"MaxItems":7}`, "images/a.png": "PNG", "maccy.exe": "NEW EXE"} {
		if got := read(t, filepath.Join(r.install, filepath.FromSlash(rel))); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	// What was removed can be put back from the backup.
	if got := read(t, filepath.Join(r.plan.BackupDir, "deleted", "old-plugin.dll")); got != "OLD PLUGIN" {
		t.Errorf("deleted backup = %q", got)
	}
	// The legacy list is done: it is not kept.
	var m PackageManifest
	_ = json.Unmarshal([]byte(read(t, r.plan.InstalledManifest)), &m)
	if len(m.LegacyFiles) != 0 {
		t.Errorf("legacy list kept: %v", m.LegacyFiles)
	}
}

func TestApplyFailsBeforeTouchingAnything(t *testing.T) {
	r := newRig(t, newPkg, nil)
	// The staged file changed after it was checked.
	write(t, filepath.Join(r.plan.StagingDir, "mygo-sqlite3.dll"), "TAMPERED")
	err := RunPlan(r.plan, r.hooks())
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch before copy") {
		t.Fatalf("%v", err)
	}
	if read(t, filepath.Join(r.install, "maccy.exe")) != "OLD EXE" {
		t.Error("the program was replaced")
	}
	if _, serr := os.Stat(filepath.Join(r.install, "mygo-sqlite3.dll")); serr == nil {
		t.Error("a file was copied")
	}
	if len(r.alerts) != 1 || !strings.Contains(r.alerts[0], "轻量更新失败，已回滚") {
		t.Errorf("alerts: %v", r.alerts)
	}
	// The old program is started again.
	if len(r.launched) != 1 || r.launched[0] != filepath.Join(r.install, "maccy.exe") {
		t.Errorf("launched %v", r.launched)
	}
}

func TestApplyRollsBackAfterAFailureHalfWay(t *testing.T) {
	r := newRig(t, newPkg, nil)
	// "res" exists as a file, so res/x.txt cannot be written: the failure
	// comes after maccy.exe and the dll were already replaced.
	write(t, filepath.Join(r.install, "res"), "I AM A FILE")
	h := r.hooks()
	copyHook := h.fill().Copy
	h.Copy = func(src, dst string) error {
		if strings.HasSuffix(dst, filepath.Join("res", "x.txt")) {
			return errors.New("simulated copy failure")
		}
		return copyHook(src, dst)
	}
	h.Sleep = func(time.Duration) {} // the copy is tried five times
	err := RunPlan(r.plan, h)
	if err == nil {
		t.Fatal("the update should have failed")
	}
	if got := read(t, filepath.Join(r.install, "maccy.exe")); got != "OLD EXE" {
		t.Errorf("maccy.exe = %q after the rollback", got)
	}
	if _, serr := os.Stat(filepath.Join(r.install, "mygo-sqlite3.dll")); serr == nil {
		t.Error("a new file stayed after the rollback")
	}
	if got := read(t, filepath.Join(r.install, "res")); got != "I AM A FILE" {
		t.Errorf("res = %q", got)
	}
	if read(t, filepath.Join(r.install, "settings.json")) != `{"MaxItems":7}` {
		t.Error("data touched")
	}
	if !strings.Contains(read(t, r.plan.LogPath), "failed:") || len(r.alerts) != 1 {
		t.Errorf("log %q alerts %v", read(t, r.plan.LogPath), r.alerts)
	}
	if _, serr := os.Stat(r.plan.InstalledManifest); serr == nil {
		t.Error("a list of a package that was not installed was written")
	}
}

func TestApplyChecksTheCopies(t *testing.T) {
	r := newRig(t, newPkg, nil)
	h := r.hooks()
	h.Copy = func(src, dst string) error { // a disk that writes garbage for one file
		if err := copyFile(src, dst); err != nil {
			return err
		}
		if filepath.Base(dst) == "mygo-sqlite3.dll" {
			return os.WriteFile(dst, []byte("GARBAGE"), 0o644)
		}
		return nil
	}
	err := RunPlan(r.plan, h)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch after copy: mygo-sqlite3.dll") {
		t.Fatalf("%v", err)
	}
	if read(t, filepath.Join(r.install, "maccy.exe")) != "OLD EXE" {
		t.Error("not rolled back")
	}
	if _, serr := os.Stat(filepath.Join(r.install, "mygo-sqlite3.dll")); serr == nil {
		t.Error("the bad copy stayed")
	}
}

func TestApplyRefusesAProtectedPathAndAMissingExe(t *testing.T) {
	r := newRig(t, newPkg, nil)
	// A package list that names user data (a package that Prepare would not
	// have let through, but the helper checks again).
	b, _ := os.ReadFile(r.plan.PackageManifest)
	var m PackageManifest
	_ = json.Unmarshal(b, &m)
	m.Files = append(m.Files, PackageFile{Path: "settings.json", SHA256: sha([]byte("x")), Size: 1})
	write(t, filepath.Join(r.plan.StagingDir, "settings.json"), "x")
	nb, _ := json.Marshal(m)
	write(t, r.plan.PackageManifest, string(nb))
	if err := RunPlan(r.plan, r.hooks()); err == nil || !strings.Contains(err.Error(), "protected path in package") {
		t.Fatalf("%v", err)
	}
	if read(t, filepath.Join(r.install, "settings.json")) != `{"MaxItems":7}` {
		t.Fatal("settings.json overwritten")
	}

	r2 := newRig(t, newPkg, nil)
	m2 := PackageManifest{AppID: "maccy", Version: "1.0.11", Runtime: "win-x64", Files: []PackageFile{{Path: "mygo-sqlite3.dll", SHA256: sha([]byte("NEW SQLITE")), Size: 10}}}
	nb, _ = json.Marshal(m2)
	write(t, r2.plan.PackageManifest, string(nb))
	if err := RunPlan(r2.plan, r2.hooks()); err == nil || !strings.Contains(err.Error(), "package missing maccy.exe") {
		t.Fatalf("%v", err)
	}
}

func TestInstallerPlanStartsTheInstallerAfterExit(t *testing.T) {
	r := newRig(t, newPkg, nil)
	p := Plan{PID: 77, Installer: "/tmp/maccy-1.0.11-setup.exe", LogPath: r.plan.LogPath}
	if err := RunPlan(p, r.hooks()); err != nil {
		t.Fatal(err)
	}
	if len(r.waited) != 1 || r.waited[0] != 77 || len(r.launched) != 1 || r.launched[0] != p.Installer {
		t.Errorf("waited %v launched %v", r.waited, r.launched)
	}
	r.launched = nil
	h := r.hooks()
	starts := 0
	h.Launch = func(path string) error {
		starts++
		if path == p.Installer {
			return errors.New("blocked")
		}
		r.launched = append(r.launched, path)
		return nil
	}
	p.RestartExe = filepath.Join(r.install, "maccy.exe")
	if err := RunPlan(p, h); err == nil || len(r.alerts) != 1 {
		t.Errorf("a failing installer must be reported: %v %v", err, r.alerts)
	}
	if len(r.launched) != 1 || r.launched[0] != p.RestartExe {
		t.Errorf("Klip has exited for the installer: it must be started again, got %v", r.launched)
	}
}

func TestCleanLegacy(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "maccy.exe"), "NEW")
	write(t, filepath.Join(dir, "maccy.dll"), "OLD")
	write(t, filepath.Join(dir, "Avalonia.Base.dll"), "OLD")
	write(t, filepath.Join(dir, "runtimes", "win-x64", "native", "x.dll"), "OLD")
	write(t, filepath.Join(dir, "settings.json"), "DATA")
	manifest := filepath.Join(t.TempDir(), "installed-package-files.json")
	pm := PackageManifest{AppID: "maccy", Version: "1.0.11", Runtime: "win-x64",
		Files:       []PackageFile{{Path: "maccy.exe"}},
		LegacyFiles: []string{"maccy.dll", "Avalonia.Base.dll", "runtimes/win-x64/native/x.dll", "settings.json", "maccy.exe", "../outside.dll", "gone.dll"}}
	b, _ := json.Marshal(pm)
	write(t, manifest, string(b))

	removed, err := CleanLegacy(dir, manifest)
	if err != nil || len(removed) != 3 {
		t.Fatalf("removed %v, %v", removed, err)
	}
	for _, f := range []string{"maccy.dll", "Avalonia.Base.dll", "runtimes/win-x64/native/x.dll"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err == nil {
			t.Errorf("%s not removed", f)
		}
	}
	if read(t, filepath.Join(dir, "maccy.exe")) != "NEW" || read(t, filepath.Join(dir, "settings.json")) != "DATA" {
		t.Error("the program or the data was removed")
	}
	// Once only.
	if removed, err := CleanLegacy(dir, manifest); err != nil || len(removed) != 0 {
		t.Errorf("second time: %v %v", removed, err)
	}
	var after PackageManifest
	_ = json.Unmarshal([]byte(read(t, manifest)), &after)
	if len(after.LegacyFiles) != 0 || len(after.Files) != 1 {
		t.Errorf("manifest after: %+v", after)
	}
	// Nothing installed by a package: nothing to do.
	if removed, err := CleanLegacy(dir, filepath.Join(dir, "no-such.json")); err != nil || removed != nil {
		t.Errorf("%v %v", removed, err)
	}
}

// ---- Coordinator

type cRig struct {
	*rig
	s      *server
	c      *Coordinator
	exe    string
	mu     sync.Mutex
	states []Phase
	shut   int
	spawn  [][2]string
}

func newCoord(t *testing.T, current string) *cRig {
	t.Helper()
	s := newServer(t)
	root := filepath.Join(t.TempDir(), "updates")
	installDir := t.TempDir()
	exe := filepath.Join(installDir, "maccy.exe")
	write(t, exe, "RUNNING PROGRAM")
	r := &cRig{rig: &rig{t: t}, s: s, exe: exe}
	r.c = NewCoordinator(fastClient(s, current), root, exe, func() { r.shut++ })
	r.c.Supported = true
	r.c.PID = 99
	r.c.OnChange = func() { r.mu.Lock(); r.states = append(r.states, r.c.State().Phase); r.mu.Unlock() }
	r.c.StartHelper = func(h, p string) error { r.spawn = append(r.spawn, [2]string{h, p}); return nil }
	r.c.Now = func() time.Time { return time.Date(2026, 6, 1, 12, 30, 0, 0, time.UTC) }
	return r
}

func (r *cRig) publish(t *testing.T, version string, z []byte, withInstaller bool) {
	m := release(version)
	if withInstaller {
		r.s.files["/setup.exe"] = []byte("SETUP")
		m.Latest.Installer = Installer{URL: r.s.URL + "/setup.exe", SHA256: sha([]byte("SETUP")), Size: 5}
	} else {
		m.Latest.Installer = Installer{}
	}
	if z != nil {
		r.s.files["/p.zip"] = z
		m.Latest.Package = &Package{Kind: "zip", Runtime: "win-x64", URL: r.s.URL + "/p.zip", SHA256: sha(z), Size: int64(len(z))}
	}
	r.s.setManifest(t, m)
}

func TestCoordinatorManualCheck(t *testing.T) {
	r := newCoord(t, "1.0.11")
	r.publish(t, "1.0.11", nil, true)
	r.c.Check(context.Background())
	if st := r.c.State(); st.Phase != UpToDateNow || st.Message != "当前已经是最新版本。" || r.c.Available() != nil {
		t.Fatalf("%+v", st)
	}
	r.c.Dismiss()
	if r.c.State().Phase != Idle {
		t.Fatalf("%+v", r.c.State())
	}

	r.publish(t, "1.0.12", nil, true)
	r.c.Check(context.Background())
	if st := r.c.State(); st.Phase != Available || st.Info.Version != "1.0.12" || r.c.Available() == nil {
		t.Fatalf("%+v", st)
	}
	// Later: the version stays known.
	r.c.Dismiss()
	if r.c.State().Phase != Available {
		t.Fatalf("%+v", r.c.State())
	}

	r.s.manifest.Store([]byte(nil))
	r.c.Check(context.Background())
	if st := r.c.State(); st.Phase != Problem || !strings.HasPrefix(st.Message, "检查更新失败：") || !strings.Contains(st.Message, "404") {
		t.Fatalf("%+v", st)
	}
}

func TestCoordinatorBackgroundRefreshIsQuiet(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", nil, true)
	r.c.Refresh(context.Background())
	if st := r.c.State(); st.Phase != Available || st.Info.Version != "1.0.11" {
		t.Fatalf("%+v", st)
	}
	for _, p := range r.states {
		if p == Checking || p == Problem {
			t.Errorf("a background check showed %v", p)
		}
	}
	// A failure leaves what is known.
	r.s.manifest.Store([]byte(nil))
	r.c.Refresh(context.Background())
	if st := r.c.State(); st.Phase != Available || st.Info == nil {
		t.Fatalf("a failed refresh forgot the update: %+v", st)
	}
	// Up to date forgets it.
	r.c.Client.Current = "1.0.11"
	r.publish(t, "1.0.11", nil, true)
	r.c.Refresh(context.Background())
	if r.c.Available() != nil || r.c.State().Phase != Idle {
		t.Fatalf("%+v", r.c.State())
	}
}

func TestCoordinatorLightUpdate(t *testing.T) {
	r := newCoord(t, "1.0.10")
	z := zipOf(t, "1.0.11", "win-x64", newPkg, nil)
	r.publish(t, "1.0.11", z, true)
	r.c.Refresh(context.Background())
	r.c.Apply(context.Background())

	if r.shut != 1 || r.c.State().Phase != Applying {
		t.Fatalf("shut %d, %+v", r.shut, r.c.State())
	}
	if len(r.spawn) != 1 {
		t.Fatalf("helpers started: %v", r.spawn)
	}
	helper, planPath := r.spawn[0][0], r.spawn[0][1]
	if read(t, helper) != "RUNNING PROGRAM" {
		t.Errorf("the helper must be a copy of the running program: %q", read(t, helper))
	}
	if filepath.Dir(helper) == filepath.Dir(r.exe) {
		t.Errorf("the helper is in the install folder, where it would be replaced")
	}
	plan, err := ReadPlan(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PID != 99 || plan.InstallDir != filepath.Dir(r.exe) || plan.Installer != "" ||
		!strings.HasSuffix(plan.BackupDir, "1.0.11-20260601-123000") ||
		filepath.Base(plan.InstalledManifest) != "installed-package-files.json" || filepath.Base(plan.LogPath) != "last_light_update.log" {
		t.Errorf("plan: %+v", plan)
	}
	// And the plan is something RunPlan can carry out.
	rr := &rig{t: t}
	if err := RunPlan(plan, rr.hooks()); err != nil {
		t.Fatal(err)
	}
	if read(t, r.exe) != "NEW EXE" || read(t, filepath.Join(filepath.Dir(r.exe), "mygo-sqlite3.dll")) != "NEW SQLITE" {
		t.Error("files not replaced")
	}
}

func TestCoordinatorFallsBackToTheInstaller(t *testing.T) {
	r := newCoord(t, "1.0.10")
	z := zipOf(t, "1.0.11", "win-x64", newPkg, func(m *PackageManifest) { m.Files[0].SHA256 = strings.Repeat("0", 64) })
	r.publish(t, "1.0.11", z, true)
	r.c.Refresh(context.Background())
	r.c.Apply(context.Background())
	if r.shut != 1 || len(r.spawn) != 1 {
		t.Fatalf("shut %d helpers %v state %+v", r.shut, r.spawn, r.c.State())
	}
	plan, _ := ReadPlan(r.spawn[0][1])
	if filepath.Base(plan.Installer) != "maccy-1.0.11-setup.exe" || read(t, plan.Installer) != "SETUP" {
		t.Errorf("plan: %+v", plan)
	}
	if st := r.c.State(); !strings.Contains(st.Message, "将改用完整安装包更新") && st.Phase != Applying {
		t.Errorf("state %+v", st)
	}

	// No installer: the light failure is the answer.
	r2 := newCoord(t, "1.0.10")
	r2.publish(t, "1.0.11", z, false)
	r2.c.Refresh(context.Background())
	r2.c.Apply(context.Background())
	if r2.shut != 0 || r2.c.State().Phase != Problem || !strings.HasPrefix(r2.c.State().Message, "轻量更新失败：") {
		t.Fatalf("%d %+v", r2.shut, r2.c.State())
	}
}

func TestCoordinatorWithoutPackageUsesTheInstaller(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", nil, true)
	r.c.Refresh(context.Background())
	r.c.Apply(context.Background())
	plan, _ := ReadPlan(r.spawn[0][1])
	if r.shut != 1 || plan.Installer == "" {
		t.Fatalf("%+v %d", plan, r.shut)
	}
	// A wrong installer checksum stops it.
	r2 := newCoord(t, "1.0.10")
	r2.publish(t, "1.0.11", nil, true)
	m := release("1.0.11")
	m.Latest.Installer = Installer{URL: r2.s.URL + "/setup.exe", SHA256: "BAD", Size: 5}
	r2.s.setManifest(t, m)
	r2.c.Refresh(context.Background())
	r2.c.Apply(context.Background())
	if r2.shut != 0 || r2.c.State().Phase != Problem || r2.c.State().Message != "没有可用的安装包下载地址。" {
		t.Fatalf("%d %+v", r2.shut, r2.c.State())
	}
}

func TestCoordinatorUnsupportedSystemAndOneAtATime(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", nil, true)
	r.c.Supported = false
	r.c.Refresh(context.Background())
	r.c.Apply(context.Background())
	if st := r.c.State(); st.Phase != Problem || !strings.Contains(st.Message, "setup.exe") || r.shut != 0 {
		t.Fatalf("%+v", st)
	}
	// Nothing to apply.
	r2 := newCoord(t, "1.0.10")
	r2.c.Apply(context.Background())
	if r2.shut != 0 || r2.c.State().Phase != Idle {
		t.Fatalf("%+v", r2.c.State())
	}
	// A second Apply while one runs does nothing.
	r3 := newCoord(t, "1.0.10")
	z := zipOf(t, "1.0.11", "win-x64", newPkg, nil)
	r3.publish(t, "1.0.11", z, true)
	r3.c.Refresh(context.Background())
	r3.c.Apply(context.Background())
	r3.c.Apply(context.Background())
	if r3.shut != 1 || len(r3.spawn) != 1 {
		t.Fatalf("shut %d spawn %d", r3.shut, len(r3.spawn))
	}
}

func TestCleanOld(t *testing.T) {
	r := newCoord(t, "1.0.10")
	write(t, filepath.Join(r.c.Root, "helper", "h.exe"), "x")
	write(t, filepath.Join(r.c.Root, "staging", "1.0.11-x", "maccy.exe"), "x")
	write(t, filepath.Join(r.c.Root, "backups", "b", "maccy.exe"), "keep")
	write(t, filepath.Join(r.c.Root, "installed-package-files.json"), "{}")
	r.c.CleanOld()
	for _, gone := range []string{"helper", "staging"} {
		if _, err := os.Stat(filepath.Join(r.c.Root, gone)); err == nil {
			t.Errorf("%s left", gone)
		}
	}
	for _, kept := range []string{"backups/b/maccy.exe", "installed-package-files.json"} {
		if _, err := os.Stat(filepath.Join(r.c.Root, filepath.FromSlash(kept))); err != nil {
			t.Errorf("%s removed", kept)
		}
	}
}

func TestManualCheckWaitsForTheQuietOneAndThenAnswers(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", nil, true)
	r.c.mu.Lock()
	r.c.busy = true // the check at start is still running
	r.c.mu.Unlock()
	done := make(chan struct{})
	go func() { r.c.Check(context.Background()); close(done) }()
	time.Sleep(120 * time.Millisecond)
	if r.c.State().Phase != Checking {
		t.Fatalf("while it waits the window says so: %+v", r.c.State())
	}
	r.c.mu.Lock()
	r.c.busy = false // the quiet one ended, with nothing to say
	r.c.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the manual check never ran")
	}
	if st := r.c.State(); st.Phase != Available {
		t.Fatalf("the window must get an answer of its own: %+v", st)
	}
}

func TestQuietCheckDoesNotOverwriteAnUpdateUnderWay(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", nil, true)
	r.c.Refresh(context.Background())
	r.c.mu.Lock()
	r.c.applying = true
	r.c.state.Phase = Downloading
	r.c.mu.Unlock()
	r.c.Refresh(context.Background())
	r.c.mu.Lock()
	r.c.applying = false
	r.c.mu.Unlock()
	if r.c.State().Phase != Downloading {
		t.Fatalf("a quiet check overwrote the download: %+v", r.c.State())
	}
}

func TestStalledDownloadIsGivenUp(t *testing.T) {
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("start"))
		w.(http.Flusher).Flush()
		<-stop // and then nothing more
	}))
	defer srv.Close()
	defer close(stop)
	c := &Client{DownloadIdle: 150 * time.Millisecond, DownloadPause: func(int) time.Duration { return 0 }}
	done := make(chan error, 1)
	go func() {
		done <- c.download(context.Background(), srv.URL, filepath.Join(t.TempDir(), "f.bin"), "", 1000, nil)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a download that stalls cannot succeed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled download waited for ever")
	}
}

func TestCleanOldLeavesAnUpdateUnderWayAlone(t *testing.T) {
	r := newCoord(t, "1.0.10")
	staged := filepath.Join(r.c.Root, "staging", "1.0.11", "maccy.exe")
	write(t, staged, "NEW")
	r.c.mu.Lock()
	r.c.applying = true
	r.c.mu.Unlock()
	r.c.CleanOld()
	if _, err := os.Stat(staged); err != nil {
		t.Fatal("startup cleanup deleted what the update about to run needs")
	}
	r.c.mu.Lock()
	r.c.applying = false
	r.c.mu.Unlock()
	r.c.CleanOld()
	if _, err := os.Stat(staged); err == nil {
		t.Fatal("with no update under way the old files go")
	}
}

func TestFailedRecheckKeepsTheKnownUpdate(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", nil, true)
	r.c.Check(context.Background())
	r.s.manifest.Store([]byte(nil)) // the server stops answering
	r.c.Check(context.Background())
	st := r.c.State()
	if st.Phase != Problem || !st.Check || r.c.Available() == nil {
		t.Fatalf("a failed look must not make the known update unknown: %+v", st)
	}
	r.c.Dismiss()
	if r.c.State().Phase != Available {
		t.Fatalf("closing the message goes back to the update: %+v", r.c.State())
	}
}

func TestMandatoryUpdateOfAnotherSystemShutsNothing(t *testing.T) {
	r := newCoord(t, "1.0.10")
	m := release("1.0.11")
	m.Latest.Installer = Installer{URL: "https://x/setup.exe", SHA256: strings.Repeat("a", 64)}
	m.Latest.Mandatory = true
	r.s.setManifest(t, m)
	r.c.Check(context.Background())
	r.c.Supported = false
	if r.c.Available() == nil || r.c.MustUpdate() != nil {
		t.Fatal("a mandatory release this system cannot install is no reason to shut it")
	}
	r.c.Supported = true
	if r.c.MustUpdate() == nil {
		t.Fatal("where it can be installed it is a must")
	}
}

func TestCheckResultDoesNotReplaceAnInstallation(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", nil, true)
	r.c.Check(context.Background())
	r.c.mu.Lock()
	r.c.applying = true
	r.c.state.Phase = Downloading
	r.c.mu.Unlock()
	r.c.setChecking(func(s *State) { *s = State{Phase: Available} })
	if r.c.State().Phase != Downloading {
		t.Fatalf("a check result replaced the download: %+v", r.c.State())
	}
}

func TestUnwritableInstallFolderGoesToTheInstaller(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes mean something else here")
	}
	r := newCoord(t, "1.0.10")
	z := zipOf(t, "1.0.11", "win-x64", map[string]string{"maccy.exe": "NEW EXE"}, nil)
	r.publish(t, "1.0.11", z, true)
	r.c.Check(context.Background())
	dir := filepath.Dir(r.exe)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot make the folder read-only")
	}
	defer os.Chmod(dir, 0o755)
	if f, err := os.Create(filepath.Join(dir, "probe")); err == nil {
		f.Close()
		t.Skip("writable anyway (root?)")
	}
	r.c.Apply(context.Background())
	if len(r.spawn) != 1 {
		t.Fatalf("the helper was started %d times", len(r.spawn))
	}
	plan, _ := os.ReadFile(r.spawn[0][1])
	if !strings.Contains(string(plan), "setup.exe") && !strings.Contains(string(plan), "installer") {
		t.Fatalf("the plan should be the installer's, not the helper's file replacing: %s", plan)
	}
}

func TestApplyTellsWhenTheOldVersionCouldNotBeRestored(t *testing.T) {
	r := newRig(t, newPkg, nil)
	h := r.hooks()
	h.Sleep = func(time.Duration) {}
	h.Launch = func(p string) error {
		_ = os.RemoveAll(r.plan.BackupDir) // what the rollback needs is gone (a scanner took it)
		return errors.New("blocked by the virus scanner")
	}
	if err := RunPlan(r.plan, h); err == nil {
		t.Fatal("a program that cannot start is a failed update")
	}
	if len(r.alerts) != 1 || !strings.Contains(r.alerts[0], "没能完全恢复") || strings.Contains(r.alerts[0], "已恢复旧版本") {
		t.Fatalf("an incomplete recovery must not be called a recovery: %v", r.alerts)
	}
}

func TestReplacedAndDeletedBackupsDoNotShareAPlace(t *testing.T) {
	files := map[string]string{}
	for k, v := range newPkg {
		files[k] = v
	}
	files["_deleted/plugin.dll"] = "NEW" // the package replaces this one...
	r := newRig(t, files, nil)
	write(t, filepath.Join(r.install, "_deleted", "plugin.dll"), "OLD REPLACED")
	write(t, filepath.Join(r.install, "plugin.dll"), "OLD DELETED") // ...and the earlier package's plugin.dll goes
	prev := PackageManifest{AppID: "maccy", Version: "1.0.10", Runtime: "win-x64", Files: []PackageFile{{Path: "plugin.dll"}}}
	b, _ := json.Marshal(prev)
	write(t, r.plan.InstalledManifest, string(b))
	if err := RunPlan(r.plan, r.hooks()); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(r.plan.BackupDir, "replaced", "_deleted", "plugin.dll")); got != "OLD REPLACED" {
		t.Errorf("replaced backup = %q", got)
	}
	if got := read(t, filepath.Join(r.plan.BackupDir, "deleted", "plugin.dll")); got != "OLD DELETED" {
		t.Errorf("deleted backup = %q", got)
	}
}

func TestReadOnlyFileInTheInstallDirMeansTheInstallerIsUsed(t *testing.T) {
	r := newCoord(t, "1.0.10")
	dir := filepath.Dir(r.exe)
	ro := filepath.Join(dir, "sqlite.dll")
	write(t, ro, "DLL")
	if !r.c.installDirWritable() {
		t.Fatal("a folder of writable files is writable")
	}
	if err := os.Chmod(ro, 0o444); err != nil {
		t.Skip("cannot make the file read-only")
	}
	defer os.Chmod(ro, 0o644)
	if f, err := os.OpenFile(ro, os.O_WRONLY, 0); err == nil {
		f.Close()
		t.Skip("read-only files are writable anyway (root?)")
	}
	if !r.c.installDirWritable() {
		t.Fatal("an unrelated read-only file must not block a package")
	}
	// The user's own data is not the update's to replace.
	_ = os.Chmod(ro, 0o644)
	write(t, filepath.Join(dir, "history.json"), "[]")
	_ = os.Chmod(filepath.Join(dir, "history.json"), 0o444)
	defer os.Chmod(filepath.Join(dir, "history.json"), 0o644)
	if !r.c.installDirWritable() {
		t.Fatal("a read-only data file is not in the update's way")
	}
}
