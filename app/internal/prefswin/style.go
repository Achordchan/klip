package prefswin

import "github.com/egoist/mygo/ui"

func primaryAction(c *ui.Context, p Palette, label string) ui.Element {
	b := ui.ButtonBase(c)
	color := p.Blue
	if b.Hovered() {
		color = ui.Hex("#2563EB")
	}
	return b.Padding(6, 14).Radius(6).Background(color).Center().Label(label).Children(func() { ui.Text(c, label).FontSize(13).FontWeight(600).TextColor(ui.RGB(255, 255, 255)).SingleLine() })
}
func statusChip(c *ui.Context, p Palette, label string, color ui.Color) {
	ui.Row(c).Padding(4, 8).Radius(5).Background(p.Hover).AlignItems(ui.Center).Children(func() { ui.Text(c, label).FontSize(12).TextColor(color).SingleLine() })
}
