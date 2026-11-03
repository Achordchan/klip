package prefswin

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"klip/internal/account"
	"klip/internal/autostart"
	"klip/internal/model"
	"klip/internal/nastest"
	"klip/internal/prefs"
	"klip/internal/settings"
	"klip/internal/syncclient"
)

type fakeAuto struct{ on bool }

func (f *fakeAuto) IsEnabled() bool          { return f.on }
func (f *fakeAuto) SetEnabled(on bool) error { f.on = on; return nil }

var _ autostart.Service = (*fakeAuto)(nil)

type fakeHost struct {
	storage, sync, closed int
	updates               func()
	accountEvents         []bool
}

func (h *fakeHost) OpenStorage()           { h.storage++ }
func (h *fakeHost) CheckUpdates() func()   { return h.updates }
func (h *fakeHost) SyncNow()               { h.sync++ }
func (h *fakeHost) AccountChanged(in bool) { h.accountEvents = append(h.accountEvents, in) }
func (h *fakeHost) Close()                 { h.closed++ }

type rig struct {
	t    *testing.T
	m    *Model
	tt   *ui.Tester
	set  *settings.Manager
	auto *fakeAuto
	host *fakeHost

	mu    sync.Mutex
	posts []func()
}

// newRig builds a window over a server at base ("" for none).
func newRig(t *testing.T, base string) *rig {
	t.Helper()
	if base == "" {
		base = "http://127.0.0.1:1"
	}
	set, _ := settings.Open(filepath.Join(t.TempDir(), "settings.json"))
	r := &rig{t: t, set: set, auto: &fakeAuto{}, host: &fakeHost{}}
	acc := &account.Service{Client: syncclient.New(base), Store: set}
	r.m = New(&prefs.Controller{Settings: set, Autostart: r.auto}, acc, r.host)
	// Work finished in the background is queued, and applied by the test, as
	// the UI thread would between frames.
	r.m.Post = func(f func()) { r.mu.Lock(); r.posts = append(r.posts, f); r.mu.Unlock() }
	r.m.Open()
	r.tt = ui.NewTester(r.m.View, Width, Height)
	return r
}

// settle waits for the background work and applies what it left.
func (r *rig) settle() {
	r.t.Helper()
	for {
		r.m.Wait()
		r.mu.Lock()
		posts := r.posts
		r.posts = nil
		r.mu.Unlock()
		if len(posts) == 0 {
			break
		}
		for _, f := range posts {
			f()
		}
	}
	r.tt.Frame()
}

func (r *rig) click(s string) {
	r.t.Helper()
	if err := r.tt.Click(s); err != nil {
		r.t.Fatalf("click %q: %v (texts %q)", s, err, r.tt.Texts())
	}
}

func (r *rig) has(s string) bool { return r.tt.HasText(s) }

func (r *rig) typeInto(label, text string) {
	r.t.Helper()
	r.click(label)
	r.tt.Type(text)
}

func TestTabsAndDefaults(t *testing.T) {
	r := newRig(t, "")
	if !r.has("开机启动") || !r.has("主题") || !r.has("跟随系统") {
		t.Fatalf("general: %q", r.tt.Texts())
	}
	if !r.has("检查更新") || !r.has("检查") {
		t.Fatalf("the row of the update check is always there: %q", r.tt.Texts())
	}
	r.click("内容")
	for _, s := range []string{"合并重复内容", "收藏项不计入上限", "最大条目", "最大容量（MB）", "存储位置", "捕获文本", "文件后缀", "单文件上限（MB）"} {
		if !r.has(s) {
			t.Fatalf("content page lacks %q: %q", s, r.tt.Texts())
		}
	}
	if v := r.m.v; v.MaxItems != "200" || v.MaxMegabytes != "300" || v.CaptureFileMaxMegabytes != "20" {
		t.Fatalf("boxes: %+v", v)
	}
	r.click("货架")
	if !r.has("启用货架") || !r.has("Ctrl（推荐）") {
		t.Fatalf("shelf: %q", r.tt.Texts())
	}
	r.click("云同步")
	for _, s := range []string{"未登录", "登录", "注册", "忘记密码", "登录后即可启用云同步，剪贴板历史将自动同步到云端。"} {
		if !r.has(s) {
			t.Fatalf("cloud page lacks %q: %q", s, r.tt.Texts())
		}
	}
	// the sync and the plan are for a signed-in user only
	for _, s := range []string{"立即同步", "订阅状态", "兑换卡密"} {
		if r.has(s) {
			t.Fatalf("signed out, the cloud page shows %q", s)
		}
	}
	r.click("关闭")
	if r.host.closed != 1 {
		t.Fatal("close")
	}
}

func TestUpdateCheckButton(t *testing.T) {
	r := newRig(t, "")
	called := 0
	r.host.updates = func() { called++ }
	r.tt.Frame()
	r.click("检查")
	if called != 1 {
		t.Fatalf("called %d", called)
	}
}

func TestCheckboxesWriteSettingsAndToast(t *testing.T) {
	r := newRig(t, "")
	r.click("内容")
	r.click("捕获文本")
	if r.set.Get().CaptureText {
		t.Fatal("not turned off")
	}
	if !r.has("设置已生效") {
		t.Fatalf("no toast: %q", r.tt.Texts())
	}
	r.click("捕获文本")
	if !r.set.Get().CaptureText {
		t.Fatal("not turned on")
	}
	r.click("通用")
	r.click("开机启动")
	if !r.auto.on || !r.set.Get().StartWithWindows {
		t.Fatalf("autostart: system %v settings %v", r.auto.on, r.set.Get().StartWithWindows)
	}
}

func TestNumbersAreKeptInRangeAndToastAfterAPause(t *testing.T) {
	r := newRig(t, "")
	r.click("内容")
	// The single-file limit is put right as it is typed.
	r.typeInto("单文件上限 MB", "9") // appended to 20: 209 -> over 300? no, 209 <= 300
	if got := r.set.Get().CaptureFileMaxMegabytes; got != 209 {
		t.Fatalf("file limit %d", got)
	}
	r.typeInto("最大容量 MB", "0") // 3000: raises
	if got := r.set.Get().MaxMegabytes; got != 3000 {
		t.Fatalf("history size %d", got)
	}
	if r.has("设置已生效") {
		t.Fatal("a typed number toasts only after a pause")
	}
	r.tt.Advance(800 * 1e6)
	if !r.has("设置已生效") {
		t.Fatalf("no toast after the pause: %q", r.tt.Texts())
	}
	// Shrinking the history size brings the file limit down with it.
	r.click("最大容量 MB")
	r.tt.Key(ui.Cmd, ui.KeyA)
	r.tt.Type("100")
	if st := r.set.Get(); st.MaxMegabytes != 100 || st.CaptureFileMaxMegabytes != 100 {
		t.Fatalf("history %d file %d", st.MaxMegabytes, st.CaptureFileMaxMegabytes)
	}
	r.tt.Frame()
	if r.m.v.CaptureFileMaxMegabytes != "100" {
		t.Fatalf("the box of the file limit did not follow: %q", r.m.v.CaptureFileMaxMegabytes)
	}
	// A bad count is ignored.
	r.click("最大条目数")
	r.tt.Key(ui.Cmd, ui.KeyA)
	r.tt.Type("0")
	if r.set.Get().MaxItems != 200 {
		t.Fatalf("max items %d", r.set.Get().MaxItems)
	}
}

func TestOpenAgainReadsSettingsAndRepairs(t *testing.T) {
	r := newRig(t, "")
	_ = r.set.Update(func(s *model.Settings) { s.CaptureFileExtensions = "" })
	r.auto.on = true
	r.m.Open()
	r.tt.Frame()
	r.click("内容")
	if r.m.v.CaptureFileExtensions != prefs.DefaultCaptureFileExtensions {
		t.Fatalf("blank list not replaced: %q", r.m.v.CaptureFileExtensions)
	}
	if !r.set.Get().StartWithWindows {
		t.Fatal("autostart state of the system not taken")
	}
}

func TestStorageAndSyncButtons(t *testing.T) {
	r := newRig(t, "")
	r.click("内容")
	r.click("打开")
	// the sync buttons are for a signed-in user
	_ = r.set.Update(func(s *model.Settings) {
		s.AuthRefreshToken = model.Ptr("r")
		s.AuthUserEmail = model.Ptr("me@example.com")
	})
	r.m.Open()
	r.tt.Frame()
	r.click("云同步")
	r.click("立即同步")
	if r.host.storage != 1 || r.host.sync != 1 {
		t.Fatalf("%+v", r.host)
	}
}

func TestSignInRegisterAndOut(t *testing.T) {
	srv := nastest.Start(t)
	r := newRig(t, srv.URL)
	r.click("云同步")

	// Missing input needs no server.
	r.click("登录")
	r.settle()
	if !r.has("请输入邮箱和密码") {
		t.Fatalf("no toast: %q", r.tt.Texts())
	}

	r.typeInto("邮箱地址", "me@example.com")
	r.typeInto("密码输入", "password1")
	r.click("注册")
	r.settle()
	if !r.has("注册成功") || !r.has("已登录（me@example.com）") || !r.has("退出登录") {
		t.Fatalf("after register: %q", r.tt.Texts())
	}
	if len(r.host.accountEvents) != 1 || !r.host.accountEvents[0] {
		t.Fatalf("host not told: %v", r.host.accountEvents)
	}
	if !r.has("未订阅") {
		t.Fatalf("plan: %q", r.tt.Texts())
	}

	// Card.
	cards := srv.Admin("/admin/card/generate", map[string]any{"count": 1, "durationDays": 30})
	code := cards["codes"].([]any)[0].(string)
	r.typeInto("卡密输入", code)
	r.click("兑换")
	r.settle()
	if !r.has("兑换成功") || !r.has("正常") {
		t.Fatalf("after redeem: %q", r.tt.Texts())
	}
	if r.has("未订阅") {
		t.Fatal("plan not refreshed")
	}

	r.click("退出登录")
	r.settle()
	if !r.has("已退出登录") || !r.has("登录") || r.set.Get().AuthRefreshToken != nil {
		t.Fatalf("after sign out: %q", r.tt.Texts())
	}
	// register (in), redeem (in: the card may be what the first sync was
	// waiting for), sign out (out)
	if ev := r.host.accountEvents; len(ev) != 3 || !ev[0] || !ev[1] || ev[2] {
		t.Fatalf("host events %v", ev)
	}

	// Wrong password; Enter in the password box signs in.
	r.typeInto("密码输入", "wrong")
	r.click("登录")
	r.settle()
	if !r.has("邮箱或密码不正确") {
		t.Fatalf("wrong password: %q", r.tt.Texts())
	}
	r.click("密码输入")
	r.tt.Key(ui.Cmd, ui.KeyA)
	r.tt.Type("password1")
	r.tt.Key(0, ui.KeyEnter)
	r.settle()
	if !r.has("登录成功") {
		t.Fatalf("enter did not sign in: %q", r.tt.Texts())
	}
}

func TestPasswordDialogs(t *testing.T) {
	srv := nastest.Start(t)
	r := newRig(t, srv.URL)
	r.click("云同步")
	r.click("忘记密码")
	r.tt.Frame()
	for _, s := range []string{"找回密码", "发送验证码", "重置密码", "取消"} {
		if !r.has(s) {
			t.Fatalf("forgot dialog lacks %q: %q", s, r.tt.Texts())
		}
	}
	// Checks come first, before the server.
	r.click("重置密码")
	r.settle()
	if !r.has("请输入有效邮箱") {
		t.Fatalf("%q", r.tt.Texts())
	}
	r.typeInto("找回邮箱", "stranger@example.com")
	r.click("发送验证码")
	r.settle()
	if !r.has("验证码已发送，请检查邮箱") || !r.has("60s 后重发") {
		t.Fatalf("after sending: %q", r.tt.Texts())
	}
	r.typeInto("验证码输入", "000000")
	r.typeInto("新密码输入", "password9")
	r.typeInto("确认新密码输入", "password8")
	r.click("重置密码")
	r.settle()
	if !r.has("两次输入的新密码不一致") {
		t.Fatalf("%q", r.tt.Texts())
	}
	r.click("取消")
	r.tt.Frame()
	if r.has("找回密码") || !r.has("忘记密码") {
		t.Fatalf("dialog not closed: %q", r.tt.Texts())
	}

	// Signed in: the other dialog.
	r.typeInto("邮箱地址", "pw@example.com")
	r.typeInto("密码输入", "password1")
	r.click("注册")
	r.settle()
	r.click("修改密码")
	r.tt.Frame()
	if !r.has("修改密码") || !r.has("当前账号") || !r.has("pw@example.com") || !r.has("确认修改") {
		t.Fatalf("change dialog: %q", r.tt.Texts())
	}
	r.click("取消")
	r.tt.Frame()
	if r.has("当前账号") {
		t.Fatal("not closed")
	}
}

func TestSignedInOnOpenLoadsPlan(t *testing.T) {
	srv := nastest.Start(t)
	tok := srv.Subscribe(t.Context(), "paid@example.com", "password1")
	r := newRig(t, srv.URL)
	_ = r.set.Update(func(s *model.Settings) {
		s.AuthAccessToken, s.AuthRefreshToken = &tok.AccessToken, &tok.RefreshToken
		s.AuthExpiresAtUnixMs = tok.ExpiresAt.UnixMilli()
		s.AuthUserEmail = model.Ptr("paid@example.com")
	})
	r.m.Open()
	r.tt.Frame()
	r.click("云同步")
	if !r.has("正在读取中...") {
		t.Fatalf("no loading state: %q", r.tt.Texts())
	}
	r.settle()
	joined := strings.Join(r.tt.Texts(), "|")
	if !strings.Contains(joined, "订阅至 20") || !r.has("已登录（paid@example.com）") {
		t.Fatalf("plan not shown: %q", joined)
	}
}

func TestAccountPageFollowsALoginTakenAwayInTheBackground(t *testing.T) {
	r := newRig(t, "http://127.0.0.1:1")
	_ = r.set.Update(func(s *model.Settings) {
		s.AuthAccessToken, s.AuthRefreshToken = model.Ptr("a"), model.Ptr("r")
		s.AuthUserEmail = model.Ptr("me@example.com")
		s.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	})
	r.m.Open()
	r.settle()
	r.click("云同步")
	if !r.has("退出登录") {
		t.Fatalf("signed in: %q", r.tt.Texts())
	}
	_ = r.set.Update(func(s *model.Settings) { syncclient.ClearAuthSession(s, false) }) // a refresh the server refused
	r.settle()
	if r.has("退出登录") || !r.has("登录") {
		t.Fatalf("the page still shows the account: %q", r.tt.Texts())
	}
}

// While a password dialog shows, the keys stay in it: Tab goes round inside
// it, and the sign-in form behind it (whose Enter signs in) is out of reach;
// Escape closes it.
func TestPasswordDialogKeepsTheKeys(t *testing.T) {
	for tabs := range 16 {
		r := newRig(t, "http://127.0.0.1:1")
		r.click("云同步")
		r.click("忘记密码")
		r.tt.Frame()
		for range tabs {
			r.tt.Key(0, ui.KeyTab)
			r.tt.Frame()
		}
		r.tt.Key(0, ui.KeyEnter)
		r.settle()
		if r.has("请输入邮箱和密码") {
			t.Fatalf("after %d tabs Enter reached the sign-in form behind the dialog", tabs)
		}
	}
	r := newRig(t, "http://127.0.0.1:1")
	r.click("云同步")
	r.click("忘记密码")
	r.tt.Frame()
	r.tt.Key(0, ui.KeyEscape)
	r.tt.Frame()
	if r.has("找回密码") || !r.has("忘记密码") {
		t.Fatalf("Escape did not close the dialog: %q", r.tt.Texts())
	}
}
