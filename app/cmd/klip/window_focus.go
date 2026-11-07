package main

import (
	"time"

	"github.com/egoist/mygo"
)

// The preferences and About keep the panel visible for their entire visible
// lifetime. The preview keeps it only while it has the focus: the user may
// click into it to select and copy text (on Windows that takes the focus
// from the panel), but a preview merely showing under the pointer must not
// keep the panel up when the user switches to another program (Alt+Tab).
// Delay inspection because native blur can precede showing the destination.
func (p *program) onWindowBlur() {
	// A blur while the preferences or About are open is not acted on then.
	if p.auxiliaryWindowVisible() {
		return
	}
	p.checkFocusSoon()
}

// checkFocusSoon hides the panel a moment later if by then the focus is in
// none of Klip's windows (the panel, the preview) and neither the
// preferences nor About is up. It serves the panel's blur, the preview's
// blur (the focus left the preview: back to the panel, or out of Klip) and
// the preview going away.
func (p *program) checkFocusSoon() {
	time.AfterFunc(100*time.Millisecond, func() {
		if p.quitting.Load() {
			return
		}
		mygo.RunOnMain(func() {
			if p.quitting.Load() || p.win == nil || !p.win.IsVisible() {
				return
			}
			previewFocused := p.preview != nil && p.preview.Focused()
			if hideAfterFocusTransfer(p.win.IsFocused(), previewFocused, p.auxiliaryWindowVisible()) {
				p.hide()
			}
		})
	})
}

// previewHidden is the preview gone away. If it had the focus (the user was
// selecting text in it), the panel takes it back, so typing goes on there;
// otherwise the focus is looked at again.
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

func hideAfterFocusTransfer(mainFocused, previewFocused, auxiliaryVisible bool) bool {
	return !mainFocused && !previewFocused && !auxiliaryVisible
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
