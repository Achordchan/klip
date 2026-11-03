package syncclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"klip/internal/model"
)

// refreshMu serializes token renewals of the process. Sessions that share
// the account (the syncer's and the event stream's) read the same refresh
// token; the server revokes it on first use, so the second request would
// fail with 401 and could clear the tokens the first one just stored.
var refreshMu sync.Mutex

// Settings is where a Session keeps the sign-in: the app's settings, behind
// two functions so that the owner can lock and save them. Update applies a
// change and saves it; its error says the save failed (the change is still
// made, in memory), so that what must not be lost is not taken as recorded.
type Settings struct {
	Get    func() model.Settings
	Update func(func(*model.Settings)) error
}

// Session is the signed-in state kept in the settings: it hands out a valid
// access token and renews the tokens, as SyncEventStreamService and
// SyncService do. It is a TokenSource.
type Session struct {
	Client   *Client
	Settings Settings
	// Now is time.Now unless a test sets it.
	Now func() time.Time
	// BaseURL is the address to refresh against (the official server).
	BaseURL string
}

func (s *Session) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// AccessToken returns the stored token when it has more than 30 seconds
// left, else renews it.
func (s *Session) AccessToken(ctx context.Context) (string, error) {
	cur := s.Settings.Get()
	access := strings.TrimSpace(model.Str(cur.AuthAccessToken))
	if access != "" && cur.AuthExpiresAtUnixMs > s.now().UnixMilli()+30_000 {
		return access, nil
	}
	ok, err := s.Refresh(ctx)
	if err != nil || !ok {
		return "", err
	}
	return model.Str(s.Settings.Get().AuthAccessToken), nil
}

// Refresh trades the refresh token for new tokens and stores them. A refusal
// of the refresh token itself signs the user out (the tokens are cleared)
// and says false; a temporary failure, and a network error, leave everything
// as it is and return the error.
//
// Renewals of sessions that share the account are serialized, and a session
// that waited for another one uses the tokens it stored instead of spending
// a refresh token that is already gone.
func (s *Session) Refresh(ctx context.Context) (bool, error) {
	before := s.Settings.Get()
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	unlock, err := LockRefresh(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	refreshMu.Lock()
	defer refreshMu.Unlock()

	// Another session may have renewed while this one waited.
	cur := s.Settings.Get()
	access := strings.TrimSpace(model.Str(cur.AuthAccessToken))
	if access != "" && access != strings.TrimSpace(model.Str(before.AuthAccessToken)) &&
		cur.AuthExpiresAtUnixMs > s.now().UnixMilli()+30_000 {
		return true, nil
	}
	refresh := strings.TrimSpace(model.Str(cur.AuthRefreshToken))
	if refresh == "" {
		return false, nil
	}

	token, err := s.Client.Refresh(ctx, refresh)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		var ae *APIError
		if !errors.As(err, &ae) || !refreshRejected(ae) {
			return false, err // no answer, or a server that is unwell: keep the tokens
		}
		_ = s.Settings.Update(func(m *model.Settings) {
			// The user may have signed out, or into another account, while
			// the request was in flight: its refusal is not about that
			// session, so leave it alone.
			if !sameRefreshToken(m, refresh) {
				return
			}
			ClearAuthSession(m, false)
		})
		return false, nil
	}

	applied := false
	serr := s.Settings.Update(func(m *model.Settings) {
		// The tokens belong to the session that asked for them. If the user
		// signed out or in while the request was in flight, do not resurrect
		// the old one (nor overwrite the new one) with this answer.
		if !sameRefreshToken(m, refresh) {
			return
		}
		m.AuthAccessToken = &token.AccessToken
		if strings.TrimSpace(token.RefreshToken) != "" {
			m.AuthRefreshToken = &token.RefreshToken
		}
		if strings.TrimSpace(token.IDToken) != "" {
			m.AuthIdToken = &token.IDToken
		}
		m.AuthExpiresAtUnixMs = token.ExpiresAt.UnixMilli()
		if strings.TrimSpace(token.Email) != "" {
			m.AuthUserEmail = &token.Email
		}
		m.NasAgentBaseUrl = model.OfficialSyncBaseUrl
		applied = true
	})
	if !applied {
		return false, nil
	}
	if serr != nil {
		// The server has spent the old refresh token: the new one is in
		// memory only, and a restart now would find a refused token and sign
		// the user out. The error says so; the next save of the settings
		// writes the tokens with it.
		return true, fmt.Errorf("save the renewed tokens: %w", serr)
	}
	return true, nil
}

// sameRefreshToken says the settings still hold the session that sent this
// refresh token.
func sameRefreshToken(m *model.Settings, refresh string) bool {
	return strings.TrimSpace(model.Str(m.AuthRefreshToken)) == refresh
}

// refreshRejected says the server refused the refresh token itself: 400 for
// a malformed one, 401 for an unknown, revoked or expired one. Anything else
// (429, 5xx, a proxy error) is temporary and must not sign the user out.
func refreshRejected(e *APIError) bool {
	return e.StatusCode == 400 || e.StatusCode == 401
}

var refreshSem = make(chan struct{}, 1)

// refreshTimeout bounds one refresh request, and so how long the lock is held.
const refreshTimeout = 30 * time.Second

// LockRefresh takes the refresh lock that Session.Refresh takes; whoever else
// refreshes the stored tokens (the account window) holds it too, and looks at
// the stored tokens again after getting it. Waiting for it ends with ctx.
func LockRefresh(ctx context.Context) (unlock func(), err error) {
	select {
	case refreshSem <- struct{}{}:
		return func() { <-refreshSem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
