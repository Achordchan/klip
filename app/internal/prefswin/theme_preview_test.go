package prefswin

import (
	"klip/internal/model"
	"klip/internal/prefs"
	"testing"
)

func TestThemePreviewDoesNotPersistUntilSave(t *testing.T) {
	r := newRig(t, "")
	saved := r.set.Get().Theme
	var calls []string
	r.m.PreviewTheme = func(v *string) {
		if v == nil {
			calls = append(calls, "restore")
		} else {
			calls = append(calls, *v)
		}
	}
	r.m.selectTheme(prefs.Option{Value: "Dark", Display: "深色"})
	if r.set.Get().Theme != saved || len(calls) != 1 || calls[0] != "Dark" {
		t.Fatal("preview did not remain transient")
	}
	_ = r.set.Update(func(s *model.Settings) { s.CaptureImages = !s.CaptureImages })
	r.m.followSettings()
	if r.m.v.Theme.Value != "Dark" || len(calls) != 1 {
		t.Fatal("background update interrupted preview")
	}
	if !r.m.Save() || r.set.Get().Theme != "Dark" || calls[len(calls)-1] != "restore" {
		t.Fatal("save did not commit/release preview")
	}
}

func TestDiscardRestoresSavedThemeAndContinueKeepsPreview(t *testing.T) {
	r := newRig(t, "")
	var active string
	r.m.PreviewTheme = func(v *string) {
		if v == nil {
			active = "saved"
		} else {
			active = *v
		}
	}
	r.m.selectTheme(prefs.Option{Value: "Light", Display: "浅色"})
	r.m.RequestClose()
	r.tt.Frame()
	r.click("继续编辑")
	if active != "Light" || r.host.closed != 0 {
		t.Fatal("continue discarded preview")
	}
	r.m.RequestClose()
	r.tt.Frame()
	r.click("放弃修改")
	if active != "saved" || r.m.Dirty() || r.host.closed != 1 {
		t.Fatal("discard did not restore saved theme")
	}
}

func TestInvalidSaveKeepsThemePreview(t *testing.T) {
	r := newRig(t, "")
	r.m.selectTheme(prefs.Option{Value: "Dark", Display: "深色"})
	r.m.v.MaxItems = "invalid"
	if r.m.Save() || !r.m.previewActive || r.set.Get().Theme == "Dark" {
		t.Fatal("failed save committed or cleared preview")
	}
}
