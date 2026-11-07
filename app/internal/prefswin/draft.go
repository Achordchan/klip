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
func (m *Model) unsavedDialog(c *ui.Context, p Palette) {
	if !m.confirmClose {
		return
	}
	ui.Overlay(c, func() {
		back := ui.Box(c).Absolute().Left(0).Top(0).Right(0).Bottom(0).Background(ui.Hex("#00000066")).Center().Modal()
		if back.OverlayShortcut(0, ui.KeyEscape) {
			m.confirmClose = false
		}
		back.Children(func() {
			ui.Column(c).Width(390).Padding(20).Gap(16).Background(p.Panel).Border(1, p.Border).Radius(10).Children(func() {
				ui.Text(c, "设置尚未保存").FontSize(panel.FontTitle).FontWeight(600).TextColor(p.Text)
				ui.Text(c, "是否保存修改后关闭？").FontSize(panel.FontBody).TextColor(p.Text)
				ui.Row(c).Gap(8).Children(func() {
					if panel.Button(c, p, "继续编辑", 5, 10, 13).Clicked() {
						m.confirmClose = false
					}
					if panel.Button(c, p, "放弃修改", 5, 10, 13).Clicked() {
						m.v = m.Prefs.CurrentView()
						m.observed = m.v
						m.syncThemePreview()
						m.confirmClose = false
						m.Host.Close()
					}
					if panel.Button(c, p, "保存并关闭", 5, 10, 13).Clicked() {
						if m.Save() {
							m.confirmClose = false
							m.Host.Close()
						} else {
							m.confirmClose = false
						}
					}
				})
			})
		})
	})
}
