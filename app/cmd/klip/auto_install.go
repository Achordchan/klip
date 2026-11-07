package main

import (
	"log"
	"time"

	"github.com/egoist/mygo"
)

// The update installed while the user is away (update.Coordinator.Prepare
// and InstallPrepared): every autoInstallEvery the program looks whether
// it is a harmless moment, and if so lets the update quit it.
const (
	autoInstallEvery = 30 * time.Second
	// autoInstallIdle is how long the user must have touched neither mouse nor
	// keyboard: someone who is working is never interrupted.
	autoInstallIdle = 3 * time.Minute
)

// autoInstallGate is what decides whether now is such a moment.
type autoInstallGate struct {
	ready     bool          // an update is downloaded, checked and waiting
	panel     bool          // the main window is up (its preview goes with it)
	prefs     bool          // the preferences are up (perhaps with edits)
	about     bool          // About is up
	updateWin bool          // the user's own check or install is under way
	migrating bool          // the data is being moved
	mandatory bool          // a mandatory update has its own window
	syncing   bool          // a sync runs: quitting would cut it off
	idle      time.Duration // since the user last used mouse or keyboard
}

// shouldAutoInstall says whether the update may quit the program now: only
// with nothing of Klip's open, nothing under way, and nobody at the machine.
func shouldAutoInstall(g autoInstallGate) bool {
	return g.ready && !g.panel && !g.prefs && !g.about && !g.updateWin &&
		!g.migrating && !g.mandatory && !g.syncing && g.idle >= autoInstallIdle
}

// watchAutoInstall starts the looks, for as long as the program runs.
func (p *program) watchAutoInstall() {
	t := time.NewTicker(autoInstallEvery)
	go func() {
		defer t.Stop()
		for range t.C {
			if p.quitting.Load() {
				return
			}
			if !p.app.Updates.Ready() {
				continue // the usual case: nothing to install, nothing to ask
			}
			mygo.RunOnMain(p.autoInstallNow)
		}
	}()
}

// gate is the state the moment is judged by. UI thread.
func (p *program) gate() autoInstallGate {
	return autoInstallGate{
		ready:     p.app.Updates.Ready(),
		panel:     p.win != nil && p.win.IsVisible(),
		prefs:     p.prefsWin != nil && p.prefsWin.IsVisible(),
		about:     p.about != nil && p.about.Visible(),
		updateWin: p.upd != nil && p.upd.Active(),
		migrating: p.storageMoving.Load(),
		mandatory: p.app.Updates.MustUpdate() != nil,
		syncing:   p.app.Syncing(),
		idle:      systemIdle(),
	}
}

// autoInstallNow installs the waiting update if now is the moment. UI thread.
func (p *program) autoInstallNow() {
	if p.quitting.Load() || p.app == nil || !shouldAutoInstall(p.gate()) {
		return
	}
	go func() {
		err := p.app.Updates.InstallPrepared(p.decideAutoInstall)
		if err != nil {
			log.Println("auto update:", err)
		}
	}()
}

// decideAutoInstall is the last word on a quiet install, once its helper
// waits: on the UI thread, where the user's clicks are handled, the moment is
// judged again and, if it still holds, the program quits for the update in
// the same step, with no click in between. Otherwise nothing happens (the
// user came back, a sync or a migration began). Any goroutine but the UI
// thread: it waits for it.
func (p *program) decideAutoInstall(commit func()) {
	done := make(chan struct{})
	mygo.RunOnMain(func() {
		defer close(done)
		g := p.gate()
		g.ready = true // the update is being handed over, no longer "ready"
		if !p.quitting.Load() && shouldAutoInstall(g) {
			commit() // quits, through quitForUpdate
		}
	})
	<-done
}
