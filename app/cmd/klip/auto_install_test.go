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
