package main

import (
	"testing"

	"github.com/egoist/mygo"
)

func TestVisibleAuxiliaryWindowsKeepMainPanel(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		main, prefs, about, hide bool
	}{
		{"main", true, false, false, false},
		{"preferences open while another application has focus", false, true, false, false},
		{"about open while another application has focus", false, false, true, false},
		{"both auxiliary windows open", false, true, true, false},
		{"another application with auxiliary windows closed", false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hideAfterFocusTransfer(tc.main, tc.prefs, tc.about); got != tc.hide {
				t.Fatalf("hide = %v, want %v", got, tc.hide)
			}
		})
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
