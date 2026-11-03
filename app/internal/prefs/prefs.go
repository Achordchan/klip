// Package prefs holds the rules of the preferences window's settings, which
// are in PreferencesWindowViewModel: what is written when a box is ticked or
// a number typed, how numbers are kept in range, and what opening the window
// repairs. The window only shows the values and calls these.
package prefs

import (
	"fmt"
	"strconv"
	"strings"

	"klip/internal/autostart"
	"klip/internal/model"
	"klip/internal/settings"
)

// DefaultCaptureFileExtensions is what a blank list is replaced by; the old
// default (before md, csv, images and archives were added) is replaced too.
const (
	DefaultCaptureFileExtensions = ".pdf,.ppt,.pptx,.doc,.docx,.xls,.xlsx,.txt,.md,.csv,.zip,.rar,.7z,.png,.jpg,.jpeg,.gif,.bmp,.webp"
	oldDefaultCaptureExtensions  = ".pdf,.ppt,.pptx,.doc,.docx,.xls,.xlsx,.txt"
)

// Option is a choice in a list: the value stored, the words shown.
type Option struct{ Value, Display string }

// Themes and ShelfModifiers are the choices of the window.
var (
	Themes = []Option{{"System", "跟随系统"}, {"Dark", "深色"}, {"Light", "浅色"}}

	ShelfModifiers = []Option{{"Ctrl", "Ctrl（推荐）"}, {"Alt", "Alt"}, {"Shift", "Shift"}}
)

// Controller changes the settings for the window.
type Controller struct {
	Settings  *settings.Manager
	Autostart autostart.Service
}

// View is what the window shows, the numbers as the text of their boxes.
type View struct {
	StartWithWindows        bool
	Theme                   Option
	MergeDuplicates         bool
	ExcludePinnedFromLimits bool
	MaxItems                string
	MaxMegabytes            string
	CaptureText             bool
	CaptureImages           bool
	CaptureFiles            bool
	CaptureFileExtensions   string
	CaptureFileMaxMegabytes string
	ShelfEnabled            bool
	ShelfModifier           Option
}

func pick(list []Option, value string) Option {
	for _, o := range list {
		if strings.EqualFold(o.Value, value) {
			return o
		}
	}
	return list[0]
}

// Open is ReloadFromSettings: it repairs what C# repairs when the window
// opens — a blank or old-default extension list becomes the default, and the
// "start with Windows" setting follows what the system says — and returns
// what to show. A shelf modifier "Disabled" shows the shelf as off.
func (c *Controller) Open() View {
	st := c.Settings.Get()

	ext := st.CaptureFileExtensions
	norm := normalizeList(ext)
	if norm == "" || strings.EqualFold(norm, normalizeList(oldDefaultCaptureExtensions)) {
		ext = DefaultCaptureFileExtensions
		_ = c.Settings.Update(func(s *model.Settings) { s.CaptureFileExtensions = ext })
		st = c.Settings.Get()
	}

	enabled := c.Autostart.IsEnabled()
	if st.StartWithWindows != enabled {
		_ = c.Settings.Update(func(s *model.Settings) { s.StartWithWindows = enabled })
		st = c.Settings.Get()
	}

	return c.CurrentView()
}

// CurrentView reads current settings without normalizing or writing them.
func (c *Controller) CurrentView() View {
	st := c.Settings.Get()
	enabled := c.Autostart.IsEnabled()
	ext := st.CaptureFileExtensions
	shelf := st.ShelfEnabled
	if strings.EqualFold(st.ShelfTriggerModifier, "Disabled") {
		shelf = false
	}
	return View{
		StartWithWindows:        enabled,
		Theme:                   pick(Themes, st.Theme),
		MergeDuplicates:         st.MergeDuplicates,
		ExcludePinnedFromLimits: st.ExcludePinnedFromLimits,
		MaxItems:                strconv.Itoa(int(st.MaxItems)),
		MaxMegabytes:            strconv.Itoa(int(st.MaxMegabytes)),
		CaptureText:             st.CaptureText,
		CaptureImages:           st.CaptureImages,
		CaptureFiles:            st.CaptureFiles,
		CaptureFileExtensions:   ext,
		CaptureFileMaxMegabytes: strconv.Itoa(int(st.CaptureFileMaxMegabytes)),
		ShelfEnabled:            shelf,
		ShelfModifier:           pick(ShelfModifiers, st.ShelfTriggerModifier),
	}
}

// normalizeList is NormalizeExtensionList: split at commas, trim, lower case.
func normalizeList(v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	var parts []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, strings.ToLower(p))
		}
	}
	return strings.Join(parts, ",")
}

// Effect says what a change did: Changed that a setting was written (the
// window says "设置已生效", after a pause for numbers typed), Text the text the
// box must now hold when the value was put right, Err why it failed.
type Effect struct {
	Changed bool
	Text    string
	SetText bool
	Err     error
}

func (c *Controller) set(same func(model.Settings) bool, f func(*model.Settings)) Effect {
	if same(c.Settings.Get()) {
		return Effect{}
	}
	if err := c.Settings.Update(f); err != nil {
		return Effect{Changed: true, Err: err}
	}
	return Effect{Changed: true}
}

// SetTheme stores "System", "Dark" or "Light".
func (c *Controller) SetTheme(v string) Effect {
	return c.set(func(s model.Settings) bool { return strings.EqualFold(s.Theme, v) },
		func(s *model.Settings) { s.Theme = v })
}

// SetStartWithWindows turns the autostart on or off in the system first; if
// that fails the setting is left alone and the error says why.
func (c *Controller) SetStartWithWindows(on bool) Effect {
	if c.Autostart.IsEnabled() == on && c.Settings.Get().StartWithWindows == on {
		return Effect{}
	}
	if err := c.Autostart.SetEnabled(on); err != nil {
		return Effect{Err: fmt.Errorf("开机启动设置失败：%w", err)}
	}
	return c.set(func(model.Settings) bool { return false }, func(s *model.Settings) { s.StartWithWindows = on })
}

func (c *Controller) SetCaptureText(on bool) Effect {
	return c.set(func(s model.Settings) bool { return s.CaptureText == on }, func(s *model.Settings) { s.CaptureText = on })
}

func (c *Controller) SetCaptureImages(on bool) Effect {
	return c.set(func(s model.Settings) bool { return s.CaptureImages == on }, func(s *model.Settings) { s.CaptureImages = on })
}

func (c *Controller) SetCaptureFiles(on bool) Effect {
	return c.set(func(s model.Settings) bool { return s.CaptureFiles == on }, func(s *model.Settings) { s.CaptureFiles = on })
}

func (c *Controller) SetMergeDuplicates(on bool) Effect {
	return c.set(func(s model.Settings) bool { return s.MergeDuplicates == on }, func(s *model.Settings) { s.MergeDuplicates = on })
}

func (c *Controller) SetExcludePinnedFromLimits(on bool) Effect {
	return c.set(func(s model.Settings) bool { return s.ExcludePinnedFromLimits == on },
		func(s *model.Settings) { s.ExcludePinnedFromLimits = on })
}

func (c *Controller) SetShelfEnabled(on bool) Effect {
	return c.set(func(s model.Settings) bool {
		return s.ShelfEnabled == on && !(on && strings.EqualFold(s.ShelfTriggerModifier, "Disabled"))
	}, func(s *model.Settings) {
		s.ShelfEnabled = on
		if on && strings.EqualFold(s.ShelfTriggerModifier, "Disabled") {
			// "Disabled" is shown as off with Ctrl picked: turning it on must
			// leave a modifier the hook can use.
			s.ShelfTriggerModifier = model.DefaultSettings().ShelfTriggerModifier
		}
	})
}

func (c *Controller) SetShelfModifier(v string) Effect {
	return c.set(func(s model.Settings) bool { return strings.EqualFold(s.ShelfTriggerModifier, v) },
		func(s *model.Settings) { s.ShelfTriggerModifier = v })
}

// SetCaptureFileExtensions stores the text as typed.
func (c *Controller) SetCaptureFileExtensions(v string) Effect {
	return c.set(func(s model.Settings) bool { return s.CaptureFileExtensions == v },
		func(s *model.Settings) { s.CaptureFileExtensions = v })
}

// SetMaxItems stores a number above zero; anything else is ignored (the box
// keeps what was typed until it is a number).
func (c *Controller) SetMaxItems(text string) Effect {
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil || n <= 0 || n > 1<<30 {
		return Effect{}
	}
	return c.set(func(s model.Settings) bool { return int(s.MaxItems) == n }, func(s *model.Settings) { s.MaxItems = int32(n) })
}

// SetMaxMegabytes stores a number above zero and, as C# does after every
// edit of the box, brings the single-file limit down to it when it is higher
// (Text is then the new text of that box). It reports a change for the toast
// even when the number was not valid, as C# does.
func (c *Controller) SetMaxMegabytes(text string) Effect {
	var saveErr error
	if n, err := strconv.Atoi(strings.TrimSpace(text)); err == nil && n > 0 && n <= 1<<30 {
		saveErr = c.Settings.Update(func(s *model.Settings) {
			if int(s.MaxMegabytes) != n {
				s.MaxMegabytes = int32(n)
			}
		})
	}
	st := c.Settings.Get()
	e := Effect{Changed: true}
	if st.CaptureFileMaxMegabytes > st.MaxMegabytes {
		if err := c.Settings.Update(func(s *model.Settings) { s.CaptureFileMaxMegabytes = s.MaxMegabytes }); err != nil {
			saveErr = err
		}
		e.Text, e.SetText = strconv.Itoa(int(st.MaxMegabytes)), true
	}
	e.Err = saveErr
	return e
}

// SetCaptureFileMax stores the single-file limit: at least 1 and at most the
// history's size limit. A number put right is written back into the box
// (Text) and does not toast.
func (c *Controller) SetCaptureFileMax(text string) Effect {
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return Effect{}
	}
	n = max(1, n)
	if max := int(c.Settings.Get().MaxMegabytes); n > max {
		n = max
	}
	fixed := strconv.Itoa(n) != text
	e := c.set(func(s model.Settings) bool { return int(s.CaptureFileMaxMegabytes) == n },
		func(s *model.Settings) { s.CaptureFileMaxMegabytes = int32(n) })
	if fixed {
		return Effect{Changed: false, Text: strconv.Itoa(n), SetText: true, Err: e.Err}
	}
	return e
}
