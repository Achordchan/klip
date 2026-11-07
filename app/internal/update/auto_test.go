package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestPrepareIsSilentAndInstallIsQuiet(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	r.mu.Lock()
	r.states = nil
	r.mu.Unlock()
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.c.Ready() {
		t.Fatal("the update was not made ready")
	}
	r.mu.Lock()
	seen := append([]Phase(nil), r.states...)
	r.mu.Unlock()
	if len(seen) != 0 || r.c.State().Phase != Available {
		t.Fatalf("preparing showed itself: %v, %+v", seen, r.c.State())
	}
	if err := r.c.InstallPrepared(commitAlways); err != nil {
		t.Fatal(err)
	}
	if r.shut != 1 || len(r.spawn) != 1 {
		t.Fatalf("shut %d, helpers %v", r.shut, r.spawn)
	}
	plan, err := ReadPlan(r.spawn[0][1])
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Quiet || plan.Version != "1.0.11" || plan.ResultPath != r.c.ResultPath() {
		t.Fatalf("plan %+v", plan)
	}
	if !r.c.QuietRelaunch() {
		t.Fatal("the next start is not told to keep quiet")
	}
	if r.c.QuietRelaunch() {
		t.Fatal("the quiet marker is used more than once")
	}
	// What the helper does with the plan, quietly.
	rr := &rig{t: t}
	if err := RunPlan(plan, rr.hooks()); err != nil {
		t.Fatal(err)
	}
	if read(t, r.exe) != "NEW EXE" || len(rr.alerts) != 0 {
		t.Fatalf("exe %q, alerts %v", read(t, r.exe), rr.alerts)
	}
}

func TestManualInstallUsesThePreparedPackage(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	hits := r.s.hits.Load()
	r.c.Apply(context.Background())
	if r.shut != 1 || len(r.spawn) != 1 {
		t.Fatalf("shut %d, helpers %v", r.shut, r.spawn)
	}
	if r.s.hits.Load() != hits {
		t.Fatal("the package was downloaded again")
	}
	if plan, _ := ReadPlan(r.spawn[0][1]); plan.Quiet {
		t.Fatal("an update the user asked for is not quiet")
	}
}

func TestDownloadedPackageIsReusedAfterARestart(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A new process: the staging goes, the zip of the coming version stays.
	c2 := NewCoordinator(r.c.Client, r.c.Root, r.exe, func() {})
	c2.Supported = true
	c2.CleanOld()
	if _, err := os.Stat(filepath.Join(r.c.Root, "packages", "maccy-1.0.11-win-x64.zip")); err != nil {
		t.Fatal("the zip of the coming version was cleaned")
	}
	c2.Refresh(context.Background())
	hits := r.s.hits.Load()
	if err := c2.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c2.Ready() || r.s.hits.Load() != hits {
		t.Fatalf("ready %v, downloaded again: %v", c2.Ready(), r.s.hits.Load() != hits)
	}
}

func TestAVersionThatFailsTwiceWaitsForTheUser(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	for i := 0; i < MaxAutoAttempts; i++ {
		if err := r.c.Prepare(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := r.c.InstallPrepared(commitAlways); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		// The helper rolled it back and the old version started again.
		r.c.mu.Lock()
		r.c.applying = false
		r.c.mu.Unlock()
	}
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.c.Ready() {
		t.Fatal("a version that failed twice is made ready again")
	}
	if err := r.c.InstallPrepared(commitAlways); err == nil {
		t.Fatal("a version that failed twice is installed again")
	}
	if r.c.Available() == nil {
		t.Fatal("the update button must stay")
	}
	// A newer version starts afresh.
	r.publish(t, "1.0.12", zipOf(t, "1.0.12", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.c.Ready() {
		t.Fatal("a newer version is not made ready")
	}
}

func TestStalePreparationIsNotInstalled(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.publish(t, "1.0.12", zipOf(t, "1.0.12", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	if r.c.Ready() {
		t.Fatal("the package of 1.0.11 counts as ready for 1.0.12")
	}
	if err := r.c.InstallPrepared(commitAlways); err == nil {
		t.Fatal("an older preparation was installed")
	}
}

func TestOnlyTheLightPackageIsInstalledQuietly(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", nil, true) // an installer only
	r.c.Refresh(context.Background())
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.c.Ready() {
		t.Fatal("an installer, which may ask for rights, is made ready for a quiet install")
	}
}

func TestQuietMarkerExpires(t *testing.T) {
	r := newCoord(t, "1.0.10")
	if err := os.MkdirAll(r.c.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(r.c.quietMarkerPath(), quietMarker{Version: "1.0.11", At: r.c.now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if r.c.QuietRelaunch() {
		t.Fatal("a start an hour after the quiet quit is the user's own")
	}
	if _, err := os.Stat(r.c.quietMarkerPath()); !os.IsNotExist(err) {
		t.Fatal("an old marker stays")
	}
}

func TestCleanOldAfterPrepareDoesNotLeaveAPreparationWithoutFiles(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.c.CleanOld() // ran second, at the same start
	if r.c.Ready() {
		t.Fatal("a preparation whose files were cleaned counts as ready")
	}
	if err := r.c.InstallPrepared(commitAlways); err == nil || len(r.spawn) != 0 {
		t.Fatalf("a helper was started for files that are gone: %v %v", err, r.spawn)
	}
	// Made ready again from the kept zip, without a download.
	hits := r.s.hits.Load()
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.c.Ready() || r.s.hits.Load() != hits {
		t.Fatalf("ready %v, downloaded again %v", r.c.Ready(), r.s.hits.Load() != hits)
	}
}

func TestUnwritableInstallFolderIsNotPreparedForAQuietInstall(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		// Windows: a mode only sets the read-only attribute, which does not
		// keep files from being made inside; root writes anywhere.
		t.Skip("needs a folder this user cannot write to")
	}
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	dir := filepath.Dir(r.exe)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.c.Ready() {
		t.Fatal("an install folder that needs rights is made ready for a quiet install")
	}
}

func commitAlways(commit func() bool) { commit() }

func TestCalledOffInstallQuitsNothingAndGivesTheAttemptBack(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopped := 0
	r.c.StopHelper = func() { stopped++ }
	// The user came back while the helper was being prepared.
	if err := r.c.InstallPrepared(func(func() bool) {}); err == nil {
		t.Fatal("a called-off install succeeded")
	}
	if len(r.spawn) != 1 || stopped != 1 {
		t.Fatalf("the waiting helper was not stopped: started %d, stopped %d", len(r.spawn), stopped)
	}
	if r.shut != 0 {
		t.Fatal("the program quit although the install was called off")
	}
	if r.c.QuietRelaunch() {
		t.Fatal("a quiet marker was written for an install that did not happen")
	}
	if n := r.c.attempts("1.0.11"); n != 0 {
		t.Fatalf("a called-off install used an attempt: %d", n)
	}
	r.c.mu.Lock()
	applying := r.c.applying
	r.c.mu.Unlock()
	if applying {
		t.Fatal("still applying")
	}
	// It is tried again later, from the kept zip.
	if err := r.c.Prepare(context.Background()); err != nil || !r.c.Ready() {
		t.Fatalf("not made ready again: %v", err)
	}
}

func TestCleanOldDuringPrepareCannotLeaveAPreparationWithoutFiles(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	cleaned := make(chan struct{})
	r.c.afterDownload = func() {
		// CleanOld comes in between the download and the publication.
		go func() { r.c.CleanOld(); close(cleaned) }()
		time.Sleep(50 * time.Millisecond)
	}
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-cleaned
	r.c.mu.Lock()
	prep := r.c.prepared
	r.c.mu.Unlock()
	if prep != nil {
		if _, err := os.Stat(prep.StagingPath); err != nil {
			t.Fatal("a preparation whose files were removed is published")
		}
	}
}

func TestStopHelperEndsARealWaitingHelper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a shell script stands in for the helper")
	}
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper.sh")
	// Says it is ready, then waits as the helper waits for the program.
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch \"$2.ready\"\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(dir, "plan.json")
	if err := WritePlan(plan, Plan{PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{}
	if err := c.spawn(helper, plan); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.stopHelper() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the helper was not stopped")
	}
	if c.stopHelper() != nil {
		t.Fatal("a second stop fails")
	}
}

func TestPrepareStandsBackForAnInstallThatTookTheFolders(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	// The user's own install holds the folders, and is under way, when the
	// quiet preparation, which passed its first look, comes to them.
	r.c.fsMu.Lock()
	done := make(chan error)
	go func() { done <- r.c.Prepare(context.Background()) }()
	time.Sleep(50 * time.Millisecond) // past its first look, waiting for the folders
	r.c.mu.Lock()
	r.c.applying = true // the user's install, which has them
	r.c.mu.Unlock()
	hits := r.s.hits.Load()
	r.c.fsMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.s.hits.Load() != hits {
		t.Fatal("the package was downloaded while an install had it")
	}
	if entries, _ := os.ReadDir(filepath.Join(r.c.Root, "staging")); len(entries) != 0 {
		t.Fatalf("unpacked beside the install: %v", entries)
	}
}

func TestQuietHelperDoesNothingWhenTheProgramExitsWithoutCommitting(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	var plan Plan
	// The user quit Klip while the helper was being made ready: decide never
	// commits; the program is gone all the same.
	_ = r.c.InstallPrepared(func(func() bool) {
		p, err := ReadPlan(r.spawn[0][1])
		if err != nil {
			t.Fatal(err)
		}
		plan = p
	})
	if plan.CommitFile == "" {
		t.Fatal("a quiet plan carries no commit file")
	}
	rr := &rig{t: t}
	if err := RunPlan(plan, rr.hooks()); err == nil {
		t.Fatal("the helper installed without the program's leave")
	}
	if read(t, r.exe) != "RUNNING PROGRAM" {
		t.Fatalf("files were changed: %q", read(t, r.exe))
	}
	if len(rr.launched) != 0 || len(rr.alerts) != 0 {
		t.Fatalf("launched %v alerts %v", rr.launched, rr.alerts)
	}
	// With the leave, it installs, once.
	r2 := newCoord(t, "1.0.10")
	r2.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r2.c.Refresh(context.Background())
	if err := r2.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r2.c.InstallPrepared(commitAlways); err != nil {
		t.Fatal(err)
	}
	plan2, _ := ReadPlan(r2.spawn[0][1])
	rr2 := &rig{t: t}
	if err := RunPlan(plan2, rr2.hooks()); err != nil || read(t, r2.exe) != "NEW EXE" {
		t.Fatalf("a committed install did not go in: %v %q", err, read(t, r2.exe))
	}
	if _, err := os.Stat(plan2.CommitFile); !os.IsNotExist(err) {
		t.Fatal("the commit file stays for another time")
	}
}

// A leave an earlier attempt left behind, which could not be removed, does
// not authorize the next attempt: each has a commit file of its own.
func TestLeftoverCommitDoesNotAuthorizeTheNextAttempt(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	planOf := func() Plan {
		t.Helper()
		if err := r.c.Prepare(context.Background()); err != nil {
			t.Fatal(err)
		}
		var plan Plan
		r.c.StopHelper = func() {}
		r.spawn = nil
		err := r.c.InstallPrepared(func(commit func() bool) {
			p, err := ReadPlan(r.spawn[0][1])
			if err != nil {
				t.Fatal(err)
			}
			plan = p
		})
		if !errors.Is(err, errCalledOff) {
			t.Fatalf("not called off: %v", err)
		}
		return plan
	}
	first := planOf()
	// The first attempt's leave, and one at the name used before, are there
	// (written, then not removed), as is anything an interrupted helper left.
	for _, f := range []string{first.CommitFile, filepath.Join(r.c.Root, "quiet-install-commit.json")} {
		if err := writeJSON(f, quietMarker{Version: "1.0.11"}); err != nil {
			t.Fatal(err)
		}
	}
	second := planOf()
	if second.CommitFile == "" || second.CommitFile == first.CommitFile {
		t.Fatalf("the attempts share a commit file: %q", second.CommitFile)
	}
	rr := &rig{t: t}
	if err := RunPlan(second, rr.hooks()); err == nil || read(t, r.exe) != "RUNNING PROGRAM" {
		t.Fatalf("a leftover leave let the helper install: %v %q", err, read(t, r.exe))
	}
	// CleanOld takes the leftovers away.
	r.c.CleanOld()
	if left, _ := filepath.Glob(filepath.Join(r.c.Root, "quiet-install-commit*.json")); len(left) != 0 {
		t.Fatalf("leftover commit files: %v", left)
	}
}

func TestCommitThatCannotBeWrittenIsNoCommit(t *testing.T) {
	r := newCoord(t, "1.0.10")
	r.publish(t, "1.0.11", zipOf(t, "1.0.11", "win-x64", newPkg, nil), true)
	r.c.Refresh(context.Background())
	if err := r.c.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.c.failCommit = func() error { return errors.New("disk full") }
	stopped := 0
	r.c.StopHelper = func() { stopped++ }
	var said *bool
	err := r.c.InstallPrepared(func(commit func() bool) { ok := commit(); said = &ok })
	if err == nil || said == nil || *said {
		t.Fatalf("a commit that was not written counts: err %v, said %v", err, said)
	}
	if r.shut != 0 || stopped != 1 || r.c.attempts("1.0.11") != 0 {
		t.Fatalf("shut %d, helper stopped %d, attempts %d", r.shut, stopped, r.c.attempts("1.0.11"))
	}
}
