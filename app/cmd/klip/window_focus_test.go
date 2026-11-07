package main

import (
	"testing"

	"github.com/egoist/mygo"

	"klip/internal/previewwin"
)

func TestWhatKeepsTheMainPanel(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		main, preview, aux, hide bool
	}{
		{"main", true, false, false, false},
		{"clicked into the preview to select text", false, true, false, false},
		{"preferences or About open", false, false, true, false},
		{"another program (Alt+Tab) while a preview shows under the pointer", false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hideAfterFocusTransfer(tc.main, tc.preview, tc.aux); got != tc.hide {
				t.Fatalf("hide = %v, want %v", got, tc.hide)
			}
		})
	}
}

func TestPreviewShowingDoesNotCountAsAuxiliary(t *testing.T) {
	p := &program{preview: &previewwin.Manager{}}
	if p.auxiliaryWindowVisible() || p.preview.Focused() {
		t.Fatal("a preview counts as keeping the panel without having the focus")
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
