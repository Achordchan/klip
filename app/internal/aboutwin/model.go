// Package aboutwin is the window about the author, which the "About" chip of
// the main window opens (AuthorInfoWindow).
package aboutwin

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/egoist/mygo/ui"
)

// Addresses the window opens (AuthorInfoWindow).
const (
	Email      = "achordchan@gmail.com"
	ProjectURL = "https://gitee.com/Achordchan/maccy"
	PrivacyURL = "https://gitee.com/Achordchan/maccy/blob/master/docs/privacy.md"
	LicenseURL = "https://gitee.com/Achordchan/maccy/blob/master/LICENSE"
	SponsorURL = "https://gitee.com/Achordchan/maccy/blob/master/docs/sponsor.md"
	AvatarURL  = "https://avatars.githubusercontent.com/u/179492542?v=4"
)

// Size of the window.
const Width, Height = 484, 304

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

// LoadAvatar fetches the picture of the author, once, in the background; the
// window shows the letters until it arrives, and without it when it never
// does.
func (m *Model) LoadAvatar() {
	m.mu.Lock()
	if m.loaded {
		m.mu.Unlock()
		return
	}
	m.loaded = true
	m.mu.Unlock()
	go func() {
		cl := http.Client{Timeout: 10 * time.Second}
		resp, err := cl.Get(AvatarURL)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return
		}
		bm, err := ui.DecodeBitmap(data)
		if err != nil || bm == nil {
			return
		}
		m.mu.Lock()
		m.avatar = bm
		m.mu.Unlock()
		if m.Redraw != nil {
			m.Redraw()
		}
	}()
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
	if dark {
		return palette{
			Window: ui.Hex("#1E1E1E"), Panel: ui.Hex("#171717"), Border: ui.Hex("#333333"),
			Control: ui.Hex("#262626"), Hover: ui.Hex("#303030"), Pressed: ui.Hex("#353535"),
			Text: ui.Hex("#EAEAEA"), Muted: ui.Hex("#A8A8A8"), Dim: ui.Hex("#7A7A7A"),
			Accent: ui.Hex("#E0A23B"), AccentSoft: ui.Hex("#E0A23B33"), Icon: ui.Hex("#CFCFCF"), Line: ui.Hex("#333333"),
		}
	}
	return palette{
		Window: ui.Hex("#F6F6F6"), Panel: ui.Hex("#FFFFFF"), Border: ui.Hex("#E1E1E1"),
		Control: ui.Hex("#FFFFFF"), Hover: ui.Hex("#F1F1F1"), Pressed: ui.Hex("#E9E9E9"),
		Text: ui.Hex("#1A1A1A"), Muted: ui.Hex("#5E5E5E"), Dim: ui.Hex("#7A7A7A"),
		Accent: ui.Hex("#B87818"), AccentSoft: ui.Hex("#B8781822"), Icon: ui.Hex("#555555"), Line: ui.Hex("#E7E7E7"),
	}
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
	p := paletteFor(c.Theme().Dark)
	c.Root().Background(ui.Transparent)
	m.mu.Lock()
	avatar := m.avatar
	m.mu.Unlock()

	ui.Column(c).Fill().Padding(18).Gap(16).Background(p.Window).Border(1, p.Border).Radius(16).Clip().Children(func() {
		// title bar
		ui.Row(c).FillWidth().Height(36).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "关于 Klip").FontSize(18).FontWeight(600).TextColor(p.Text).Grow(1)
			ui.Row(c).Height(28).MinWidth(70).Padding(0, 14).Center().Background(p.Control).Border(1, p.Border).Radius(10).Children(func() {
				ui.Text(c, m.Version).FontSize(14).TextColor(p.Muted)
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
		ui.Row(c).FillWidth().Height(154).Gap(14).Children(func() {
			ui.Column(c).Width(136).FillHeight().Padding(14).Gap(8).Center().Background(p.Panel).Border(1, p.Border).Radius(13).Children(func() {
				ui.Row(c).Size(72, 72).Radius(18).Clip().Border(1, p.Border).Center().
					Gradient(ui.Hex("#383838"), ui.Hex("#161616"), 135).Children(func() {
					if avatar != nil {
						ui.Image(c, avatar).Size(72, 72).Fit(ui.Cover)
					} else {
						ui.Text(c, "A+").FontSize(22).FontWeight(600).TextColor(p.Accent)
					}
				})
				ui.Column(c).Gap(2).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "Achord Chan").FontSize(15).FontWeight(600).TextColor(p.Text)
					ui.Text(c, "Klip 作者").FontSize(12).TextColor(p.Muted)
				})
			})
			ui.Column(c).Grow(1).FillHeight().Padding(14, 16).Gap(3).Background(p.Panel).Border(1, p.Border).Radius(13).Children(func() {
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
					ui.Text(c, Email).FontSize(15).FontWeight(600).TextColor(p.Text)
				})
				if mail.Clicked() {
					m.open("mailto:" + Email)
				}
				ui.Box(c).FillWidth().Height(1).Background(p.Line)
				ui.Row(c).FillWidth().Height(30).Gap(12).AlignItems(ui.Center).Children(func() {
					ui.Image(c, icon("phone", pathPhone, p.Icon, 1.8)).Size(20, 20)
					ui.Text(c, "131 6023 5855").FontSize(15).FontWeight(600).TextColor(p.Text)
				})
				ui.Box(c).FillWidth().Height(1).Background(p.Line)
				ui.Row(c).FillWidth().Height(22).Gap(12).AlignItems(ui.Center).Children(func() {
					ui.Image(c, icon("person", pathPerson, p.Icon, 1.8)).Size(18, 18)
					ui.Text(c, "Independent Builder").FontSize(13).FontWeight(600).TextColor(p.Muted)
				})
			})
		})

		// links
		ui.Row(c).FillWidth().Height(46).Gap(10).Children(func() {
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
				b.Grow(1).FillHeight().Padding(0, 9).Gap(6).AlignItems(ui.Center).Radius(11).Border(1, p.Border).Background(bg).Label(l.label).Children(func() {
					ui.Image(c, icon(l.name, l.path, p.Icon, 1.8)).Size(16, 16)
					ui.Text(c, l.label).FontSize(13).FontWeight(600).TextColor(p.Text).SingleLine()
				})
				if b.Clicked() {
					m.open(l.url)
				}
			}
		})
	})
}
