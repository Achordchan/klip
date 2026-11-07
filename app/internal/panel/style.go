package panel

import (
	"runtime"

	"github.com/egoist/mygo/ui"
)

// Shared typography is expressed in logical pixels; native DPI scaling remains
// enabled so text stays sharp without giving each window a different baseline.
const (
	FontBody      = 13
	FontCaption   = 12
	FontTitle     = 16
	WindowPadding = 16
	CardRadius    = 10
)

func ApplyTypography(c *ui.Context) {
	theme := *c.Theme()
	theme.FontSize = FontBody
	c.SetTheme(&theme)
}

// Windows' GDI surface does not composite alpha. Paint behind rounded content
// and let DWM clip the outer window, rather than exposing transparent black.
func WindowBackdrop(background ui.Color) ui.Color {
	if runtime.GOOS == "windows" {
		return background
	}
	return ui.Transparent
}
