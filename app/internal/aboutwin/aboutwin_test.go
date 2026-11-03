package aboutwin

import (
	"image/png"
	"os"
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestPlace(t *testing.T) {
	wa := [4]int{0, 0, 1920, 1080}
	if x, y := Place(500, 110, wa); x != 500-Width/2 || y != 110 {
		t.Fatalf("%d,%d", x, y)
	}
	if x, _ := Place(10, 110, wa); x != 0 {
		t.Fatalf("left edge: %d", x)
	}
	if x, y := Place(1900, 1070, wa); x != 1920-Width || y != 1080-Height {
		t.Fatalf("right and bottom edge: %d,%d", x, y)
	}
}

func TestLinksOpen(t *testing.T) {
	var opened []string
	closed := 0
	m := &Model{Version: "v1.0.11", Open: func(u string) { opened = append(opened, u) }, Close: func() { closed++ }}
	tt := ui.NewTester(m.View, Width, Height)
	for _, s := range []string{"关于 Klip", "v1.0.11", "Achord Chan", "Klip 作者", "联系信息", Email, "131 6023 5855", "Independent Builder", "项目主页", "隐私条款", "开源许可", "赞助作者"} {
		if !tt.HasText(s) {
			t.Fatalf("missing %q in %q", s, tt.Texts())
		}
	}
	for label, want := range map[string]string{"项目主页": ProjectURL, "隐私条款": PrivacyURL, "开源许可": LicenseURL, "赞助作者": SponsorURL, Email: "mailto:" + Email} {
		opened = nil
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
		if len(opened) != 1 || opened[0] != want {
			t.Errorf("%s opened %v, want %s", label, opened, want)
		}
	}
	if err := tt.Click("关闭"); err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatal("close button")
	}
}

func TestShot(t *testing.T) {
	out := os.Getenv("KLIP_SHOT")
	if out == "" {
		t.Skip("set KLIP_SHOT=file.png")
	}
	m := &Model{Version: "v1.0.11"}
	tt := ui.NewTester(m.View, Width, Height)
	if os.Getenv("KLIP_DARK") == "1" {
		tt.SetDark(true)
	}
	tt.Frame()
	f, _ := os.Create(out)
	defer f.Close()
	_ = png.Encode(f, tt.Image())
}
