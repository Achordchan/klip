package updatewin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"klip/internal/update"
)

type host struct{ quit int }

func (h *host) Quit() { h.quit++ }

type rig struct {
	t       *testing.T
	m       *Model
	tt      *ui.Tester
	h       *host
	co      *update.Coordinator
	srv     *httptest.Server
	shut    int
	helpers [][2]string
	wg      sync.WaitGroup

	mu      sync.Mutex
	release update.Release
	gate    chan struct{} // when set, the installer download waits here after half
}

func newRig(t *testing.T, current string, rel update.Release) *rig {
	r := &rig{t: t, h: &host{}, release: rel}
	setup := []byte("SETUP-CONTENT-0123456789")
	sum := sha256.Sum256(setup)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/manifest.json":
			r.mu.Lock()
			rel := r.release
			r.mu.Unlock()
			rel.Installer = update.Installer{URL: r.srv.URL + "/setup.exe", SHA256: strings.ToUpper(hex.EncodeToString(sum[:])), Size: int64(len(setup))}
			b, _ := json.Marshal(update.Manifest{AppID: "maccy", Latest: rel})
			w.Write(b)
		case "/setup.exe":
			w.Header().Set("Content-Length", "24")
			w.Write(setup[:12])
			w.(http.Flusher).Flush()
			if r.gate != nil {
				<-r.gate
			}
			w.Write(setup[12:])
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(r.srv.Close)
	exe := filepath.Join(t.TempDir(), "maccy.exe")
	_ = os.WriteFile(exe, []byte("EXE"), 0o755)
	cl := &update.Client{HTTP: r.srv.Client(), ManifestURL: r.srv.URL + "/manifest.json", Current: current, Runtime: "win-x64",
		ManifestPause: func(int) time.Duration { return time.Millisecond }, DownloadPause: func(int) time.Duration { return time.Millisecond }}
	r.co = update.NewCoordinator(cl, filepath.Join(t.TempDir(), "updates"), exe, func() { r.shut++ })
	r.co.Supported = true
	r.co.StartHelper = func(h, p string) error { r.helpers = append(r.helpers, [2]string{h, p}); return nil }
	r.m = New(r.co, r.h)
	r.m.Go = func(f func()) { r.wg.Add(1); go func() { defer r.wg.Done(); f() }() }
	r.tt = ui.NewTester(r.m.View, 460, 380)
	return r
}

func (r *rig) wait() { r.wg.Wait(); r.tt.Frame() }

func (r *rig) click(s string) {
	r.t.Helper()
	if err := r.tt.Click(s); err != nil {
		r.t.Fatalf("click %q: %v (%q)", s, err, r.tt.Texts())
	}
}

func TestManualCheckUpToDate(t *testing.T) {
	r := newRig(t, "1.0.11", update.Release{Version: "1.0.11"})
	r.m.CheckNow()
	r.wait()
	if r.m.Kind() != Message || !r.tt.HasText("检查更新") || !r.tt.HasText("当前已经是最新版本。") {
		t.Fatalf("%v %q", r.m.Kind(), r.tt.Texts())
	}
	r.click("确定")
	if r.m.Kind() != None || r.m.Active() || r.co.State().Phase != update.Idle {
		t.Fatalf("%v active=%v %+v", r.m.Kind(), r.m.Active(), r.co.State())
	}
}

func TestCheckWindowText(t *testing.T) {
	r := newRig(t, "1.0.10", update.Release{Version: "1.0.11"})
	cu := ui.NewTester(r.m.ViewCheck, Sizes[Check][0], Sizes[Check][1])
	for _, s := range []string{"检查更新中", "正在连接更新服务器，请稍候..."} {
		if !cu.HasText(s) {
			t.Fatalf("missing %q: %q", s, cu.Texts())
		}
	}
}

func TestSilentFindIsNoWindow(t *testing.T) {
	r := newRig(t, "1.0.10", update.Release{Version: "1.0.11"})
	r.co.Refresh(context.Background())
	if r.co.Available() == nil || r.m.Kind() != None {
		t.Fatalf("a version found by the silent check opens a window: %v", r.m.Kind())
	}
}

func TestAvailableShowsNotesAndLater(t *testing.T) {
	r := newRig(t, "1.0.10", update.Release{Version: "1.0.11", Notes: "- 全新的内核\n- 更小更快"})
	r.m.CheckNow()
	r.wait()
	if r.m.Kind() != Prompt || r.m.Title(Prompt) != "发现新版本" {
		t.Fatalf("%v %q", r.m.Kind(), r.m.Title(Prompt))
	}
	for _, s := range []string{"发现新版本", "版本：1.0.11", "更新内容：", "稍后", "取消", "立即更新"} {
		if !r.tt.HasText(s) {
			t.Fatalf("missing %q: %q", s, r.tt.Texts())
		}
	}
	if !strings.Contains(strings.Join(r.tt.Texts(), "|"), "全新的内核") {
		t.Fatalf("notes not shown: %q", r.tt.Texts())
	}
	r.click("稍后")
	if r.m.Kind() != None || r.co.Available() == nil {
		t.Fatalf("later must keep the version known: %v %+v", r.m.Kind(), r.co.State())
	}
}

func TestNoticeWithoutNotes(t *testing.T) {
	r := newRig(t, "1.0.10", update.Release{Version: "1.0.11"})
	r.m.CheckNow()
	r.wait()
	if !r.tt.HasText("（无）") {
		t.Fatalf("%q", r.tt.Texts())
	}
	r.click("取消") // a normal update: cancel is the same as later
	if r.m.Kind() != None || r.h.quit != 0 {
		t.Fatalf("%v quit %d", r.m.Kind(), r.h.quit)
	}
}

func TestMandatoryUpdateCanBeDeclinedOnlyByQuitting(t *testing.T) {
	r := newRig(t, "1.0.10", update.Release{Version: "1.0.11", Mandatory: true})
	r.m.CheckNow()
	r.wait()
	if r.m.Title(Prompt) != "需要更新" || r.tt.HasText("稍后") || !r.tt.HasText("退出") {
		t.Fatalf("%q", r.tt.Texts())
	}
	r.click("退出")
	if r.h.quit != 1 {
		t.Fatal("quit")
	}
	// the close button of the window is the same
	r.h.quit = 0
	r.m.WindowClosed(Prompt)
	if r.h.quit != 1 {
		t.Fatal("closing the window of a mandatory update must quit")
	}
}

func TestButtonStartsTheDownloadAtOnce(t *testing.T) {
	r := newRig(t, "1.0.10", update.Release{Version: "1.0.11"})
	r.co.Refresh(context.Background())
	r.gate = make(chan struct{})
	r.m.ApplyNow()
	deadline := time.Now().Add(3 * time.Second)
	for r.co.State().Read < 12 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	r.tt.Frame()
	if r.m.Kind() != Progress || r.m.Title(Progress) != "正在下载更新" || !r.tt.HasText("正在下载...") || !r.tt.HasText("12 B / 24 B") {
		t.Fatalf("%v: %q", r.m.Kind(), r.tt.Texts())
	}
	close(r.gate)
	r.wait()
	if r.shut != 1 || len(r.helpers) != 1 || !r.tt.HasText("正在重启以完成更新...") {
		t.Fatalf("shut %d helpers %v: %q", r.shut, r.helpers, r.tt.Texts())
	}
}

func TestFailureIsShownAndCanBeRetried(t *testing.T) {
	r := newRig(t, "1.0.10", update.Release{Version: "1.0.11"})
	r.m.CheckNow()
	r.wait()
	st := r.co.State()
	st.Info.InstallerSize = 3 // a manifest that promises another size than the file has
	r.click("立即更新")
	r.wait()
	if r.shut != 0 || r.m.Kind() != Message || r.m.Title(Message) != "下载失败" || !r.tt.HasText("下载失败") ||
		!strings.Contains(strings.Join(r.tt.Texts(), "|"), "download exceeds expected size") || !r.tt.HasText("重试") {
		t.Fatalf("shut %d %v: %q", r.shut, r.m.Kind(), r.tt.Texts())
	}
	st.Info.InstallerSize = 24
	r.click("重试")
	r.wait()
	if r.shut != 1 || !r.tt.HasText("正在重启以完成更新...") {
		t.Fatalf("shut %d: %q", r.shut, r.tt.Texts())
	}
}

func TestCheckFailureShowsTheMessage(t *testing.T) {
	r := newRig(t, "1.0.10", update.Release{Version: "1.0.11"})
	r.co.Client.ManifestURL = r.srv.URL + "/nope.json"
	r.m.CheckNow()
	r.wait()
	if r.m.Title(Message) != "检查更新失败" || !r.tt.HasText("检查更新失败") || r.tt.HasText("重试") {
		t.Fatalf("%q", r.tt.Texts())
	}
	r.click("确定")
	if r.m.Active() || r.co.State().Phase != update.Idle {
		t.Fatalf("%v %+v", r.m.Active(), r.co.State())
	}
}

func TestSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.00 KB", 1536: "1.50 KB", 5 << 20: "5.00 MB", 3 << 30: "3.00 GB"} {
		if got := size(n); got != want {
			t.Errorf("size(%d) = %q, want %q", n, got, want)
		}
	}
}

// At the size of its window the prompt keeps its buttons inside it and under
// the notes, whatever the notes are.
func TestPromptFitsItsWindow(t *testing.T) {
	for _, notes := range []string{"", "- 全新的内核", strings.Repeat("- 一行更新说明\n", 60)} {
		r := newRig(t, "1.0.10", update.Release{Version: "1.0.11", Notes: notes})
		r.m.CheckNow()
		r.wait()
		w, h := Sizes[Prompt][0], Sizes[Prompt][1]
		tt := ui.NewTester(r.m.ViewPrompt, w, h)
		tt.Frame()
		label, ok1 := tt.Find("更新内容：")
		// A button the window cuts off shows only part of its height.
		whole := ui.NewTester(r.m.ViewPrompt, w, h+400)
		whole.Frame()
		for _, b := range []string{"稍后", "取消", "立即更新"} {
			r, ok := tt.Find(b)
			full, _ := whole.Find(b)
			if !ok || !ok1 {
				t.Fatalf("missing %q: %q", b, tt.Texts())
			}
			if r.H < full.H || r.Y+r.H > float32(h) || r.X+r.W > float32(w) || r.Y < label.Y+label.H {
				t.Fatalf("notes %d bytes: %q at %+v does not fit a %dx%d window under the notes (%+v)", len(notes), b, r, w, h, label)
			}
		}
	}
}
