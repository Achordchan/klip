package main

import (
	"time"

	"github.com/egoist/mygo"
)

// Auxiliary windows keep the panel visible for their entire visible lifetime.
// Delay inspection because native blur can precede showing the destination.
func (p *program) onWindowBlur() {
	// A blur while an auxiliary window is open is handled permanently, not
	// reconsidered later after that window closes.
	if p.auxiliaryWindowVisible() {
		return
	}
	time.AfterFunc(100*time.Millisecond, func() {
		if p.quitting.Load() {
			return
		}
		mygo.RunOnMain(func() {
			if p.quitting.Load() || p.win == nil || !p.win.IsVisible() {
				return
			}
			mainFocused := p.win.IsFocused()
			prefsVisible := p.prefsWin != nil && p.prefsWin.IsVisible()
			aboutVisible := p.about != nil && p.about.Visible()
			if hideAfterFocusTransfer(mainFocused, prefsVisible, aboutVisible) {
				p.hide()
			}
		})
	})
}

func hideAfterFocusTransfer(mainFocused, prefsVisible, aboutVisible bool) bool {
	return !mainFocused && !prefsVisible && !aboutVisible
}

func (p *program) auxiliaryWindowVisible() bool {
	return (p.prefsWin != nil && p.prefsWin.IsVisible()) || (p.about != nil && p.about.Visible())
}

func (p *program) closePreferences() {
	p.prefsWin.Hide()
	if !p.quitting.Load() && p.win != nil && p.win.IsVisible() {
		p.win.Focus()
	}
}
