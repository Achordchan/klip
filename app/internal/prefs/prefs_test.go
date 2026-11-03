package prefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"klip/internal/model"
	"klip/internal/settings"
)

type fakeAuto struct {
	on  bool
	err error
}

func (f *fakeAuto) IsEnabled() bool { return f.on }
func (f *fakeAuto) SetEnabled(on bool) error {
	if f.err != nil {
		return f.err
	}
	f.on = on
	return nil
}

func newCtl(t *testing.T) (*Controller, *settings.Manager, *fakeAuto, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	m, _ := settings.Open(path)
	a := &fakeAuto{}
	return &Controller{Settings: m, Autostart: a}, m, a, path
}

func TestOpenShowsDefaults(t *testing.T) {
	c, _, _, _ := newCtl(t)
	v := c.Open()
	if v.Theme.Value != "System" || v.MaxItems != "200" || v.MaxMegabytes != "300" || v.CaptureFileMaxMegabytes != "20" ||
		!v.CaptureText || !v.CaptureImages || !v.CaptureFiles || !v.MergeDuplicates || !v.ExcludePinnedFromLimits ||
		!v.ShelfEnabled || v.ShelfModifier.Value != "Ctrl" || v.StartWithWindows || v.CaptureFileExtensions != DefaultCaptureFileExtensions {
		t.Fatalf("%+v", v)
	}
}

func TestOpenRepairsExtensions(t *testing.T) {
	for name, in := range map[string]string{
		"blank":       "  ",
		"old default": " .PDF, .ppt,.pptx,.doc,.docx,.xls,.xlsx,.txt ",
	} {
		c, m, _, _ := newCtl(t)
		_ = m.Update(func(s *model.Settings) { s.CaptureFileExtensions = in })
		v := c.Open()
		if v.CaptureFileExtensions != DefaultCaptureFileExtensions || m.Get().CaptureFileExtensions != DefaultCaptureFileExtensions {
			t.Errorf("%s: %q / %q", name, v.CaptureFileExtensions, m.Get().CaptureFileExtensions)
		}
	}
	c, m, _, _ := newCtl(t)
	_ = m.Update(func(s *model.Settings) { s.CaptureFileExtensions = ".txt,.pdf" })
	if v := c.Open(); v.CaptureFileExtensions != ".txt,.pdf" {
		t.Fatalf("the user's own list was changed: %q", v.CaptureFileExtensions)
	}
}

func TestOpenSyncsAutostartFromSystem(t *testing.T) {
	c, m, a, _ := newCtl(t)
	a.on = true // 1.0.10 had turned it on
	v := c.Open()
	if !v.StartWithWindows || !m.Get().StartWithWindows {
		t.Fatalf("the system's state is the truth: %+v %v", v, m.Get().StartWithWindows)
	}
	a.on = false // the user removed it in the system settings
	if v := c.Open(); v.StartWithWindows || m.Get().StartWithWindows {
		t.Fatal("not followed")
	}
}

func TestOpenShelfDisabledModifier(t *testing.T) {
	c, m, _, _ := newCtl(t)
	_ = m.Update(func(s *model.Settings) { s.ShelfTriggerModifier = "Disabled" })
	v := c.Open()
	if v.ShelfEnabled || v.ShelfModifier.Value != "Ctrl" {
		t.Fatalf("%+v", v)
	}
	_ = m.Update(func(s *model.Settings) { s.ShelfTriggerModifier = "alt" })
	if v := c.Open(); v.ShelfModifier.Value != "Alt" {
		t.Fatalf("case-insensitive: %+v", v.ShelfModifier)
	}
}

func TestToggles(t *testing.T) {
	c, m, _, path := newCtl(t)
	type tc struct {
		name string
		set  func(bool) Effect
		get  func(model.Settings) bool
	}
	for _, x := range []tc{
		{"text", c.SetCaptureText, func(s model.Settings) bool { return s.CaptureText }},
		{"images", c.SetCaptureImages, func(s model.Settings) bool { return s.CaptureImages }},
		{"files", c.SetCaptureFiles, func(s model.Settings) bool { return s.CaptureFiles }},
		{"merge", c.SetMergeDuplicates, func(s model.Settings) bool { return s.MergeDuplicates }},
		{"pinned", c.SetExcludePinnedFromLimits, func(s model.Settings) bool { return s.ExcludePinnedFromLimits }},
		{"shelf", c.SetShelfEnabled, func(s model.Settings) bool { return s.ShelfEnabled }},
	} {
		if e := x.set(true); e.Changed {
			t.Errorf("%s: setting what is already so is a change", x.name)
		}
		if e := x.set(false); !e.Changed || e.Err != nil || x.get(m.Get()) {
			t.Errorf("%s: off: %+v", x.name, e)
		}
		if e := x.set(false); e.Changed {
			t.Errorf("%s: twice", x.name)
		}
		if e := x.set(true); !e.Changed || !x.get(m.Get()) {
			t.Errorf("%s: on: %+v", x.name, e)
		}
	}
	if e := c.SetTheme("Dark"); !e.Changed || m.Get().Theme != "Dark" {
		t.Errorf("theme: %+v", e)
	}
	if e := c.SetTheme("dark"); e.Changed {
		t.Errorf("theme, same in another case: %+v", e)
	}
	if e := c.SetShelfModifier("Alt"); !e.Changed || m.Get().ShelfTriggerModifier != "Alt" {
		t.Errorf("modifier: %+v", e)
	}
	if e := c.SetCaptureFileExtensions(" .A , .b"); !e.Changed || m.Get().CaptureFileExtensions != " .A , .b" {
		t.Errorf("extensions are kept as typed: %+v", e)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"Theme":"Dark"`) || !strings.Contains(string(data), `"ShelfTriggerModifier":"Alt"`) {
		t.Fatalf("not written: %s", data)
	}
}

func TestStartWithWindows(t *testing.T) {
	c, m, a, _ := newCtl(t)
	if e := c.SetStartWithWindows(true); !e.Changed || e.Err != nil || !a.on || !m.Get().StartWithWindows {
		t.Fatalf("%+v %v", e, a.on)
	}
	if e := c.SetStartWithWindows(true); e.Changed {
		t.Fatal("twice")
	}
	a.err = errors.New("access denied")
	e := c.SetStartWithWindows(false)
	if e.Err == nil || !strings.Contains(e.Err.Error(), "access denied") || !m.Get().StartWithWindows || !a.on {
		t.Fatalf("a failure must leave the setting as it was: %+v on=%v", e, a.on)
	}
}

func TestNumbers(t *testing.T) {
	c, m, _, _ := newCtl(t)
	for _, bad := range []string{"", "abc", "0", "-5", "1.5", "99999999999"} {
		if e := c.SetMaxItems(bad); e.Changed || m.Get().MaxItems != 200 {
			t.Errorf("SetMaxItems(%q): %+v, now %d", bad, e, m.Get().MaxItems)
		}
	}
	if e := c.SetMaxItems(" 50 "); !e.Changed || m.Get().MaxItems != 50 {
		t.Errorf("SetMaxItems: %+v", e)
	}
	if e := c.SetMaxItems("50"); e.Changed {
		t.Errorf("same number: %+v", e)
	}
}

func TestMaxMegabytesBringsFileLimitDown(t *testing.T) {
	c, m, _, _ := newCtl(t) // history 300, file 20
	if e := c.SetMaxMegabytes("10"); !e.Changed || !e.SetText || e.Text != "10" {
		t.Fatalf("%+v", e)
	}
	if st := m.Get(); st.MaxMegabytes != 10 || st.CaptureFileMaxMegabytes != 10 {
		t.Fatalf("%+v", st)
	}
	if e := c.SetMaxMegabytes("500"); !e.Changed || e.SetText {
		t.Fatalf("raising leaves the file limit alone: %+v", e)
	}
	if st := m.Get(); st.MaxMegabytes != 500 || st.CaptureFileMaxMegabytes != 10 {
		t.Fatalf("%+v", st)
	}
	// Not a number: nothing stored, still a toast (C# shows it after every edit).
	if e := c.SetMaxMegabytes("x"); !e.Changed || m.Get().MaxMegabytes != 500 {
		t.Fatalf("%+v", e)
	}
}

func TestCaptureFileMaxIsKeptInRange(t *testing.T) {
	c, m, _, _ := newCtl(t) // history 300
	if e := c.SetCaptureFileMax("0"); e.Changed || !e.SetText || e.Text != "1" || m.Get().CaptureFileMaxMegabytes != 1 {
		t.Fatalf("below 1: %+v %d", e, m.Get().CaptureFileMaxMegabytes)
	}
	if e := c.SetCaptureFileMax("1000"); e.Changed || !e.SetText || e.Text != "300" || m.Get().CaptureFileMaxMegabytes != 300 {
		t.Fatalf("above the history limit: %+v %d", e, m.Get().CaptureFileMaxMegabytes)
	}
	if e := c.SetCaptureFileMax("40"); !e.Changed || e.SetText || m.Get().CaptureFileMaxMegabytes != 40 {
		t.Fatalf("in range: %+v", e)
	}
	if e := c.SetCaptureFileMax("40"); e.Changed {
		t.Fatalf("same: %+v", e)
	}
	if e := c.SetCaptureFileMax("abc"); e.Changed || e.SetText || m.Get().CaptureFileMaxMegabytes != 40 {
		t.Fatalf("not a number: %+v", e)
	}
	// Written back as typed when it is already right but padded: " 7" is 7.
	if e := c.SetCaptureFileMax(" 7"); e.Changed || !e.SetText || e.Text != "7" || m.Get().CaptureFileMaxMegabytes != 7 {
		t.Fatalf("padded: %+v", e)
	}
}

func TestEnablingShelfReplacesDisabledModifier(t *testing.T) {
	c, m, _, _ := newCtl(t)
	_ = m.Update(func(s *model.Settings) { s.ShelfTriggerModifier = "Disabled"; s.ShelfEnabled = true })
	c.SetShelfEnabled(true)
	if got := m.Get().ShelfTriggerModifier; got != "Ctrl" {
		t.Fatalf("modifier = %q, want the one that is shown", got)
	}
}

func TestAutostartReconcilesPortableSetting(t *testing.T) {
	c, m, a, _ := newCtl(t)
	if err := m.Update(func(s *model.Settings) { s.StartWithWindows = true }); err != nil {
		t.Fatal(err)
	}
	if e := c.SetStartWithWindows(true); e.Err != nil || !a.on {
		t.Fatalf("OS entry not enabled: %+v", e)
	}
}
