package panel

import "github.com/egoist/mygo/ui"

// Palette is the set of colours of App.axaml, one for each theme variant. The
// windows of Klip draw with these and not with the colours of the toolkit's
// theme, as the .NET client does. (Avalonia writes #AARRGGBB, MyGo #RRGGBBAA:
// the translucent ones are turned around.)
type Palette struct {
	Background  ui.Color // AppBackground: the window
	Panel       ui.Color // AppPanelBackground: the bars above and below
	Control     ui.Color // AppControlBackground: buttons, inputs, tags
	Hover       ui.Color // AppControlHoverBackground
	Pressed     ui.Color // AppControlPressedBackground: also the chosen row
	Border      ui.Color // AppBorder
	Divider     ui.Color // AppDivider
	Text        ui.Color // AppForeground
	Muted       ui.Color // AppMutedForeground
	Muted2      ui.Color // AppMuted2Foreground
	PreviewBg   ui.Color // AppPreviewBackground
	PreviewBtn  ui.Color // AppPreviewButtonBackground
	ToastBg     ui.Color // AppToastBackground
	ToastText   ui.Color // AppToastForeground
	Star        ui.Color // the gold of a favourite
	UpdateBg    ui.Color // the orange of the update button
	UpdateEdge  ui.Color
	Blue        ui.Color // the save button
	Red         ui.Color // the clear button
	WorkingIcon ui.Color
	OKIcon      ui.Color
	ErrIcon     ui.Color
}

// PaletteFor returns the colours of the dark or the light variant.
func PaletteFor(dark bool) Palette {
	p := Palette{
		Star:        ui.Hex("#D8C05E"),
		UpdateBg:    ui.Hex("#F97316"),
		UpdateEdge:  ui.Hex("#EA580C"),
		Blue:        ui.Hex("#3B82F6"),
		Red:         ui.Hex("#EF4444"),
		WorkingIcon: ui.Hex("#F59E0B"),
		OKIcon:      ui.Hex("#10B981"),
		ErrIcon:     ui.Hex("#EF4444"),
		PreviewBtn:  ui.Hex("#00000066"),
	}
	if dark {
		p.Background, p.Panel = ui.Hex("#1E1E1E"), ui.Hex("#171717")
		p.Control, p.Hover, p.Pressed = ui.Hex("#2A2A2A"), ui.Hex("#333333"), ui.Hex("#3A3A3A")
		p.Border, p.Divider = ui.Hex("#3A3A3A"), ui.Hex("#2A2A2A")
		p.Text, p.Muted, p.Muted2 = ui.Hex("#EAEAEA"), ui.Hex("#CFCFCF"), ui.Hex("#A8A8A8")
		p.PreviewBg = ui.Hex("#111111EE")
		p.ToastBg, p.ToastText = ui.Hex("#333333CC"), ui.Hex("#F0F0F0")
		return p
	}
	p.Background, p.Panel = ui.Hex("#F6F6F6"), ui.Hex("#FFFFFF")
	p.Control, p.Hover, p.Pressed = ui.Hex("#FFFFFF"), ui.Hex("#F0F0F0"), ui.Hex("#E8E8E8")
	p.Border, p.Divider = ui.Hex("#DDDDDD"), ui.Hex("#E5E5E5")
	p.Text, p.Muted, p.Muted2 = ui.Hex("#1A1A1A"), ui.Hex("#4A4A4A"), ui.Hex("#6A6A6A")
	p.PreviewBg = ui.Hex("#FFFFFFEE")
	p.ToastBg, p.ToastText = ui.Hex("#111111CC"), ui.Hex("#FFFFFF")
	return p
}
