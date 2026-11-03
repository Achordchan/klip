package prefswin

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"klip/internal/account"
	"klip/internal/model"
)

// KLIP_SHOT=dir go test -run TestShots ./internal/prefswin writes one PNG per
// page (KLIP_DARK=1 for the dark theme), for looking at.
func TestShots(t *testing.T) {
	dir := os.Getenv("KLIP_SHOT")
	if dir == "" {
		t.Skip("set KLIP_SHOT=dir")
	}
	r := newRig(t, "")
	if os.Getenv("KLIP_DARK") == "1" {
		r.tt.SetDark(true)
	}
	r.host.updates = func() {}
	r.m.sub = account.Subscription{Status: "订阅至 2026-11-19", Tier: "Mini", Storage: "12.5 MB / 1 GB", Retention: "30 天", Limit: "正常"}
	save := func(name string) {
		r.tt.Frame()
		f, err := os.Create(filepath.Join(dir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		png.Encode(f, r.tt.Image())
	}
	save("general")
	r.click("内容")
	save("content")
	r.click("货架")
	save("shelf")
	r.click("云同步")
	save("cloud-out")
	_ = r.set.Update(func(s *model.Settings) {
		s.AuthRefreshToken = model.Ptr("r")
		s.AuthUserEmail = model.Ptr("me@example.com")
	})
	r.m.Open()
	r.m.sub = account.Subscription{Status: "订阅至 2026-11-19", Tier: "Mini", Storage: "12.5 MB / 1 GB", Retention: "30 天", Limit: "正常"}
	r.tt.Frame()
	r.click("云同步")
	save("cloud-in")
	_ = r.set.Update(func(s *model.Settings) { s.AuthRefreshToken = nil; s.AuthUserEmail = nil; s.AuthAccessToken = nil })
	r.m.Open()
	r.tt.Frame()
	r.click("云同步")
	r.click("忘记密码")
	save("dialog")
}
