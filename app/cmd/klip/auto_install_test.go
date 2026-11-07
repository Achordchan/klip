package main

import (
	"testing"
	"time"
)

func TestShouldAutoInstall(t *testing.T) {
	away := autoInstallGate{ready: true, idle: autoInstallIdle}
	if !shouldAutoInstall(away) {
		t.Fatal("nothing open, nobody there: the update should go in")
	}
	for name, change := range map[string]func(*autoInstallGate){
		"nothing ready":          func(g *autoInstallGate) { g.ready = false },
		"the panel is up":        func(g *autoInstallGate) { g.panel = true },
		"the preferences are up": func(g *autoInstallGate) { g.prefs = true },
		"About is up":            func(g *autoInstallGate) { g.about = true },
		"the user's own update":  func(g *autoInstallGate) { g.updateWin = true },
		"a migration":            func(g *autoInstallGate) { g.migrating = true },
		"a mandatory update":     func(g *autoInstallGate) { g.mandatory = true },
		"a sync":                 func(g *autoInstallGate) { g.syncing = true },
		"the user is working":    func(g *autoInstallGate) { g.idle = autoInstallIdle - time.Second },
	} {
		g := away
		change(&g)
		if shouldAutoInstall(g) {
			t.Errorf("%s: the update would quit Klip", name)
		}
	}
}

func TestQuietInstallGivesTheSyncRunnerBackWhenItCannotCommit(t *testing.T) {
	away := func() autoInstallGate { return autoInstallGate{ready: true, idle: autoInstallIdle} }
	no := func() bool { return false }
	for _, tc := range []struct {
		name                       string
		reserved, committed, quits bool
		released                   int
	}{
		{"committed: the runner stays held", true, true, true, 0},
		{"the commit failed: the runner is given back", true, false, false, 1},
		{"a sync runs: nothing is reserved", false, true, false, 0},
	} {
		released, commits := 0, 0
		reserve := func() (func(), bool) {
			if !tc.reserved {
				return nil, false
			}
			return func() { released++ }, true
		}
		commit := func() bool { commits++; return tc.committed }
		quits := decideQuietInstall(away, no, reserve, commit)
		if quits != tc.quits || released != tc.released {
			t.Errorf("%s: quits %v, released %d", tc.name, quits, released)
		}
		if !tc.reserved && commits != 0 {
			t.Errorf("%s: committed without the runner", tc.name)
		}
	}
	// The user is back: nothing is reserved, nothing committed.
	reserved := false
	busy := func() autoInstallGate { g := away(); g.panel = true; return g }
	if decideQuietInstall(busy, no, func() (func(), bool) { reserved = true; return func() {}, true }, func() bool { return true }) || reserved {
		t.Fatal("the install went on with the panel up")
	}
}
