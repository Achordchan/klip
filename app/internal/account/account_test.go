package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"klip/internal/model"
	"klip/internal/nastest"
	"klip/internal/settings"
	"klip/internal/syncclient"
)

func newService(t *testing.T, base string) (*Service, *settings.Manager) {
	t.Helper()
	m, _ := settings.Open(filepath.Join(t.TempDir(), "settings.json"))
	return &Service{Client: syncclient.New(base), Store: m}, m
}

func TestSignedIn(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	st := model.DefaultSettings()
	if SignedIn(st, now) {
		t.Fatal("defaults are signed out")
	}
	st.AuthAccessToken = model.Ptr("a")
	st.AuthExpiresAtUnixMs = now.UnixMilli() + 29_000
	if SignedIn(st, now) {
		t.Fatal("an access token with under 30 s left does not count")
	}
	st.AuthExpiresAtUnixMs = now.UnixMilli() + 31_000
	if !SignedIn(st, now) {
		t.Fatal("an access token with 31 s left counts")
	}
	st = model.DefaultSettings()
	st.AuthRefreshToken = model.Ptr(" r ")
	if !SignedIn(st, now) {
		t.Fatal("a refresh token counts")
	}
	st.AuthRefreshToken = model.Ptr("  ")
	if SignedIn(st, now) {
		t.Fatal("a blank refresh token does not")
	}
}

func TestInputChecksNeedNoServer(t *testing.T) {
	s, _ := newService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	for name, r := range map[string]Result{
		"signin":   s.SignIn(ctx, "", "x"),
		"register": s.Register(ctx, "a@b.c", "  "),
	} {
		if r.OK || r.Message != "请输入邮箱和密码" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if _, r := s.SendCode(ctx, Reset, "no-at-sign"); r.Message != "请输入有效邮箱" {
		t.Errorf("SendCode: %+v", r)
	}
	if _, r := s.SendCode(ctx, Change, ""); r.Message != "请输入有效邮箱" {
		t.Errorf("SendCode change without address: %+v", r)
	}
	cases := []struct{ email, code, pw, confirm, want string }{
		{"x", "123456", "password1", "password1", "请输入有效邮箱"},
		{"a@b.c", " ", "password1", "password1", "请输入验证码"},
		{"a@b.c", "123456", "short", "short", "新密码至少 8 位"},
		{"a@b.c", "123456", "password1", "password2", "两次输入的新密码不一致"},
	}
	for _, c := range cases {
		if r := s.ResetPassword(ctx, c.email, c.code, c.pw, c.confirm); r.OK || r.Message != c.want {
			t.Errorf("ResetPassword(%v): %+v", c, r)
		}
		if r := s.ChangePassword(ctx, c.email, c.code, c.pw, c.confirm); r.OK || r.Message != c.want {
			t.Errorf("ChangePassword(%v): %+v", c, r)
		}
	}
	if r := s.Redeem(ctx, "  "); r.Message != "请输入卡密" {
		t.Errorf("Redeem blank: %+v", r)
	}
	if r := s.Redeem(ctx, "abc"); r.Message != "请先登录" {
		t.Errorf("Redeem signed out: %+v", r)
	}
	if got := s.Subscription(ctx); got.Status != "未登录" || got.Tier != "-" {
		t.Errorf("Subscription signed out: %+v", got)
	}
}

func TestExplainAuth(t *testing.T) {
	for in, want := range map[string]string{
		"invalid email or password":              "邮箱或密码不正确",
		"Invalid Email":                          "请输入有效邮箱",
		"password must be at least 8 characters": "新密码至少 8 位",
		"invalid or expired code":                "验证码错误或已过期",
		"too many code attempts":                 "验证码错误次数过多，请重新获取",
		"email service not configured":           "邮件服务尚未配置，请联系管理员",
		"failed to send email":                   "验证码邮件发送失败，请稍后重试",
		"invalid request":                        "登录请求格式异常，请重启应用后重试",
		"response missing accessToken":           "登录响应异常，请稍后重试",
		"net/http: request canceled (timed out)": "登录请求超时，请检查网络后重试",
		"something odd":                          "登录失败：something odd",
	} {
		if got := ExplainAuth(errors.New(in), "登录失败"); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if got := ExplainAuth(context.Canceled, "登录失败"); got != "请求超时或已取消，请稍后重试" {
		t.Errorf("canceled: %q", got)
	}
	if got := ExplainAuth(errors.New(" "), "登录失败"); got != "登录失败" {
		t.Errorf("blank: %q", got)
	}
}

func TestFormatBytes(t *testing.T) {
	for in, want := range map[int64]string{-5: "0 B", 0: "0 B", 1023: "1023 B", 1024: "1 KB", 1536: "1.5 KB",
		1 << 20: "1 MB", 1234567: "1.18 MB", 5 << 30: "5 GB", 3 << 40: "3 TB", 5000 << 40: "5000 TB"} {
		if got := formatBytes(in); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", in, got, want)
		}
	}
	a, b := int64(1<<20), int64(0)
	if got := formatStorage(&a, &b); got != "1 MB / -" {
		t.Errorf("no limit: %q", got)
	}
	if got := formatStorage(nil, nil); got != "- / -" {
		t.Errorf("nothing: %q", got)
	}
}

// The whole account life against the real server.
func TestAccountAgainstServer(t *testing.T) {
	srv := nastest.Start(t)
	ctx := context.Background()
	s, m := newService(t, srv.URL)

	if r := s.SignIn(ctx, "nobody@example.com", "password1"); r.OK || r.Message != "邮箱或密码不正确" {
		t.Fatalf("unknown account: %+v", r)
	}
	r := s.Register(ctx, " me@example.com ", "password1")
	if !r.OK || r.Message != "注册成功" {
		t.Fatalf("register: %+v", r)
	}
	if !s.SignedIn() || s.Email() != "me@example.com" || s.StatusText() != "已登录（me@example.com）" {
		t.Fatalf("after register: %v %q %q", s.SignedIn(), s.Email(), s.StatusText())
	}
	if got := m.Get().NasAgentBaseUrl; got != model.OfficialSyncBaseUrl {
		t.Fatalf("server address after sign-in = %q", got)
	}
	if r := s.Register(ctx, "me@example.com", "password1"); r.OK {
		t.Fatalf("a second registration worked: %+v", r)
	} else {
		t.Logf("second registration says: %q", r.Message)
	}

	// Free account: no plan.
	if got := s.Subscription(ctx); got.Status != "未订阅" {
		t.Fatalf("free account: %+v", got)
	}
	// The server answers {"error":"card not found or used"}; like C#, which
	// hides a JSON body behind a plain text, the window says only this.
	if r := s.Redeem(ctx, "0000-0000-0000-0000"); r.OK || r.Message != "兑换失败" {
		t.Fatalf("unknown card: %+v", r)
	}
	cards := srv.Admin("/admin/card/generate", map[string]any{"count": 1, "durationDays": 30})
	code := cards["codes"].([]any)[0].(string)
	if r := s.Redeem(ctx, " "+code+" "); !r.OK || r.Message != "兑换成功" {
		t.Fatalf("redeem: %+v", r)
	}
	if r := s.Redeem(ctx, code); r.OK {
		t.Fatalf("a card works once: %+v", r)
	}
	sub := s.Subscription(ctx)
	if !strings.HasPrefix(sub.Status, "订阅至 20") || sub.Tier == "-" || sub.Tier == "" || sub.Limit != "正常" || sub.OverLimit {
		t.Fatalf("subscription after redeem: %+v", sub)
	}
	t.Logf("subscription: %+v", sub)

	// Sign out, and in again.
	if r := s.SignOut(); !r.OK || r.Message != "已退出登录" || s.SignedIn() || s.Email() != "" {
		t.Fatalf("sign out: %+v", r)
	}
	if got := m.Get(); got.AuthUserEmail != nil || got.AuthRefreshToken != nil {
		t.Fatalf("session left behind: %+v", got)
	}
	if r := s.SignIn(ctx, "me@example.com", "wrong-password"); r.OK || r.Message != "邮箱或密码不正确" {
		t.Fatalf("wrong password: %+v", r)
	}
	if r := s.SignIn(ctx, "ME@example.com", "password1"); !r.OK || r.Message != "登录成功" {
		t.Fatalf("sign in: %+v", r)
	}
}

func TestEnsureAccessToken(t *testing.T) {
	srv := nastest.Start(t)
	ctx := context.Background()
	s, m := newService(t, srv.URL)
	if tok, err := s.EnsureAccessToken(ctx); tok != "" || err != nil {
		t.Fatalf("signed out: %q %v", tok, err)
	}
	if r := s.Register(ctx, "tok@example.com", "password1"); !r.OK {
		t.Fatal(r)
	}
	good := model.Str(m.Get().AuthAccessToken)
	if tok, err := s.EnsureAccessToken(ctx); err != nil || tok != good {
		t.Fatalf("a good token comes back as it is: %q %v", tok, err)
	}

	// Expired access token: the refresh token renews it.
	_ = m.Update(func(st *model.Settings) { st.AuthExpiresAtUnixMs = time.Now().UnixMilli() - 1000 })
	tok, err := s.EnsureAccessToken(ctx)
	if err != nil || tok == "" {
		t.Fatalf("refresh: %q %v", tok, err)
	}
	if m.Get().AuthExpiresAtUnixMs <= time.Now().UnixMilli() {
		t.Fatal("expiry not renewed")
	}

	// A token that looks valid but the server refuses: signed out, the
	// address kept.
	_ = m.Update(func(st *model.Settings) { st.AuthAccessToken = model.Ptr("not-a-token") })
	if tok, err := s.EnsureAccessToken(ctx); tok != "" || err != nil {
		t.Fatalf("refused token: %q %v", tok, err)
	}
	if got := m.Get(); got.AuthRefreshToken != nil || model.Str(got.AuthUserEmail) != "tok@example.com" {
		t.Fatalf("after a refused token: %+v", got)
	}
}

func TestPasswordCodeFlow(t *testing.T) {
	srv := nastest.Start(t)
	ctx := context.Background()
	s, _ := newService(t, srv.URL)
	if r := s.Register(ctx, "someone@example.com", "password1"); !r.OK {
		t.Fatal(r)
	}
	s.SignOut()
	// The test server has no mail service: for a known address it says so;
	// for an unknown one it answers as if it had sent (it does not tell which
	// addresses have accounts).
	if cd, r := s.SendCode(ctx, Reset, "someone@example.com"); r.OK || r.Message != "邮件服务尚未配置，请联系管理员" {
		t.Fatalf("SendCode for a known address: cooldown %d, %+v", cd, r)
	}
	cd, r := s.SendCode(ctx, Reset, "stranger@example.com")
	if !r.OK || r.Message != "验证码已发送，请检查邮箱" || cd <= 0 {
		t.Fatalf("SendCode for an unknown address: cooldown %d, %+v", cd, r)
	}
	// A made-up code is refused, in words.
	if r := s.ResetPassword(ctx, "someone@example.com", "000000", "password2", "password2"); r.OK || r.Message != "验证码错误或已过期" {
		t.Fatalf("ResetPassword with a wrong code: %+v", r)
	}
	// The old password still works.
	if r := s.SignIn(ctx, "someone@example.com", "password1"); !r.OK {
		t.Fatalf("sign in after a failed reset: %+v", r)
	}
	// Changing needs the signed-in address and a good token.
	if cd, r := s.SendCode(ctx, Change, "ignored@example.com"); r.OK || r.Message != "邮件服务尚未配置，请联系管理员" {
		t.Fatalf("SendCode change: %d %+v", cd, r)
	}
	if r := s.ChangePassword(ctx, "", "000000", "password3", "password3"); r.OK || r.Message != "验证码错误或已过期" {
		t.Fatalf("ChangePassword with a wrong code: %+v", r)
	}
	// A signed-out change is refused before the server.
	s.SignOut()
	if _, r := s.SendCode(ctx, Change, ""); r.OK || r.Message != "请输入有效邮箱" {
		t.Fatalf("SendCode change signed out: %+v", r)
	}
}

func TestNetworkFailureKeepsTheLogin(t *testing.T) {
	s, m := newService(t, "http://127.0.0.1:1") // nobody answers there
	_ = m.Update(func(st *model.Settings) {
		st.AuthAccessToken, st.AuthRefreshToken = model.Ptr("old-access"), model.Ptr("the-refresh")
		st.AuthExpiresAtUnixMs = 1 // expired: a refresh is needed
	})
	if _, err := s.EnsureAccessToken(context.Background()); err == nil {
		t.Fatal("no answer must be an error, not a sign-out")
	}
	if model.Str(m.Get().AuthRefreshToken) != "the-refresh" {
		t.Fatal("the login was erased by a network failure")
	}
}

func TestServerTroubleKeepsTheLogin(t *testing.T) {
	for _, status := range []int{500, 503, 429} {
		hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		s, m := newService(t, hs.URL)
		_ = m.Update(func(st *model.Settings) {
			st.AuthAccessToken, st.AuthRefreshToken = model.Ptr("access"), model.Ptr("refresh")
			st.AuthExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli() // still valid: /auth/me is asked
		})
		if _, err := s.EnsureAccessToken(context.Background()); err == nil {
			t.Errorf("%d: trouble on the server must be an error", status)
		}
		if model.Str(m.Get().AuthRefreshToken) != "refresh" {
			t.Errorf("%d: the login was erased by a server in trouble", status)
		}
		hs.Close()
	}
}

func TestSignOutThatCannotBeSavedSaysSo(t *testing.T) {
	dir := t.TempDir()
	m, _ := settings.Open(filepath.Join(dir, "settings.json"))
	s := &Service{Client: syncclient.New("http://127.0.0.1:1"), Store: m}
	_ = m.Update(func(st *model.Settings) { st.AuthRefreshToken = model.Ptr("r") })
	s.Store = &memorySaveFailure{state: m.Get()}

	if r := s.SignOut(); r.OK {
		t.Fatalf("a sign-out that was not saved reported success: %+v", r)
	}
}

func TestSignInThatCannotBeSavedSaysSo(t *testing.T) {
	srv := nastest.Start(t)
	dir := t.TempDir()
	m, _ := settings.Open(filepath.Join(dir, "settings.json"))
	s := &Service{Client: syncclient.New(srv.URL), Store: m}
	s.Store = &memorySaveFailure{state: m.Get()}

	r := s.Register(context.Background(), "save@example.com", "password1")
	if !r.OK || !strings.Contains(r.Message, "无法保存") {
		t.Fatalf("the user must be told the login was not saved: %+v", r)
	}
}

func TestPasswordCleanupLeavesANewerLoginAlone(t *testing.T) {
	s, m := newService(t, "http://127.0.0.1:1")
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a1", RefreshToken: "r1", Email: "one@example.com"}, "one@example.com")
	epoch := s.epoch.Load() // the password request starts under this login
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a2", RefreshToken: "r2", Email: "two@example.com"}, "two@example.com")
	s.afterPasswordUpdated("one@example.com", epoch) // and its answer comes after the other login
	if model.Str(m.Get().AuthRefreshToken) != "r2" || model.Str(m.Get().AuthUserEmail) != "two@example.com" {
		t.Fatalf("the newer login was cleared: %+v", m.Get())
	}
	// The same login, tokens renewed meanwhile: it is cleared.
	epoch = s.epoch.Load()
	_ = m.Update(func(st *model.Settings) { st.AuthRefreshToken = model.Ptr("r2-renewed") })
	s.afterPasswordUpdated("two@example.com", epoch)
	if model.Str(m.Get().AuthRefreshToken) != "" {
		t.Fatal("the session of the password that changed must end, renewed tokens or not")
	}
}

func TestAnotherAccountStartsWithoutTheLastSyncState(t *testing.T) {
	s, m := newService(t, "http://127.0.0.1:1")
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a1", RefreshToken: "r1", Email: "one@example.com"}, "one@example.com")
	_ = m.Update(func(st *model.Settings) {
		st.SyncLastLocalFingerprint, st.SyncLastRemoteVersion = model.Ptr("fp"), model.Ptr("v1")
	})
	// The same account signing in again keeps what it knows.
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a1b", RefreshToken: "r1b", Email: "ONE@example.com"}, "one@example.com")
	if model.Str(m.Get().SyncLastRemoteVersion) != "v1" {
		t.Fatal("the same account lost its sync state")
	}
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a2", RefreshToken: "r2", Email: "two@example.com"}, "two@example.com")
	if m.Get().SyncLastLocalFingerprint != nil || m.Get().SyncLastRemoteVersion != nil {
		t.Fatal("another account must not inherit the sync state of the first")
	}
}

func TestSignInAnsweredAfterALoginChangeIsRefused(t *testing.T) {
	s, m := newService(t, "http://127.0.0.1:1")
	epoch := s.epoch.Load() // the sign-in begins
	s.SignOut()             // a sign-out (or a password reset) happens while it is out
	err := s.storeTokensAt(syncclient.TokenSet{AccessToken: "late", RefreshToken: "late-r", Email: "x@example.com"}, "x@example.com", epoch)
	if !errors.Is(err, errSuperseded) || model.Str(m.Get().AuthRefreshToken) != "" {
		t.Fatalf("a late answer was stored: %v %+v", err, m.Get())
	}
}

func TestSameAccountAfterASignOutKeepsItsSyncState(t *testing.T) {
	s, m := newService(t, "http://127.0.0.1:1")
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a1", RefreshToken: "r1", Email: "one@example.com"}, "one@example.com")
	_ = m.Update(func(st *model.Settings) { st.SyncLastRemoteVersion = model.Ptr("v1") })
	s.SignOut() // the address is forgotten, the state of the sync is not
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a1b", RefreshToken: "r1b", Email: "One@Example.com"}, "one@example.com")
	if model.Str(m.Get().SyncLastRemoteVersion) != "v1" {
		t.Fatal("signing back in to the same account lost the sync state")
	}
}

func TestMigratedLoginSignedOutThenAnotherAccountDoesNotInheritTheSyncState(t *testing.T) {
	s, m := newService(t, "http://127.0.0.1:1")
	// What 1.0.10 left: a login and its checkpoints, no record of the account.
	_ = m.Update(func(st *model.Settings) {
		st.AuthUserEmail, st.AuthRefreshToken = model.Ptr("old@example.com"), model.Ptr("r")
		st.SyncLastLocalFingerprint, st.SyncLastRemoteVersion = model.Ptr("fp"), model.Ptr("v9")
	})
	s.SignOut()
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a", RefreshToken: "r2", Email: "new@example.com"}, "new@example.com")
	if m.Get().SyncLastRemoteVersion != nil || m.Get().SyncLastLocalFingerprint != nil {
		t.Fatal("the new account inherited the checkpoints of the old one")
	}
}

func TestSyncAccountFileThatCannotBeWrittenDoesNotKeepTheOldName(t *testing.T) {
	dir := t.TempDir()
	s, _ := newService(t, "http://127.0.0.1:1")
	s.AccountFile = filepath.Join(dir, "sync", "account.txt")
	s.setSyncAccount("a@example.com")
	if s.syncAccount() != "a@example.com" {
		t.Fatal("not remembered")
	}
	_ = os.Chmod(s.AccountFile, 0o400) // the file can no longer be written
	if f, err := os.OpenFile(s.AccountFile, os.O_WRONLY, 0); err == nil {
		f.Close()
		t.Skip("writable anyway (root?)")
	}
	s.setSyncAccount("b@example.com")
	s2 := &Service{AccountFile: s.AccountFile} // a later run
	if got := s2.syncAccount(); got == "a@example.com" {
		t.Fatalf("a later run would read %q as the account of the checkpoints", got)
	}
}

func TestSidecarAndSettingsAgreeAfterAFailedSave(t *testing.T) {
	dir := t.TempDir()
	m, _ := settings.Open(filepath.Join(dir, "settings.json"))
	s := &Service{Client: syncclient.New("http://127.0.0.1:1"), Store: m, AccountFile: filepath.Join(dir, "sync", "account.txt")}
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a", RefreshToken: "ra", Email: "a@example.com"}, "a@example.com")
	s.Store = &memorySaveFailure{state: m.Get()}

	if err := s.storeTokens(syncclient.TokenSet{AccessToken: "b", RefreshToken: "rb", Email: "b@example.com"}, "b@example.com"); err == nil {
		t.Fatal("the save should have failed")
	}
	// A later run: the disk has account A's login and checkpoints, and the record
	// must still say A.
	later := &Service{AccountFile: s.AccountFile}
	if got := later.syncAccount(); got != "a@example.com" {
		t.Fatalf("the record says %q while the settings on disk are account A's", got)
	}
}

func TestOwnershipFollowsThePersistedLoginAfterARecovery(t *testing.T) {
	dir := t.TempDir()
	m, _ := settings.Open(filepath.Join(dir, "settings.json"))
	s := &Service{Client: syncclient.New("http://127.0.0.1:1"), Store: m, AccountFile: filepath.Join(dir, "sync", "account.txt")}
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a", RefreshToken: "ra", Email: "a@example.com"}, "a@example.com")
	_ = os.Chmod(dir, 0o500)
	if f, err := os.Create(filepath.Join(dir, "probe")); err == nil {
		f.Close()
		os.Chmod(dir, 0o755)
		t.Skip("writable anyway (root?)")
	}
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "b", RefreshToken: "rb", Email: "b@example.com"}, "b@example.com") // not saved
	_ = os.Chmod(dir, 0o755)
	_ = m.Update(func(st *model.Settings) { st.SyncLastRemoteVersion = model.Ptr("b-state") }) // saved now, with B's login
	s.SignOut()
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a2", RefreshToken: "ra2", Email: "a@example.com"}, "a@example.com")
	if m.Get().SyncLastRemoteVersion != nil {
		t.Fatal("A inherited the checkpoints B made")
	}
}

func TestPasswordResetOfAnotherAccountDoesNotAdoptTheCheckpoints(t *testing.T) {
	s, m := newService(t, "http://127.0.0.1:1")
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a", RefreshToken: "ra", Email: "a@example.com"}, "a@example.com")
	_ = m.Update(func(st *model.Settings) { st.SyncLastRemoteVersion = model.Ptr("a-state") })
	s.SignOut()
	s.afterPasswordUpdated("b@example.com", s.epoch.Load()) // B's password is reset while nobody is signed in
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "b", RefreshToken: "rb", Email: "b@example.com"}, "b@example.com")
	if m.Get().SyncLastRemoteVersion != nil {
		t.Fatal("B started with the checkpoints of A")
	}
}

func TestCheckpointsWithoutKnownOwnerAreNotAdopted(t *testing.T) {
	s, m := newService(t, "http://127.0.0.1:1")
	// What an older client leaves after a sign-out: checkpoints, no address, no sidecar.
	_ = m.Update(func(st *model.Settings) { st.SyncLastRemoteVersion = model.Ptr("old-state") })
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "b", RefreshToken: "rb", Email: "b@example.com"}, "b@example.com")
	if m.Get().SyncLastRemoteVersion != nil {
		t.Fatal("a login adopted checkpoints whose owner is unknown")
	}
}

func TestPasswordUpdatedThatCannotBeSavedSaysSo(t *testing.T) {
	dir := t.TempDir()
	m, _ := settings.Open(filepath.Join(dir, "settings.json"))
	s := &Service{Client: syncclient.New("http://127.0.0.1:1"), Store: m}
	_ = m.Update(func(st *model.Settings) { st.AuthRefreshToken = model.Ptr("r") })
	s.Store = &memorySaveFailure{state: m.Get()}

	r := s.passwordUpdated("a@example.com", s.epoch.Load())
	if !r.OK || !strings.Contains(r.Message, "无法保存设置") {
		t.Fatalf("the password changed, but the unsaved session must be reported: %+v", r)
	}
}

func TestSignOutThatCannotBeSavedDoesNotRelabelTheCheckpoints(t *testing.T) {
	dir := t.TempDir()
	m, _ := settings.Open(filepath.Join(dir, "settings.json"))
	s := &Service{Client: syncclient.New("http://127.0.0.1:1"), Store: m, AccountFile: filepath.Join(dir, "sync", "account.txt")}
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "a", RefreshToken: "ra", Email: "a@example.com"}, "a@example.com")
	_ = m.Update(func(st *model.Settings) { st.SyncLastRemoteVersion = model.Ptr("a-state") })
	s.SignOut() // the disk: no address, A's checkpoints, a record saying A
	s.Store = &memorySaveFailure{state: m.Get()}

	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "b", RefreshToken: "rb", Email: "b@example.com"}, "b@example.com") // not saved
	if r := s.SignOut(); r.OK {
		t.Fatal("the sign-out was not saved")
	}
	s2 := &Service{AccountFile: s.AccountFile} // a later run
	if got := s2.syncAccount(); got != "a@example.com" {
		t.Fatalf("the record says %q, but the checkpoints on disk are A's", got)
	}
}

func TestSignOutWithARecordThatCannotBeChangedLeavesNoCheckpoints(t *testing.T) {
	dir := t.TempDir()
	m, _ := settings.Open(filepath.Join(dir, "settings.json"))
	s := &Service{Client: syncclient.New("http://127.0.0.1:1"), Store: m, AccountFile: filepath.Join(dir, "sync", "account.txt")}
	s.setSyncAccount("a@example.com") // the record says A
	_ = s.storeTokens(syncclient.TokenSet{AccessToken: "b", RefreshToken: "rb", Email: "b@example.com"}, "b@example.com")
	_ = m.Update(func(st *model.Settings) { st.SyncLastRemoteVersion = model.Ptr("b-state") })
	// The record can be neither rewritten nor removed.
	syncDir := filepath.Dir(s.AccountFile)
	_ = os.Chmod(s.AccountFile, 0o400)
	_ = os.Chmod(syncDir, 0o500)
	defer os.Chmod(syncDir, 0o755)
	if err := os.WriteFile(s.AccountFile, []byte("x"), 0o600); err == nil {
		t.Skip("writable anyway (root?)")
	}
	if r := s.SignOut(); !r.OK {
		t.Fatalf("%+v", r)
	}
	if m.Get().SyncLastRemoteVersion != nil {
		t.Fatal("B's checkpoints survived the sign-out next to a record that says A")
	}
}

// memorySaveFailure models a failed disk write while retaining memory changes.
type memorySaveFailure struct{ state model.Settings }

func (s *memorySaveFailure) Get() model.Settings { return s.state }
func (s *memorySaveFailure) Update(f func(*model.Settings)) error {
	f(&s.state)
	return errors.New("simulated persistence failure")
}
