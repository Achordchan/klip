package update

import (
	"context"
	"os"
	"path/filepath"
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
	if err := r.c.InstallPrepared(); err != nil {
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
		if err := r.c.InstallPrepared(); err != nil {
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
	if err := r.c.InstallPrepared(); err == nil {
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
	if err := r.c.InstallPrepared(); err == nil {
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
