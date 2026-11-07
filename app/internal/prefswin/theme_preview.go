package prefswin

import "klip/internal/prefs"

func (m *Model) selectTheme(option prefs.Option) {
	m.v.Theme = option
	m.syncThemePreview()
}

// Preview never writes settings. Keep it active across unrelated background
// settings updates, and release it after save/discard to follow the saved theme.
func (m *Model) syncThemePreview() {
	active := m.v.Theme != m.observed.Theme
	value := m.v.Theme.Value
	if active == m.previewActive && (!active || value == m.previewValue) {
		return
	}
	m.previewActive, m.previewValue = active, value
	if m.PreviewTheme != nil {
		if active {
			copy := value
			m.PreviewTheme(&copy)
		} else {
			m.PreviewTheme(nil)
		}
	}
}
