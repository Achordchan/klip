package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The update nobody has to ask for: Prepare downloads and unpacks the newer
// version in the background, silently; the program calls InstallPrepared
// when the user is away (the panel and the other windows closed, no sync,
// no input for a while). Klip then quits, the helper puts the files in place
// and starts the new version, which shows nothing (QuietRelaunch): the user
// finds Klip updated. Only the light package qualifies: the installer may
// ask for administrator rights, which nobody is there to give.

// MaxAutoAttempts is how many times one version is installed without the
// user: one that fails that often waits for the update button.
const MaxAutoAttempts = 2

// quietMarkerTTL is how long after the quiet quit the marker still counts: a
// start much later is the user's own.
const quietMarkerTTL = 10 * time.Minute

// errNotReady is InstallPrepared with nothing to install.
var errNotReady = errors.New("no update is ready to be installed")

// autoEligible says whether info can be installed without the user here.
func (c *Coordinator) autoEligible(info *Info) bool {
	return info != nil && c.Supported && info.HasSupportedPackage(c.Client.runtime()) &&
		c.installDirWritable() && c.attempts(info.Version) < MaxAutoAttempts
}

// Prepare downloads and unpacks the newer version that is known, for
// InstallPrepared, if it can be installed without the user and is not ready
// yet. It says nothing to anybody: the state the windows show is untouched,
// and a failure is only returned (the next check tries again). It does not
// hold the update back from a manual install, which uses what it made.
func (c *Coordinator) Prepare(ctx context.Context) error {
	c.mu.Lock()
	info := c.state.Info
	if info == nil || c.applying || c.preparing || !c.autoEligible(info) ||
		(c.prepared != nil && c.prepared.Version == info.Version) {
		c.mu.Unlock()
		return nil
	}
	c.preparing = true
	want := *info
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.preparing = false; c.mu.Unlock() }()

	// The folders this uses are the ones CleanOld and Apply use. The lock is
	// held until the package is published: a CleanOld in between would remove
	// its files and leave a preparation that points at nothing.
	c.fsMu.Lock()
	defer c.fsMu.Unlock()
	prep, err := c.Client.DownloadPackage(ctx, want, c.Root, nil)
	if c.afterDownload != nil {
		c.afterDownload()
	}
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur := c.state.Info; cur == nil || cur.Version != want.Version || c.applying {
		// A newer version is known since, or an install began: this one is
		// not wanted. Its folder goes with the next CleanOld.
		return nil
	}
	c.prepared = prep
	return nil
}

// Ready says whether an update is downloaded, unpacked and waiting to be
// installed without the user.
func (c *Coordinator) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readyLocked()
}

func (c *Coordinator) readyLocked() bool {
	info := c.state.Info
	return c.prepared != nil && info != nil && c.prepared.Version == info.Version && !c.applying
}

// takePrepared is the package made ready for version, which is then no
// longer kept (nil when there is none for it).
func (c *Coordinator) takePrepared(version string) *Prepared {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prepared == nil || c.prepared.Version != version {
		return nil
	}
	p := c.prepared
	c.prepared = nil
	return p
}

// errCalledOff is a quiet install that decide did not let go on.
var errCalledOff = errors.New("the quiet install was called off")

// InstallPrepared installs the update Prepare made ready, quietly: no window,
// no message box (Plan.Quiet), and the new version, started by the helper,
// does not show itself (QuietRelaunch). The attempt is counted before the
// helper starts, so that a version that fails is not tried for ever: after
// MaxAutoAttempts it waits for the user.
//
// The moment was chosen before this was called, and preparing the helper
// takes a while: decide has the last word. It is called once the helper
// waits, with commit, which writes the quiet marker and asks the program to
// quit (Shutdown); decide calls it, on the UI thread right after looking
// again that nobody came back, or returns without it to call the install
// off. Called off, the helper, which still waits for this process and has
// touched nothing, is stopped and reaped (were it left, a quit by the user
// within its wait would install the update after all, and it would hold the
// update mutex meanwhile), and the attempt is given back.
func (c *Coordinator) InstallPrepared(decide func(commit func())) error {
	c.mu.Lock()
	if !c.readyLocked() || c.attempts(c.prepared.Version) >= MaxAutoAttempts {
		c.mu.Unlock()
		return errNotReady
	}
	prep := c.prepared
	c.prepared = nil
	c.applying = true
	c.mu.Unlock()
	fail := func(err error) error {
		c.mu.Lock()
		c.applying = false
		c.mu.Unlock()
		return err
	}
	c.fsMu.Lock()
	defer c.fsMu.Unlock()
	if err := c.countAttempt(prep.Version); err != nil {
		return fail(err) // not counted, not tried: a loop could not be stopped
	}
	if err := c.startPackage(prep, true); err != nil {
		return fail(err)
	}
	committed := false
	decide(func() {
		committed = true
		// The helper waits for this process: the new one is told to keep quiet.
		if err := writeJSON(c.quietMarkerPath(), quietMarker{Version: prep.Version, At: c.now()}); err != nil {
			_ = err // a start that shows the panel is all that is lost
		}
		c.done()
	})
	if !committed {
		if err := c.stopHelper(); err != nil {
			// It could not be stopped: it will install when the program quits.
			// The attempt stays counted, and the install stays under way for
			// this process, so that nothing starts a second one.
			return fmt.Errorf("%w; the helper could not be stopped: %v", errCalledOff, err)
		}
		c.uncountAttempt(prep.Version)
		return fail(errCalledOff)
	}
	return nil
}

// ---- attempts

func (c *Coordinator) attemptsPath() string {
	return filepath.Join(c.Root, "auto-update-attempts.json")
}

func (c *Coordinator) readAttempts() map[string]int {
	m := map[string]int{}
	if b, err := os.ReadFile(c.attemptsPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// attempts is how many times version was installed without the user.
func (c *Coordinator) attempts(version string) int {
	return c.readAttempts()[NormalizeVersion(version)]
}

// countAttempt counts one more attempt of version, durably, and forgets the
// attempts of other versions (an older one is passed, a newer one is new).
func (c *Coordinator) countAttempt(version string) error {
	v := NormalizeVersion(version)
	n := c.readAttempts()[v]
	if err := os.MkdirAll(c.Root, 0o755); err != nil {
		return err
	}
	return writeJSON(c.attemptsPath(), map[string]int{v: n + 1})
}

// uncountAttempt gives back an attempt that was counted and not made.
func (c *Coordinator) uncountAttempt(version string) {
	v := NormalizeVersion(version)
	if n := c.readAttempts()[v]; n > 0 {
		_ = writeJSON(c.attemptsPath(), map[string]int{v: n - 1})
	}
}

// ---- quiet relaunch

type quietMarker struct {
	Version string    `json:"version"`
	At      time.Time `json:"at"`
}

func (c *Coordinator) quietMarkerPath() string { return filepath.Join(c.Root, "quiet-relaunch.json") }

// QuietRelaunch says whether this start is the one an update made after the
// program quit for it without the user (the new version, or the old one put
// back): then it should not show itself. The marker is used once.
func (c *Coordinator) QuietRelaunch() bool {
	path := c.quietMarkerPath()
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_ = os.Remove(path)
	var m quietMarker
	if json.Unmarshal(b, &m) != nil {
		return false
	}
	age := c.now().Sub(m.At)
	return age >= 0 && age <= quietMarkerTTL && strings.TrimSpace(m.Version) != ""
}
