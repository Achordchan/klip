package main

import (
	"testing"

	"github.com/egoist/mygo"

	"klip/internal/previewwin"
)

func TestVisibleAuxiliaryWindowsKeepMainPanel(t *testing.T) {
	for _, tc := range []struct {
		name            string
		main, aux, hide bool
	}{
		{"main", true, false, false},
		{"an auxiliary window (preferences, About, the preview being used) is up", false, true, false},
		{"another application with auxiliary windows closed", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hideAfterFocusTransfer(tc.main, tc.aux); got != tc.hide {
				t.Fatalf("hide = %v, want %v", got, tc.hide)
			}
		})
	}
}

func TestPreviewCountsAsAnAuxiliaryWindow(t *testing.T) {
	p := &program{preview: &previewwin.Manager{}}
	if p.auxiliaryWindowVisible() {
		t.Fatal("a preview never shown counts as up")
	}
}

func TestNoQuitWhileMigrating(t *testing.T) {
	p := &program{}
	p.migrating.Store(true)
	e := &mygo.QuitEvent{}
	p.beforeQuit(e)
	if !e.DefaultPrevented() || p.quitting.Load() {
		t.Fatal("a quit went on while the data was being copied")
	}
	p.migrating.Store(false) // the relaunch at the end
	e = &mygo.QuitEvent{}
	p.beforeQuit(e)
	if e.DefaultPrevented() || !p.quitting.Load() {
		t.Fatal("the quit after the migration was held")
	}
}
