package prefswin

import (
	"github.com/egoist/mygo/ui"
	"klip/internal/panel"
)

func (m *Model) Dirty() bool { return m.v != m.observed }
func (m *Model) Save() bool {
	if err := m.Prefs.Save(m.observed, m.v); err != nil {
		m.toast("保存失败：" + err.Error())
		return false
	}
	m.v = m.Prefs.CurrentView()
	m.observed = m.v
	m.syncThemePreview()
	m.toast("设置已保存")
	return true
}
func (m *Model) RequestClose() {
	if !m.Dirty() {
		m.Host.Close()
		return
	}
	m.confirmClose = true
	if m.Redraw != nil {
		m.Redraw()
	}
}

// RequestQuit is the program about to quit. Without unsaved edits it returns
// false and the quit goes on. With some, it puts up the unsaved dialog and
// returns true: the quit waits, and quit is called once the edits are saved
// or discarded; "继续编辑" calls it off.
func (m *Model) RequestQuit(quit func()) bool {
	if !m.Dirty() {
		return false
	}
	m.confirmClose = true
	m.quitAfter = quit
	if m.Redraw != nil {
		m.Redraw()
	}
	return true
}

// leave ends the dialog after a save or a discard: the quit it was put up for
// goes on, or the window closes.
func (m *Model) leave() {
	m.confirmClose = false
	if quit := m.quitAfter; quit != nil {
		m.quitAfter = nil
		quit()
		return
	}
	m.Host.Close()
}

func (m *Model) stay() {
	m.confirmClose = false
	m.quitAfter = nil
}

func (m *Model) unsavedDialog(c *ui.Context, p Palette) {
	if !m.confirmClose {
		return
	}
	question, saveLabel := "是否保存修改后关闭？", "保存并关闭"
	if m.quitAfter != nil {
		question, saveLabel = "是否保存修改后退出 Klip？", "保存并退出"
	}
	ui.Overlay(c, func() {
		back := ui.Box(c).Absolute().Left(0).Top(0).Right(0).Bottom(0).Background(ui.Hex("#00000066")).Center().Modal()
		if back.OverlayShortcut(0, ui.KeyEscape) {
			m.stay()
		}
		back.Children(func() {
			ui.Column(c).Width(390).Padding(20).Gap(16).Background(p.Panel).Border(1, p.Border).Radius(10).Children(func() {
				ui.Text(c, "设置尚未保存").FontSize(panel.FontTitle).FontWeight(600).TextColor(p.Text)
				ui.Text(c, question).FontSize(panel.FontBody).TextColor(p.Text)
				ui.Row(c).Gap(8).Children(func() {
					if panel.Button(c, p, "继续编辑", 5, 10, 13).Clicked() {
						m.stay()
					}
					if panel.Button(c, p, "放弃修改", 5, 10, 13).Clicked() {
						m.v = m.Prefs.CurrentView()
						m.observed = m.v
						m.syncThemePreview()
						m.leave()
					}
					if panel.Button(c, p, saveLabel, 5, 10, 13).Label(saveLabel).Clicked() {
						if m.Save() {
							m.leave()
						} else {
							m.stay() // the reason is in the toast; the quit is off
						}
					}
				})
			})
		})
	})
}
