package panel

import (
	"github.com/egoist/mygo/ui"
	"runtime"
	"testing"
)

func TestWindowBackdropMatchesNativeSurface(t *testing.T) {
	for _, dark := range []bool{false, true} {
		bg := PaletteFor(dark).Background
		want := ui.Transparent
		if runtime.GOOS == "windows" {
			want = bg
		}
		if got := WindowBackdrop(bg); got != want {
			t.Fatalf("backdrop=%v want %v", got, want)
		}
	}
}
