package main

import (
	"time"

	"github.com/egoist/mygo"
)

// Auxiliary windows keep the panel visible for their entire visible lifetime:
// the preferences, About, and the preview, which the user may click into to
// select and copy text (on Windows that takes the focus from the panel).
// Delay inspection because native blur can precede showing the destination.
func (p *program) onWindowBlur() {
	// A blur while an auxiliary window is open is not acted on then; the
	// preview, which goes away on its own, looks again when it does
	// (previewHidden).
	if p.auxiliaryWindowVisible() {
		return
	}
	p.checkFocusSoon()
}

// checkFocusSoon hides the panel a moment later if by then the focus is in
// none of Klip's windows and no auxiliary window is up.
func (p *program) checkFocusSoon() {
	time.AfterFunc(100*time.Millisecond, func() {
		if p.quitting.Load() {
			return
		}
		mygo.RunOnMain(func() {
			if p.quitting.Load() || p.win == nil || !p.win.IsVisible() {
				return
			}
			mainFocused := p.win.IsFocused()
			if hideAfterFocusTransfer(mainFocused, p.auxiliaryWindowVisible()) {
				p.hide()
			}
		})
	})
}

// previewHidden is the preview gone away. If it had the focus (the user was
// selecting text in it), the panel takes it back, so typing goes on there;
// otherwise the panel goes too, unless the focus is still in Klip: the click
// that left the preview may have been into another program, whose blur the
// panel had already been told of while the preview was up.
func (p *program) previewHidden(hadFocus bool) {
	if p.quitting.Load() || p.win == nil || !p.win.IsVisible() {
		return
	}
	if hadFocus {
		p.win.Focus()
		return
	}
	p.checkFocusSoon()
}

func hideAfterFocusTransfer(mainFocused, auxiliaryVisible bool) bool {
	return !mainFocused && !auxiliaryVisible
}

func (p *program) auxiliaryWindowVisible() bool {
	return (p.prefsWin != nil && p.prefsWin.IsVisible()) || (p.about != nil && p.about.Visible()) ||
		(p.preview != nil && p.preview.Visible())
}

func (p *program) closePreferences() {
	p.prefsWin.Hide()
	if !p.quitting.Load() && p.win != nil && p.win.IsVisible() {
		p.win.Focus()
	}
}
