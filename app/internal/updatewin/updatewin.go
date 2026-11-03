// Package updatewin is the windows of an update: the check, the notice of a
// new version, the download and the messages (UpdateCheckWindow,
// UpdatePromptWindow, UpdateProgressWindow and MessageWindow of the C# client).
// They follow the state of the update.Coordinator, one window at a time.
package updatewin

import (
	"context"
	"fmt"
	"strings"

	"github.com/egoist/mygo/ui"

	"klip/internal/panel"
	"klip/internal/update"
)

// Kind is which of the windows the state calls for.
type Kind string

const (
	None     Kind = ""
	Check    Kind = "check"
	Prompt   Kind = "prompt"
	Progress Kind = "progress"
	Message  Kind = "message"
)

// Sizes of the windows (the Width and Height of the .axaml files).
var Sizes = map[Kind][2]int{
	Check: {420, 148}, Prompt: {520, 360}, Progress: {520, 170}, Message: {480, 220},
}

// Host is what the windows ask of the program around it.
type Host interface {
	// Quit ends the program (a mandatory update that is declined).
	Quit()
}

// Model is the state of the windows. A window shows only while the user's
// own request is under way: a version that a silent check found is known to
// the main window's button, and nothing more.
type Model struct {
	Coordinator *update.Coordinator
	Host        Host
	// Go runs f in the background (go f()); tests run it at once.
	Go func(f func())
	// Quitting says that the program is quitting (may be nil): windows then
	// close for good.
	Quitting func() bool

	active bool
}

// New returns a Model.
func New(c *update.Coordinator, h Host) *Model {
	return &Model{Coordinator: c, Host: h, Go: func(f func()) { go f() }}
}

// Active says that a request of the user is under way.
func (m *Model) Active() bool { return m.active }

// Kind is the window that has to be on screen now.
func (m *Model) Kind() Kind {
	if !m.active && !m.mandatory() {
		return None
	}
	switch m.Coordinator.State().Phase {
	case update.Checking:
		return Check
	case update.Available:
		return Prompt
	case update.Downloading, update.Applying:
		return Progress
	case update.UpToDateNow, update.Problem:
		return Message
	}
	return None
}

// mandatory says that an update that must be installed is known: until it is
// installed (or the program quit) its window stays, whoever asked for it.
func (m *Model) mandatory() bool { return m.Coordinator.MustUpdate() != nil }

// Title is the title of the window of a kind.
func (m *Model) Title(k Kind) string {
	st := m.Coordinator.State()
	switch k {
	case Prompt:
		if st.Info != nil && m.mandatory() {
			return "需要更新"
		}
		return "发现新版本"
	case Progress:
		return "正在下载更新"
	case Message:
		return st.Title
	}
	return "检查更新"
}

// CheckNow is the user asking for a check (the tray's 检测更新...).
func (m *Model) CheckNow() {
	m.active = true
	m.Go(func() { m.Coordinator.Check(context.Background()) })
}

// ApplyNow is the user pressing the update button of the main window: the
// download starts at once, without the notice (ApplyAvailableUpdateAsync).
func (m *Model) ApplyNow() {
	m.active = true
	if m.Coordinator.Available() == nil {
		m.CheckNow()
		return
	}
	m.apply()
}

func (m *Model) apply() {
	m.Go(func() { m.Coordinator.Apply(context.Background()) })
}

// Finish ends the user's request: a message is read, a notice is put off.
func (m *Model) Finish() {
	m.Coordinator.Dismiss()
	m.active = false
}

// Declined is the window of a notice closed, or its cancel button: for an
// update that must be installed, that ends the program.
func (m *Model) Declined() {
	if m.mandatory() {
		m.Host.Quit()
		return
	}
	m.Finish()
}

// WindowClosed is for the close button of a window of the system.
func (m *Model) WindowClosed(k Kind) {
	switch k {
	case Prompt:
		m.Declined()
	case Message:
		if m.mandatory() {
			m.Host.Quit() // a mandatory update has no way out but the update and leaving
			return
		}
		m.Finish()
	}
}

// ---- views

// card is the Border of the windows: padding 16 over the colour of the
// window.
func frame(c *ui.Context, p panel.Palette, body func()) {
	c.Root().Background(p.Background)
	ui.Column(c).Fill().Padding(16).Gap(10).Background(p.Background).Children(body)
}

func button(c *ui.Context, p panel.Palette, label string) bool {
	return panel.Button(c, p, label, 5, 11, 14).Label(label).Clicked()
}

func primary(c *ui.Context, label string) bool { return ui.PrimaryButton(c, label).Clicked() }

func footer(c *ui.Context, p panel.Palette, buttons func()) {
	ui.Row(c).Grow(1)
	buttonRow(c, p, buttons)
}

// buttonRow is the line and the buttons at the foot of a window, under what
// grows to fill the rest of it.
func buttonRow(c *ui.Context, p panel.Palette, buttons func()) {
	ui.Box(c).FillWidth().Height(1).Background(p.Divider).Margin(10, 0, 0)
	ui.Row(c).FillWidth().Gap(10).Justify(ui.End).Children(buttons)
}

// ViewCheck is the small window of a check under way.
func (m *Model) ViewCheck(c *ui.Context) {
	p := panel.PaletteFor(c.Theme().Dark)
	c.Root().Background(ui.Transparent)
	st := m.Coordinator.State()
	msg := st.Message
	if msg == "" {
		msg = "正在连接更新服务器，请稍候..."
	}
	ui.Column(c).Fill().Padding(16).Gap(12).Background(p.Panel).Border(1, p.Border).Radius(14).Children(func() {
		ui.Text(c, "检查更新中").FontSize(16).FontWeight(600).TextColor(p.Text)
		ui.Progress(c, -1).FillWidth().Height(10).Label("检查更新")
		ui.Text(c, msg).FontSize(12).TextColor(p.Muted).Wrap()
	})
}

// ViewPrompt is the notice of a new version.
func (m *Model) ViewPrompt(c *ui.Context) {
	p := panel.PaletteFor(c.Theme().Dark)
	info := m.Coordinator.State().Info
	if info == nil {
		return
	}
	frame(c, p, func() {
		ui.Text(c, m.Title(Prompt)).FontSize(16).FontWeight(600).TextColor(p.Text)
		// The notes take what the window has left above the buttons: the window
		// is of a fixed size, and the buttons must stay inside it.
		ui.Column(c).FillWidth().Grow(1).MinHeight(0).Padding(12).Gap(8).Background(p.Panel).Border(1, p.Border).Radius(10).Children(func() {
			ui.Text(c, "版本："+info.Version).TextColor(p.Text)
			ui.Text(c, "更新内容：").TextColor(p.Muted)
			ui.Column(c).FillWidth().Grow(1).MinHeight(0).Padding(10).Background(p.Control).Border(1, p.Border).Radius(10).Children(func() {
				notes := strings.TrimSpace(info.Notes)
				if notes == "" {
					notes = "（无）"
				}
				ui.Scroll(c).FillWidth().Grow(1).MinHeight(0).Children(func() {
					ui.Text(c, notes).TextColor(p.Text).Wrap().FillWidth().Selectable()
				})
			})
		})
		buttonRow(c, p, func() {
			mandatory := m.mandatory()
			if !mandatory && button(c, p, "稍后") {
				m.Finish()
			}
			cancel := "取消"
			if mandatory {
				cancel = "退出"
			}
			if button(c, p, cancel) {
				m.Declined()
			}
			if primary(c, "立即更新") {
				m.apply()
			}
		})
	})
}

// ViewProgress is the download.
func (m *Model) ViewProgress(c *ui.Context) {
	p := panel.PaletteFor(c.Theme().Dark)
	st := m.Coordinator.State()
	frame(c, p, func() {
		status := "正在下载..."
		if st.Phase == update.Applying {
			status = st.Message
		}
		ui.Text(c, status).FontSize(13).TextColor(p.Text).Wrap()
		value, detail := -1.0, ""
		switch {
		case st.Total > 0:
			value = min(1, float64(st.Read)/float64(st.Total))
			detail = size(st.Read) + " / " + size(st.Total)
		case st.Read > 0:
			detail = size(st.Read)
		}
		ui.Progress(c, value).FillWidth().Height(14).Label("下载进度")
		ui.Text(c, detail).FontSize(12).TextColor(p.Muted)
		if st.Message != "" && st.Phase == update.Downloading {
			ui.Text(c, st.Message).FontSize(12).TextColor(p.Red).Wrap()
		}
	})
}

// ViewMessage is a message: that there is nothing newer, or what went wrong.
func (m *Model) ViewMessage(c *ui.Context) {
	p := panel.PaletteFor(c.Theme().Dark)
	st := m.Coordinator.State()
	frame(c, p, func() {
		ui.Text(c, st.Title).FontSize(16).FontWeight(600).TextColor(p.Text)
		ui.Column(c).FillWidth().Padding(12).Background(p.Panel).Border(1, p.Border).Radius(10).Children(func() {
			ui.Scroll(c).FillWidth().Height(60).Children(func() {
				ui.Text(c, st.Detail).TextColor(p.Text).Wrap().FillWidth().Selectable()
			})
		})
		footer(c, p, func() {
			if st.Phase == update.Problem && st.Info != nil {
				// After a failed look the known update can still be installed.
				retry := "重试"
				if st.Check {
					retry = "立即更新"
				}
				if button(c, p, retry) {
					m.apply()
				}
			}
			if m.mandatory() {
				if primary(c, "退出") {
					m.Host.Quit()
				}
			} else if primary(c, "确定") {
				m.Finish()
			}
		})
	})
}

// View is the view of the window the state calls for, for a window that is
// the only one (and for the tests).
func (m *Model) View(c *ui.Context) {
	switch m.Kind() {
	case Check:
		m.ViewCheck(c)
	case Prompt:
		m.ViewPrompt(c)
	case Progress:
		m.ViewProgress(c)
	case Message:
		m.ViewMessage(c)
	}
}

// size is UpdateProgressWindow.FormatBytes: two decimals.
func size(n int64) string {
	const kb = 1024.0
	const mb, gb = kb * 1024, kb * 1024 * 1024
	switch {
	case float64(n) >= gb:
		return fmt.Sprintf("%.2f GB", float64(n)/gb)
	case float64(n) >= mb:
		return fmt.Sprintf("%.2f MB", float64(n)/mb)
	case float64(n) >= kb:
		return fmt.Sprintf("%.2f KB", float64(n)/kb)
	}
	return fmt.Sprintf("%d B", n)
}
