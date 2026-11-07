// Package aboutwin is the window about the author, which the "About" chip of
// the main window opens (AuthorInfoWindow).
package aboutwin

import (
	_ "embed"
	"fmt"
	"sync"

	"github.com/egoist/mygo/ui"
	"klip/internal/panel"
)

// Addresses the window opens (AuthorInfoWindow).
const (
	Email      = "achordchan@gmail.com"
	ProjectURL = "https://github.com/Achordchan/klip"
	PrivacyURL = "https://github.com/Achordchan/klip/blob/main/docs/privacy.md"
	LicenseURL = "https://github.com/Achordchan/klip/blob/main/LICENSE"
	SponsorURL = "https://github.com/Achordchan/klip/blob/main/docs/sponsor.md"
)

// Size of the window.
const Width, Height = 484, 276

// Model is the content of the window.
type Model struct {
	// Version is shown in the chip beside the title, as "v1.0.11".
	Version string
	// Open opens an address with the default program.
	Open func(url string)
	// Close closes the window.
	Close func()
	// Redraw asks for a frame from any goroutine.
	Redraw func()

	mu     sync.Mutex
	avatar *ui.Bitmap
	loaded bool
}

//go:embed assets/author.jpg
var authorImage []byte

// LoadAvatar decodes the bundled author image once without network access.
func (m *Model) LoadAvatar() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded {
		return
	}
	m.avatar, _ = ui.DecodeBitmap(authorImage)
	m.loaded = true
}

func (m *Model) open(url string) {
	if m.Open != nil {
		m.Open(url)
	}
}

// palette is the set of colours of the window, which has its own (the
// AuthorWindowBackground family of AuthorInfoWindow.axaml).
type palette struct {
	Window, Panel, Border, Control, Hover, Pressed   ui.Color
	Text, Muted, Dim, Accent, AccentSoft, Icon, Line ui.Color
}

func paletteFor(dark bool) palette {
	p := panel.PaletteFor(dark)
	return palette{Window: p.Background, Panel: p.Panel, Border: p.Border, Control: p.Control, Hover: p.Hover, Pressed: p.Pressed, Text: p.Text, Muted: p.Muted, Dim: p.Muted2, Accent: p.Blue, AccentSoft: p.Hover, Icon: p.Muted, Line: p.Divider}
}

// ---- icons: the paths of the .NET window, drawn in the colour of the theme

var (
	iconMu    sync.Mutex
	iconCache = map[string]*ui.SVG{}
)

func hex(c ui.Color) string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

func icon(name, path string, color ui.Color, width float64) *ui.SVG {
	key := name + hex(color)
	iconMu.Lock()
	defer iconMu.Unlock()
	if s, ok := iconCache[key]; ok {
		return s
	}
	s := ui.MustParseSVG([]byte(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24">`+
		`<path d="%s" fill="none" stroke="%s" stroke-width="%g" stroke-linecap="round" stroke-linejoin="round"/></svg>`, path, hex(color), width)))
	iconCache[key] = s
	return s
}

const (
	pathMail   = "M 3 6.5 C 3 5.7 3.7 5 4.5 5 L 19.5 5 C 20.3 5 21 5.7 21 6.5 L 21 17.5 C 21 18.3 20.3 19 19.5 19 L 4.5 19 C 3.7 19 3 18.3 3 17.5 Z M 4 7 L 12 13 L 20 7"
	pathPhone  = "M 7.2 4.5 C 6.8 3.5 5.8 3 4.8 3.3 L 3.6 3.7 C 2.8 4 2.4 4.8 2.5 5.6 C 3.3 12.5 8.7 18 15.6 21.1 C 16.4 21.5 17.3 21.3 17.9 20.7 L 18.8 19.8 C 19.6 19 19.6 17.8 18.8 17 L 16.9 15.1 C 16.3 14.5 15.3 14.4 14.6 14.9 L 13.5 15.6 C 11.5 14.4 9.8 12.7 8.6 10.7 L 9.3 9.6 C 9.8 8.9 9.7 7.9 9.1 7.3 Z"
	pathPerson = "M 12 13 C 14.2 13 16 11.2 16 9 C 16 6.8 14.2 5 12 5 C 9.8 5 8 6.8 8 9 C 8 11.2 9.8 13 12 13 Z M 5 21 C 5.8 17.5 8.4 15.5 12 15.5 C 15.6 15.5 18.2 17.5 19 21"
	pathGlobe  = "M 12 21 C 17 21 21 17 21 12 C 21 7 17 3 12 3 C 7 3 3 7 3 12 C 3 17 7 21 12 21 Z M 3.5 12 L 20.5 12 M 12 3 C 9.6 5.5 8.5 8.5 8.5 12 C 8.5 15.5 9.6 18.5 12 21 M 12 3 C 14.4 5.5 15.5 8.5 15.5 12 C 15.5 15.5 14.4 18.5 12 21"
	pathShield = "M 12 3 L 20 6 L 20 11 C 20 15.6 17.2 19.2 12 21 C 6.8 19.2 4 15.6 4 11 L 4 6 Z M 9 12 L 11.2 14.2 L 15.5 9.7"
	pathCode   = "M 9 6 L 4 12 L 9 18 M 15 6 L 20 12 L 15 18 M 13.2 5 L 10.8 19"
	pathHeart  = "M 12 20 C 8 16.5 4.5 13.6 4.5 9.4 C 4.5 6.8 6.4 5 8.7 5 C 10 5 11.2 5.7 12 6.8 C 12.8 5.7 14 5 15.3 5 C 17.6 5 19.5 6.8 19.5 9.4 C 19.5 13.6 16 16.5 12 20 Z"
	pathClose  = "M 3 3 L 21 21 M 21 3 L 3 21"
)

// View is the window's content.
func (m *Model) View(c *ui.Context) {
	panel.ApplyTypography(c)
	p := paletteFor(c.Theme().Dark)
	c.Root().Background(panel.WindowBackdrop(p.Window))
	m.mu.Lock()
	avatar := m.avatar
	m.mu.Unlock()

	ui.Column(c).Fill().Padding(panel.WindowPadding).Gap(12).Background(p.Window).Border(1, p.Border).Radius(12).Clip().Children(func() {
		// title bar
		ui.Row(c).FillWidth().Height(32).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "关于 Klip").FontSize(panel.FontTitle).FontWeight(600).TextColor(p.Text).Grow(1)
			ui.Row(c).Height(28).MinWidth(70).Padding(0, 14).Center().Background(p.Control).Border(1, p.Border).Radius(10).Children(func() {
				ui.Text(c, m.Version).FontSize(panel.FontCaption).TextColor(p.Muted)
			})
			cl := ui.ButtonBase(c)
			bg := ui.Transparent
			if cl.Hovered() {
				bg = p.Hover
			}
			cl.Size(28, 28).Radius(8).Background(bg).Center().Label("关闭").Children(func() {
				ui.Image(c, icon("close", pathClose, p.Muted, 2.2)).Size(14, 14)
			})
			if cl.Clicked() && m.Close != nil {
				m.Close()
			}
		})

		// the author, and how to reach them
		ui.Row(c).FillWidth().Height(148).Gap(14).Children(func() {
			ui.Column(c).Width(136).FillHeight().Padding(14).Gap(8).Center().Background(p.Panel).Border(1, p.Border).Radius(panel.CardRadius).Children(func() {
				ui.Row(c).Size(64, 64).Radius(12).Clip().Border(1, p.Border).Center().
					Gradient(ui.Hex("#383838"), ui.Hex("#161616"), 135).Children(func() {
					if avatar != nil {
						ui.Image(c, avatar).Size(64, 64).Fit(ui.Cover)
					} else {
						ui.Text(c, "A+").FontSize(22).FontWeight(600).TextColor(p.Accent)
					}
				})
				ui.Column(c).Gap(2).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "Achord Chan").FontSize(panel.FontBody).TextColor(p.Text)
					ui.Text(c, "Klip 作者").FontSize(12).TextColor(p.Muted)
				})
			})
			ui.Column(c).Grow(1).FillHeight().Padding(14, 16).Gap(3).Background(p.Panel).Border(1, p.Border).Radius(panel.CardRadius).Children(func() {
				ui.Row(c).Height(20).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "联系信息").FontSize(12).FontWeight(600).TextColor(p.Muted)
				})
				ui.Box(c).FillWidth().Height(1)
				mail := ui.ButtonBase(c)
				bg := ui.Transparent
				if mail.Hovered() {
					bg = p.AccentSoft
				}
				mail.FillWidth().Height(30).Gap(12).Justify(ui.Start).AlignItems(ui.Center).Radius(8).Background(bg).Label(Email).Children(func() {
					ui.Image(c, icon("mail", pathMail, p.Icon, 1.8)).Size(20, 20)
					ui.Text(c, Email).FontSize(panel.FontBody).TextColor(p.Text)
				})
				if mail.Clicked() {
					m.open("mailto:" + Email)
				}
				ui.Box(c).FillWidth().Height(1).Background(p.Line)
				ui.Row(c).FillWidth().Height(30).Gap(12).AlignItems(ui.Center).Children(func() {
					ui.Image(c, icon("phone", pathPhone, p.Icon, 1.8)).Size(20, 20)
					ui.Text(c, "131 6023 5855").FontSize(panel.FontBody).TextColor(p.Text)
				})
				ui.Box(c).FillWidth().Height(1).Background(p.Line)
				ui.Row(c).FillWidth().Height(22).Gap(12).AlignItems(ui.Center).Children(func() {
					ui.Image(c, icon("person", pathPerson, p.Icon, 1.8)).Size(18, 18)
					ui.Text(c, "Independent Builder").FontSize(panel.FontCaption).TextColor(p.Muted)
				})
			})
		})

		// links
		ui.Row(c).FillWidth().Height(40).Gap(10).Children(func() {
			for _, l := range []struct{ name, label, path, url string }{
				{"globe", "项目主页", pathGlobe, ProjectURL},
				{"shield", "隐私条款", pathShield, PrivacyURL},
				{"code", "开源许可", pathCode, LicenseURL},
				{"heart", "赞助作者", pathHeart, SponsorURL},
			} {
				b := ui.ButtonBase(c)
				bg := p.Control
				switch {
				case b.Pressed():
					bg = p.Pressed
				case b.Hovered():
					bg = p.Hover
				}
				b.Grow(1).FillHeight().Padding(0, 9).Gap(6).AlignItems(ui.Center).Radius(panel.CardRadius).Border(1, p.Border).Background(bg).Label(l.label).Children(func() {
					ui.Image(c, icon(l.name, l.path, p.Icon, 1.8)).Size(16, 16)
					ui.Text(c, l.label).FontSize(panel.FontBody).TextColor(p.Text).SingleLine()
				})
				if b.Clicked() {
					m.open(l.url)
				}
			}
		})
	})
}
