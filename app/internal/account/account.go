// Package account is what the preferences window does with the server's
// account: sign in and out, register, the password code flows, the
// subscription and the card redemption. It is the account half of
// PreferencesWindowViewModel without the window: every call returns the text
// the C# client shows in its toast, so a window only has to show it.
package account

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"klip/internal/model"
	"klip/internal/syncclient"
)

// Store is the settings, as the account needs them.
type Store interface {
	Get() model.Settings
	Update(func(*model.Settings)) error
}

// Service talks to the server for the signed-in user.
type Service struct {
	// epoch moves whenever a sign-in begins or ends. It names a login across the
	// renewals of its tokens, which a refresh token does not.
	epoch atomic.Int64
	// sessMu makes a change of login (sign-in, sign-out, the clearing that follows
	// a password change) and the check of epoch that goes before it one step.
	sessMu sync.Mutex
	// BeforeLoginChange is called, with sessMu held, just before the stored login
	// is replaced or ended; work that belongs to the old login should be stopped
	// there, not after (may be nil).
	// AccountFile is where the account that the sync state is about is written
	// down (a file of its own: the settings are exchanged with the C# client and
	// other devices as they are, and take no new fields). Empty keeps it in memory.
	AccountFile       string
	lastAccount       string
	BeforeLoginChange func()
	// AfterLoginChange is called, with sessMu held, once the stored login has
	// been replaced or ended (may be nil).
	AfterLoginChange func()

	Client *syncclient.Client
	Store  Store
	// Now is time.Now unless a test sets it.
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Timeouts of the C# flows.
const (
	signInTimeout   = 3 * time.Minute
	passwordTimeout = 2 * time.Minute
)

// SignedIn is HasSignedInSession: an access token with more than 30 seconds
// left, or a refresh token.
func SignedIn(st model.Settings, now time.Time) bool {
	if strings.TrimSpace(model.Str(st.AuthAccessToken)) != "" && st.AuthExpiresAtUnixMs > now.UnixMilli()+30_000 {
		return true
	}
	return strings.TrimSpace(model.Str(st.AuthRefreshToken)) != ""
}

// SignedIn says whether the user is signed in now.
func (s *Service) SignedIn() bool { return SignedIn(s.Store.Get(), s.now()) }

// Email is the address shown as signed in, "" when not.
func (s *Service) Email() string {
	st := s.Store.Get()
	if !SignedIn(st, s.now()) {
		return ""
	}
	return strings.TrimSpace(model.Str(st.AuthUserEmail))
}

// StatusText is "未登录", "已登录" or "已登录（邮箱）".
func (s *Service) StatusText() string {
	if !s.SignedIn() {
		return "未登录"
	}
	if e := s.Email(); e != "" {
		return "已登录（" + e + "）"
	}
	return "已登录"
}

func (s *Service) afterChange() {
	if s.AfterLoginChange != nil {
		s.AfterLoginChange()
	}
}

func (s *Service) syncAccount() string {
	if s.lastAccount != "" {
		return s.lastAccount // set in this run: newer than anything on disk
	}
	if s.AccountFile != "" {
		if b, err := os.ReadFile(s.AccountFile); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return s.lastAccount
}

// setSyncAccount records whose the checkpoints are. It says whether the disk
// now tells the truth: a record that could be neither updated nor removed is
// still the old one.
func (s *Service) setSyncAccount(email string) bool {
	s.lastAccount = email
	if s.AccountFile == "" {
		return true
	}
	_ = os.MkdirAll(filepath.Dir(s.AccountFile), 0o700)
	if err := os.WriteFile(s.AccountFile, []byte(email), 0o600); err != nil {
		// A file that could not be updated must not be left saying the old
		// account: that would outlive this run and name the wrong one.
		if rerr := os.Remove(s.AccountFile); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return false
		}
	}
	return true
}

func (s *Service) beforeChange() {
	if s.BeforeLoginChange != nil {
		s.BeforeLoginChange()
	}
}

// errSuperseded: something that ends or replaces the login (a sign-out, a
// password reset) happened while this sign-in was being answered.
var errSuperseded = errors.New("account: superseded")

func (s *Service) storeTokens(t syncclient.TokenSet, fallbackEmail string) error {
	return s.storeTokensAt(t, fallbackEmail, -1)
}

// storeTokensAt stores a login if no other change of login has happened since
// epoch (-1: whatever the epoch is).
func (s *Service) storeTokensAt(t syncclient.TokenSet, fallbackEmail string, epoch int64) error {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	if epoch >= 0 && s.epoch.Load() != epoch {
		return errSuperseded
	}
	s.epoch.Add(1)
	s.beforeChange()
	email := t.Email
	if strings.TrimSpace(email) == "" {
		email = fallbackEmail
	}
	defer s.afterChange()
	who := strings.TrimSpace(email)
	err := s.Store.Update(func(m *model.Settings) {
		// The sync state is about one account; it is kept through a sign-out and a
		// sign-in to the same one, and dropped when the account is another.
		// While a login is stored, its address is whose the checkpoints are (they
		// are written and kept with it); the record on the side is for when the
		// address has been forgotten by a sign-out.
		prev := strings.TrimSpace(model.Str(m.AuthUserEmail))
		if prev == "" {
			prev = s.syncAccount()
		}
		// With no owner at all (a sign-out by an older client left checkpoints and
		// no address) they cannot be tied to this account, so they are dropped
		// too; the cost is one full comparison on the next sync.
		if prev == "" || !strings.EqualFold(prev, who) {
			m.SyncLastLocalFingerprint, m.SyncLastRemoteVersion = nil, nil
			m.SyncLastRemoteUpdatedAt, m.SyncLastRemoteSha256 = nil, nil
		}
		m.AuthAccessToken = &t.AccessToken
		m.AuthRefreshToken = &t.RefreshToken
		m.AuthIdToken = &t.IDToken
		m.AuthExpiresAtUnixMs = t.ExpiresAt.UnixMilli()
		m.AuthUserEmail = &email
		m.NasAgentBaseUrl = model.OfficialSyncBaseUrl
	})
	if err != nil {
		// The file still has the old login and its checkpoints, so the record of
		// whose they are stays as it is on disk; this run knows the new one.
		s.lastAccount = who
		return err
	}
	s.setSyncAccount(who) // after the settings: the two never disagree on disk
	return nil
}

// Result is what a call did: Message is for the toast, OK says whether it
// worked.
type Result struct {
	OK      bool
	Message string
}

func fail(msg string) Result { return Result{Message: msg} }

// SignIn is LoginAsync. A blank email or password is refused before any call.
func (s *Service) SignIn(ctx context.Context, email, password string) Result {
	return s.enter(ctx, email, password, "登录成功", "登录失败", "登录已取消", s.Client.Login)
}

// Register is RegisterAsync: the server signs the new account in.
func (s *Service) Register(ctx context.Context, email, password string) Result {
	return s.enter(ctx, email, password, "注册成功", "注册失败", "已取消", s.Client.Register)
}

func (s *Service) enter(ctx context.Context, email, password, okMsg, failMsg, cancelMsg string,
	call func(context.Context, string, string) (syncclient.TokenSet, error)) Result {
	email = strings.TrimSpace(email)
	if email == "" || strings.TrimSpace(password) == "" {
		return fail("请输入邮箱和密码")
	}
	ctx, cancel := context.WithTimeout(ctx, signInTimeout)
	defer cancel()
	startEpoch := s.epoch.Load()
	t, err := call(ctx, email, password)
	if err != nil {
		return s.failure(err, failMsg, cancelMsg)
	}
	if err := s.storeTokensAt(t, email, startEpoch); err != nil {
		if errors.Is(err, errSuperseded) {
			return fail("账号状态在登录期间发生了变化，请重新登录")
		}
		// Signed in for this run (the tokens are in memory and work), but the
		// file does not have them: say so, or the user finds out at the next start.
		return Result{OK: true, Message: okMsg + "，但无法保存登录状态，重启后需要重新登录：" + err.Error()}
	}
	return Result{OK: true, Message: okMsg}
}

// SignOut forgets the tokens and the address.
func (s *Service) SignOut() Result {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	s.epoch.Add(1)
	s.beforeChange()
	defer s.afterChange()
	// The sync state belongs to the account that is leaving; the address is
	// forgotten by this, so the record of whose it is has to be there (a login
	// from 1.0.10 has none yet). It is written after the settings, as in
	// storeTokens: if they could not be saved the file still holds the old
	// address and the old checkpoints, and a record naming the account of this
	// run would give those checkpoints to the wrong one.
	leaving := strings.TrimSpace(model.Str(s.Store.Get().AuthUserEmail))
	record, recordErr := os.ReadFile(s.AccountFile)
	preserve := leaving != "" && ((s.AccountFile == "" && strings.EqualFold(s.lastAccount, leaving)) || (recordErr == nil && strings.EqualFold(strings.TrimSpace(string(record)), leaving)))
	if err := s.Store.Update(func(m *model.Settings) {
		syncclient.ClearAuthSession(m, true)
		if !preserve {
			m.SyncLastLocalFingerprint, m.SyncLastRemoteVersion = nil, nil
			m.SyncLastRemoteUpdatedAt, m.SyncLastRemoteSha256 = nil, nil
		}
	}); err != nil {
		if leaving != "" {
			s.lastAccount = leaving // this run knows; the disk is not told
		}
		// The tokens are gone from memory but the file still has them: after
		// a restart the user would be signed in again.
		return fail("已退出，但无法保存设置，重启后可能仍是登录状态：" + err.Error())
	}
	if leaving != "" && !s.setSyncAccount(leaving) {
		// The record on disk can not be made to say whose the checkpoints are, and
		// the old one may name somebody else: no checkpoint may outlive this
		// sign-out, or a later sign-in would take them for its own.
		_ = s.Store.Update(func(m *model.Settings) {
			m.SyncLastLocalFingerprint, m.SyncLastRemoteVersion = nil, nil
			m.SyncLastRemoteUpdatedAt, m.SyncLastRemoteSha256 = nil, nil
		})
	}
	return Result{OK: true, Message: "已退出登录"}
}

// EnsureAccessToken is EnsureAccessTokenAsync: a token that is still good is
// checked with the server (a refused one signs the user out), else the
// refresh token is used. "" means signed out.
func (s *Service) EnsureAccessToken(ctx context.Context) (string, error) {
	// One refresh at a time, the background sessions included; what the
	// other one stored is looked at after the wait.
	// The refresh lock is held while the server is asked, so the asking is
	// bounded: a server that never answers must not hold up every other renewal.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	unlock, lerr := syncclient.LockRefresh(ctx)
	if lerr != nil {
		return "", lerr
	}
	defer unlock()
	cur := s.Store.Get()
	access := strings.TrimSpace(model.Str(cur.AuthAccessToken))
	if access != "" && cur.AuthExpiresAtUnixMs > s.now().UnixMilli()+30_000 {
		email, err := s.Client.CurrentEmail(ctx, access)
		if err != nil {
			// No answer (offline, a reset connection) says nothing about the
			// login: it stays, and the caller is told to try again later.
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", err
		}
		if strings.TrimSpace(email) != "" {
			// The answer may have taken a while: if the user has signed out or
			// in as somebody else since, the token is not theirs to be used.
			if strings.TrimSpace(model.Str(s.Store.Get().AuthAccessToken)) != access {
				return "", nil
			}
			return access, nil
		}
		s.clearSession(model.Str(cur.AuthRefreshToken), access)
		return "", nil
	}
	refresh := strings.TrimSpace(model.Str(cur.AuthRefreshToken))
	if refresh == "" {
		return "", nil
	}
	t, err := s.Client.Refresh(ctx, refresh)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var ae *syncclient.APIError
		if !errors.As(err, &ae) || !refused(ae.StatusCode) {
			// No answer, or a server that is busy or failing (5xx, 429):
			// not a reason to sign out.
			return "", err
		}
		s.clearSession(refresh, access)
		return "", nil
	}
	// The user may have signed out, or in as somebody else, while the request
	// was on its way: the answer belongs to the session that asked.
	same := false
	var issued string // the token of the session that asked, as it was stored
	saveErr := s.Store.Update(func(m *model.Settings) {
		if strings.TrimSpace(model.Str(m.AuthRefreshToken)) != refresh {
			return
		}
		same = true
		issued = t.AccessToken
		m.AuthAccessToken = &t.AccessToken
		if strings.TrimSpace(t.RefreshToken) != "" {
			m.AuthRefreshToken = &t.RefreshToken
		}
		if strings.TrimSpace(t.IDToken) != "" {
			m.AuthIdToken = &t.IDToken
		}
		m.AuthExpiresAtUnixMs = t.ExpiresAt.UnixMilli()
		if strings.TrimSpace(t.Email) != "" {
			m.AuthUserEmail = &t.Email
		}
		m.NasAgentBaseUrl = model.OfficialSyncBaseUrl
	})
	if !same {
		return "", nil
	}
	if saveErr != nil {
		return "", fmt.Errorf("登录已续期，但无法保存设置，重启后可能需要重新登录: %w", saveErr)
	}
	return issued, nil // not read again: the user may be somebody else by now
}

// refused says whether an HTTP status is the server turning the token down
// (as against being unwell).
func refused(status int) bool {
	return status == 400 || status == 401 || status == 403
}

// clearSession signs out, but only the session that was found wanting: if the
// tokens are no longer those, somebody signed in again meanwhile.
func (s *Service) clearSession(refresh, access string) {
	_ = s.Store.Update(func(m *model.Settings) {
		if strings.TrimSpace(model.Str(m.AuthRefreshToken)) != strings.TrimSpace(refresh) ||
			strings.TrimSpace(model.Str(m.AuthAccessToken)) != strings.TrimSpace(access) {
			return
		}
		syncclient.ClearAuthSession(m, false)
	})
}

// Purpose of a password code.
type Purpose string

const (
	Reset  Purpose = "reset"  // forgot password: not signed in
	Change Purpose = "change" // change password: signed in
)

func validEmail(e string) bool { return strings.TrimSpace(e) != "" && strings.Contains(e, "@") }

// SendCode asks for a code by mail. For Change the address is the signed-in
// one; for Reset it is what the user typed. Cooldown is the seconds before
// another code may be asked for.
func (s *Service) SendCode(ctx context.Context, p Purpose, typedEmail string) (cooldown int, r Result) {
	startEpoch := s.epoch.Load()
	var email, token string
	if p == Change {
		email = strings.TrimSpace(model.Str(s.Store.Get().AuthUserEmail))
		if email == "" {
			email = strings.TrimSpace(typedEmail)
		}
	} else {
		email = strings.TrimSpace(typedEmail)
	}
	if !validEmail(email) {
		return 0, fail("请输入有效邮箱")
	}
	ctx, cancel := context.WithTimeout(ctx, passwordTimeout)
	defer cancel()
	if p == Change {
		var err error
		token, err = s.EnsureAccessToken(ctx)
		if s.epoch.Load() != startEpoch {
			return 0, fail("登录已变更，请重试")
		}
		if err == nil && token == "" {
			return 0, fail("登录已失效，请重新登录")
		}
		if err != nil {
			return 0, s.failure(err, "发送验证码失败", "发送验证码已取消")
		}
	}
	cd, err := s.Client.SendPasswordCode(ctx, string(p), email, token)
	if err != nil {
		return 0, s.failure(err, "发送验证码失败", "发送验证码已取消")
	}
	return cd, Result{OK: true, Message: "验证码已发送，请检查邮箱"}
}

func (s *Service) failure(err error, failMsg, cancelMsg string) Result {
	// C# catches OperationCanceledException, which a timeout is too.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fail(cancelMsg)
	}
	return fail(ExplainAuth(err, failMsg))
}

// validateNewPassword is ValidatePasswordRecoveryInputs.
func validateNewPassword(email, code, pw, confirm string) string {
	switch {
	case !validEmail(email):
		return "请输入有效邮箱"
	case strings.TrimSpace(code) == "":
		return "请输入验证码"
	case len(pw) < 8:
		return "新密码至少 8 位"
	case pw != confirm:
		return "两次输入的新密码不一致"
	}
	return ""
}

// ResetPassword sets a new password with the code from the mail, for a user
// who is not signed in.
func (s *Service) ResetPassword(ctx context.Context, email, code, pw, confirm string) Result {
	startEpoch := s.epoch.Load()
	email = strings.TrimSpace(email)
	if msg := validateNewPassword(email, code, pw, confirm); msg != "" {
		return fail(msg)
	}
	ctx, cancel := context.WithTimeout(ctx, passwordTimeout)
	defer cancel()
	if err := s.Client.ResetPassword(ctx, email, code, pw); err != nil {
		return s.failure(err, "找回密码失败", "找回密码已取消")
	}
	return s.passwordUpdated(email, startEpoch)
}

// ChangePassword sets a new password for the signed-in user.
func (s *Service) ChangePassword(ctx context.Context, typedEmail, code, pw, confirm string) Result {
	startEpoch := s.epoch.Load()
	email := strings.TrimSpace(model.Str(s.Store.Get().AuthUserEmail))
	if email == "" {
		email = strings.TrimSpace(typedEmail)
	}
	if msg := validateNewPassword(email, code, pw, confirm); msg != "" {
		return fail(msg)
	}
	ctx, cancel := context.WithTimeout(ctx, passwordTimeout)
	defer cancel()
	token, err := s.EnsureAccessToken(ctx)
	if s.epoch.Load() != startEpoch {
		return fail("登录已变更，请重试")
	}
	if err == nil && token == "" {
		return fail("登录已失效，请重新登录")
	}
	if err != nil {
		return s.failure(err, "修改密码失败", "修改密码已取消")
	}
	if err := s.Client.ChangePassword(ctx, token, code, pw); err != nil {
		return s.failure(err, "修改密码失败", "修改密码已取消")
	}
	return s.passwordUpdated(email, startEpoch)
}

// afterPasswordUpdated is ClearAuthAfterPasswordUpdated: the old session is
// no good, the address stays for the next sign-in. It ends the session the
// request started in (refresh is its refresh token, "" for a reset made while
// signed out); if somebody else is signed in by now, that one is left alone.
func (s *Service) afterPasswordUpdated(email string, epoch int64) error {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	if s.epoch.Load() != epoch {
		return nil // somebody signed in or out since: not the login whose password changed
	}
	s.epoch.Add(1)
	s.beforeChange()
	defer s.afterChange()
	who := strings.TrimSpace(email)
	return s.Store.Update(func(m *model.Settings) {
		// The address is remembered for the next sign-in, but the checkpoints are
		// somebody's: if they are another account's, the address taking their
		// place must not make them look like its own.
		owner := strings.TrimSpace(model.Str(m.AuthUserEmail))
		if owner == "" {
			owner = s.syncAccount()
		}
		if owner == "" || !strings.EqualFold(owner, who) {
			m.SyncLastLocalFingerprint, m.SyncLastRemoteVersion = nil, nil
			m.SyncLastRemoteUpdatedAt, m.SyncLastRemoteSha256 = nil, nil
		}
		m.AuthUserEmail = &email
		syncclient.ClearAuthSession(m, false)
	})
}

// passwordUpdated is the result of a password change or reset that the server
// accepted: the password is changed whatever happens to the local session, so
// a failure to save the cleared session is told apart from a failed change.
func (s *Service) passwordUpdated(email string, epoch int64) Result {
	if err := s.afterPasswordUpdated(email, epoch); err != nil {
		return Result{OK: true, Message: "密码已更新，但无法保存设置，重启后可能仍是旧的登录状态，请重新登录：" + err.Error()}
	}
	return Result{OK: true, Message: "密码已更新，请使用新密码重新登录"}
}

// Subscription is what the cloud tab shows about the plan.
type Subscription struct {
	Status    string // 订阅至 2026-01-02, 已订阅, 未订阅, 未登录, 查询失败, 正在读取中...
	Tier      string
	Storage   string
	Retention string
	Limit     string // 正常 / 已超限（可能影响同步）
	OverLimit bool
}

// Loading is the placeholder while the server is asked.
func Loading() Subscription {
	l := "正在读取中..."
	return Subscription{Status: l, Tier: l, Storage: l, Retention: l, Limit: l}
}

// None is a plan with nothing to show: status says why.
func None(status string) Subscription {
	return Subscription{Status: status, Tier: "-", Storage: "-", Retention: "-", Limit: "-"}
}

// Subscription asks the server (RefreshSubscriptionAsync).
func (s *Service) Subscription(ctx context.Context) Subscription {
	ctx, cancel := context.WithTimeout(ctx, passwordTimeout)
	defer cancel()
	epoch := s.epoch.Load()
	token, err := s.EnsureAccessToken(ctx)
	if err == nil && s.epoch.Load() != epoch {
		return None("未登录") // the login changed while the token was got: it is not this screen's
	}
	if err != nil {
		return None("查询失败") // no answer: the login is still there
	}
	if token == "" {
		return None("未登录")
	}
	st, err := s.Client.SubscriptionStatus(ctx, token)
	if err != nil || st == nil {
		return None("查询失败")
	}
	if !st.Subscribed {
		return None("未订阅")
	}
	out := Subscription{Status: "已订阅"}
	if st.ExpiresAt != nil {
		if t, ok := parseTime(*st.ExpiresAt); ok {
			out.Status = "订阅至 " + t.Format("2006-01-02")
		}
	}
	out.Tier = "未知"
	if st.Tier != nil && strings.TrimSpace(*st.Tier) != "" {
		out.Tier = *st.Tier
	}
	out.Storage = formatStorage(st.StorageBytes, st.StorageLimitBytes)
	out.Retention = "未限制"
	if st.RetentionDays != nil && *st.RetentionDays > 0 {
		out.Retention = fmt.Sprintf("%d 天", *st.RetentionDays)
	}
	switch {
	case st.OverLimit != nil:
		out.OverLimit = *st.OverLimit
	case st.StorageBytes != nil && st.StorageLimitBytes != nil && *st.StorageLimitBytes > 0:
		out.OverLimit = *st.StorageBytes > *st.StorageLimitBytes
	}
	out.Limit = "正常"
	if out.OverLimit {
		out.Limit = "已超限（可能影响同步）"
	}
	return out
}

func parseTime(s string) (time.Time, bool) {
	for _, l := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.9999999", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(l, strings.TrimSpace(s)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Redeem exchanges a card code (RedeemCardAsync).
func (s *Service) Redeem(ctx context.Context, code string) Result {
	code = strings.TrimSpace(code)
	if code == "" {
		return fail("请输入卡密")
	}
	ctx, cancel := context.WithTimeout(ctx, passwordTimeout)
	defer cancel()
	epoch := s.epoch.Load() // the login the card is redeemed for
	token, err := s.EnsureAccessToken(ctx)
	if err != nil {
		return fail("暂时无法连接服务器，请稍后再试")
	}
	if s.epoch.Load() != epoch {
		return fail("账号在兑换期间发生了变化，请重试")
	}
	if token == "" {
		return fail("请先登录")
	}
	res, err := s.Client.RedeemCard(ctx, token, code)
	if err != nil {
		return fail("兑换失败：" + err.Error())
	}
	if res == nil {
		return fail("兑换失败：网络错误")
	}
	if !res.Success {
		e := res.Error
		if e == "" {
			e = "未知错误"
		}
		if strings.HasPrefix(e, "{") || strings.HasPrefix(e, "[") {
			e = "兑换失败"
		}
		return fail(e)
	}
	return Result{OK: true, Message: "兑换成功"}
}

func formatStorage(used, limit *int64) string {
	u, l := "-", "-"
	if used != nil {
		u = formatBytes(*used)
	}
	if limit != nil && *limit > 0 {
		l = formatBytes(*limit)
	}
	return u + " / " + l
}

// formatBytes is FormatBytes: 1024-based, two decimals at most.
func formatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return s + " " + units[i]
}

// ExplainAuth is ExplainAuthException: the server's English reasons in
// Chinese, anything else after the fallback.
func ExplainAuth(err error, fallback string) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "请求超时或已取消，请稍后重试"
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return fallback
	}
	has := func(sub string) bool { return strings.Contains(strings.ToLower(msg), sub) }
	switch {
	case has("invalid email or password"):
		return "邮箱或密码不正确"
	case has("invalid email"):
		return "请输入有效邮箱"
	case has("password must be at least"):
		return "新密码至少 8 位"
	case has("invalid or expired code"):
		return "验证码错误或已过期"
	case has("too many code attempts"):
		return "验证码错误次数过多，请重新获取"
	case has("email service not configured"):
		return "邮件服务尚未配置，请联系管理员"
	case has("failed to send email"):
		return "验证码邮件发送失败，请稍后重试"
	case has("invalid request"):
		return "登录请求格式异常，请重启应用后重试"
	case has("missing") && has("accesstoken"):
		return "登录响应异常，请稍后重试"
	case has("timed out") || has("timeout"):
		return "登录请求超时，请检查网络后重试"
	}
	return fallback + "：" + msg
}
