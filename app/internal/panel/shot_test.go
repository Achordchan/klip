package panel

import (
	"image/png"
	"os"
	"testing"

	"klip/internal/app"
	"klip/internal/model"
)

// TestShot writes a picture of the window to $KLIP_SHOT (KLIP_DARK=1 for the
// dark colours): the way to look at it without a screen.
func TestShot(t *testing.T) {
	out := os.Getenv("KLIP_SHOT")
	if out == "" {
		t.Skip("set KLIP_SHOT=file.png")
	}
	r := newRig(t, "最近复制的一段文字，用来看排版是否正常", "func main() { fmt.Println(\"hello\") }",
		"https://example.com/some/long/url/that/goes/on/and/on/and/on/forever/and/ever", "第二行\n第三行 被合成一行显示", "plain text number five")
	items := r.h.Items()
	r.h.TogglePinned(items[1].ID)
	r.h.Add(model.Item{ID: "00000000-0000-0000-0000-0000000000aa", Kind: model.KindFileList, CapturedAt: items[0].CapturedAt,
		FilePaths: []string{"/Users/x/报告.pdf", "/Users/x/data.csv"}, CopyCount: 3, ApproxBytes: 2 << 20, ContentHash: model.Ptr("files:AA")})
	r.ho.update = os.Getenv("KLIP_UPDATE")
	switch os.Getenv("KLIP_SYNC") {
	case "working":
		r.ho.sync = app.SyncView{Cloud: true, Syncing: true, Progress: true, Text: "上传快照", Percent: 84}
	case "ok":
		r.ho.sync = app.SyncView{Cloud: true, Result: app.SyncOK}
	case "error":
		r.ho.sync = app.SyncView{Cloud: true, Result: app.SyncFailed}
	}
	r.m.Reset()
	if os.Getenv("KLIP_FAV") == "1" {
		r.m.sec = sectionFavorites
	}
	if os.Getenv("KLIP_DIALOG") == "note" {
		r.m.openNote(r.h.Items()[0])
	}
	if os.Getenv("KLIP_DIALOG") == "clear" {
		r.m.askClear()
	}
	if os.Getenv("KLIP_TOAST") != "" {
		r.m.ShowToast("已写回剪贴板")
	}
	if os.Getenv("KLIP_DARK") == "1" {
		r.tt.SetDark(true)
	}
	r.tt.Frame()
	r.tt.Frame()
	f, _ := os.Create(out)
	defer f.Close()
	png.Encode(f, r.tt.Image())
}
