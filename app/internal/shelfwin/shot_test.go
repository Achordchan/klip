package shelfwin

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
)

// KLIP_SHOT=dir go test -run TestShots ./internal/shelfwin writes pictures
// of the window (KLIP_DARK=1 for the dark theme).
func TestShots(t *testing.T) {
	dir := os.Getenv("KLIP_SHOT")
	if dir == "" {
		t.Skip("set KLIP_SHOT=dir")
	}
	files := t.TempDir()
	save := func(name string, m *Model, size int) {
		tt := ui.NewTester(m.View, size, size)
		if os.Getenv("KLIP_DARK") == "1" {
			tt.SetDark(true)
		}
		tt.Frame()
		f, _ := os.Create(filepath.Join(dir, name+".png"))
		defer f.Close()
		png.Encode(f, tt.Image())
	}
	save("empty", New(&fakeHost{}), CompactSize)
	one := New(&fakeHost{})
	one.AddFiles([]string{file(t, files, "季度报告.pdf", 3<<20)})
	save("one", one, CompactSize)
	many := New(&fakeHost{})
	many.AddFiles([]string{file(t, files, "季度报告.pdf", 3<<20), pngFile(t, files, "截图.png", 1920, 1080), file(t, files, "数据.xlsx", 52000), file(t, files, "录音.mp3", 4<<20)})
	many.Select(1, false)
	save("many", many, ExpandedSize)
	stack := New(&fakeHost{})
	stack.AddFiles([]string{file(t, files, "a.zip", 100), file(t, files, "b.zip", 200)})
	stack.FoldStack()
	save("stack", stack, CompactSize)
}
