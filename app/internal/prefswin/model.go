// Package prefswin is the preferences window: general, content, shelf and
// cloud-sync pages, with the account (sign in, register, password codes, plan,
// card) on the last. The window is PreferencesWindow.axaml; the rules are in
// packages prefs and account, which this only draws and calls.
package prefswin

import (
	"context"
	"reflect"
	"sync"
	"time"

	"klip/internal/account"
	"klip/internal/prefs"
)

// Host is what the window asks of the program around it.
type Host interface {
	// OpenStorage shows the data folder in the file manager.
	OpenStorage()
	// CheckUpdates starts the update check (may be nil).
	CheckUpdates() func()
	// SyncNow starts a sync the user asked for.
	SyncNow()
	// AccountChanged says the user signed in or out (a sign-in starts a sync).
	AccountChanged(signedIn bool)
	// Close hides the window.
	Close()
}

// Model is the state of the window.
type Model struct {
	Prefs   *prefs.Controller
	Account *account.Service
	Host    Host
	subGen  int // moves whenever the subscription shown stops being the current one
	// Post runs f on the UI thread (Window.Update). Tests may run it at once.
	Post func(f func())
	// Redraw asks for a frame (any goroutine); may be nil.
	Redraw func()
	// SyncStatus is the sync's last result, for the page (may be nil).
	SyncStatus func() string
	// Now is time.Now unless a test sets it.
	Now func() time.Time

	tab      int
	v        prefs.View
	observed prefs.View

	// account page
	email, password string
	authBusy        bool
	signedIn        bool
	statusText      string
	sub             account.Subscription
	redeem          string
	redeemBusy      bool

	// password dialogs
	forgot, change bool
	code, pw1, pw2 string
	codeBusy       bool
	cooldown       int
	cooldownGen    int

	mu     sync.Mutex
	toasts []string
	later  time.Time // when the debounced "设置已生效" is due; zero: none

	toastMsg   string
	toastUntil time.Time

	wg sync.WaitGroup
}

// New returns a Model; call Open when the window is shown.
func New(p *prefs.Controller, a *account.Service, h Host) *Model {
	return &Model{Prefs: p, Account: a, Host: h, Now: time.Now, Post: func(f func()) { f() }}
}

// Wait blocks until the calls running in the background are done (tests).
func (m *Model) Wait() { m.wg.Wait() }

func (m *Model) toast(msg string) {
	if msg == "" {
		return
	}
	m.mu.Lock()
	m.toasts = append(m.toasts, msg)
	m.mu.Unlock()
	if m.Redraw != nil {
		m.Redraw()
	}
}

func (m *Model) takeToasts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.toasts
	m.toasts = nil
	return t
}

// Open is for each time the window is shown: it reads the settings again,
// and, when signed in, asks the server about the plan.
func (m *Model) Open() {
	m.v = m.Prefs.Open()
	m.observed = m.v
	m.refreshAccount()
	m.closeDialogs()
	m.sub = account.None("未登录")
	m.subGen++
	if m.signedIn {
		m.loadSubscription()
	}
}

// followLogin brings the account page in line with the login as it is: the
// tokens may have been taken away in the background (a refresh the server
// refused), and the page would go on showing the account.
func (m *Model) followLogin() {
	if now := m.Account.SignedIn(); now != m.signedIn {
		m.refreshAccount()
		m.closeDialogs()
		m.sub = account.None("未登录")
		m.subGen++
		if m.signedIn {
			m.loadSubscription()
		}
	}
}

func (m *Model) refreshAccount() {
	m.signedIn = m.Account.SignedIn()
	m.statusText = m.Account.StatusText()
	if e := m.Account.Email(); e != "" {
		m.email = e
	}
	m.password = ""
}

// async runs work off the UI thread and then done on it.
func (m *Model) async(work func(ctx context.Context) (done func())) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		done := work(context.Background())
		m.Post(func() {
			if done != nil {
				done()
			}
			if m.Redraw != nil {
				m.Redraw()
			}
		})
	}()
}

func (m *Model) loadSubscription() {
	m.sub = account.Loading()
	m.subGen++
	gen := m.subGen // an answer that comes after the account changed is not for this screen
	m.async(func(ctx context.Context) func() {
		s := m.Account.Subscription(ctx)
		return func() {
			if gen != m.subGen {
				return
			}
			m.sub = s
			m.refreshAccount() // a token the server refused signs the user out
		}
	})
}

func (m *Model) signIn(register bool) {
	if m.authBusy {
		return
	}
	m.authBusy = true
	email, pw := m.email, m.password
	m.async(func(ctx context.Context) func() {
		var r account.Result
		if register {
			r = m.Account.Register(ctx, email, pw)
		} else {
			r = m.Account.SignIn(ctx, email, pw)
		}
		return func() {
			m.authBusy = false
			m.toast(r.Message)
			if !r.OK {
				return
			}
			m.refreshAccount()
			m.loadSubscription()
			m.Host.AccountChanged(true)
		}
	})
}

func (m *Model) signOut() {
	r := m.Account.SignOut()
	m.refreshAccount()
	m.sub = account.None("未登录")
	m.subGen++
	m.toast(r.Message)
	m.Host.AccountChanged(false)
}

func (m *Model) openForgot() {
	if m.signedIn {
		return
	}
	m.change, m.forgot = false, true
	m.clearCodeInputs()
}

func (m *Model) openChange() {
	if !m.signedIn {
		return
	}
	m.forgot, m.change = false, true
	m.clearCodeInputs()
}

func (m *Model) closeDialogs() {
	m.forgot, m.change = false, false
	m.clearCodeInputs()
}

func (m *Model) clearCodeInputs() { m.code, m.pw1, m.pw2 = "", "", "" }

func (m *Model) sendCode() {
	if m.codeBusy || m.cooldown > 0 {
		return
	}
	purpose := account.Reset
	if m.change {
		purpose = account.Change
	} else if !m.forgot {
		return
	}
	m.codeBusy = true
	email := m.email
	m.async(func(ctx context.Context) func() {
		cd, r := m.Account.SendCode(ctx, purpose, email)
		return func() {
			m.codeBusy = false
			m.toast(r.Message)
			if r.OK {
				m.startCooldown(cd)
			} else {
				m.refreshAccount()
			}
		}
	})
}

// startCooldown counts the seconds down to the next code, one a second. A
// newer countdown replaces an older one.
func (m *Model) startCooldown(seconds int) {
	m.cooldownGen++
	gen := m.cooldownGen
	m.cooldown = max(0, seconds)
	n := m.cooldown
	if n == 0 {
		return
	}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for ; n > 0; n-- {
			<-t.C
			m.Post(func() {
				if gen != m.cooldownGen || m.cooldown <= 0 {
					return
				}
				m.cooldown--
				if m.Redraw != nil {
					m.Redraw()
				}
			})
		}
	}()
}

func (m *Model) submitPassword() {
	if m.codeBusy {
		return
	}
	forgot := m.forgot
	m.codeBusy = true
	email, code, p1, p2 := m.email, m.code, m.pw1, m.pw2
	m.async(func(ctx context.Context) func() {
		var r account.Result
		if forgot {
			r = m.Account.ResetPassword(ctx, email, code, p1, p2)
		} else {
			r = m.Account.ChangePassword(ctx, email, code, p1, p2)
		}
		return func() {
			m.codeBusy = false
			m.toast(r.Message)
			if r.OK {
				m.closeDialogs()
				m.refreshAccount()
				if m.Account.SignedIn() {
					// Somebody is signed in still: the service left a newer login
					// alone, so there is nothing to show as signed out. (The login
					// state itself is asked, not a counter of this window's.)
					return
				}
				m.sub = account.None("未登录")
				m.subGen++
				m.Host.AccountChanged(false)
			}
		}
	})
}

func (m *Model) redeemCard() {
	if m.redeemBusy {
		return
	}
	m.redeemBusy = true
	code := m.redeem
	gen := m.subGen
	m.async(func(ctx context.Context) func() {
		r := m.Account.Redeem(ctx, code)
		var s account.Subscription
		if r.OK {
			s = m.Account.Subscription(ctx)
		}
		return func() {
			m.redeemBusy = false
			m.toast(r.Message)
			if r.OK {
				m.redeem = ""
				if gen == m.subGen { // still the account the card was redeemed for
					m.sub = s
				}
				// A card may be what the sign-in's first sync was waiting
				// for (no subscription): sync now, as after a sign-in.
				m.Host.AccountChanged(true)
			}
		}
	})
}

// followSettings updates fields unchanged locally, preserving in-progress edits.
func (m *Model) followSettings() {
	next := m.Prefs.CurrentView()
	cur, old, fresh := reflect.ValueOf(&m.v).Elem(), reflect.ValueOf(m.observed), reflect.ValueOf(next)
	for i := 0; i < cur.NumField(); i++ {
		if reflect.DeepEqual(cur.Field(i).Interface(), old.Field(i).Interface()) {
			cur.Field(i).Set(fresh.Field(i))
		}
	}
	m.observed = next
}
