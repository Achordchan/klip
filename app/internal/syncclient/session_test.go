package syncclient

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"klip/internal/model"
)

// A server that cannot answer is not a reason to sign the user out: the
// tokens stay and the caller sees the failure.
func TestSessionRefreshKeepsTheTokensWhenTheServerIsUnwell(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":"down for maintenance"}`)
	}))
	defer srv.Close()

	s := model.DefaultSettings()
	s.AuthAccessToken, s.AuthRefreshToken = model.Ptr("access"), model.Ptr("refresh")
	s.AuthExpiresAtUnixMs = 1234
	sess := &Session{
		Client:   New(srv.URL),
		Settings: Settings{Get: func() model.Settings { return s }, Update: func(f func(*model.Settings)) error { f(&s); return nil }},
	}

	ok, err := sess.Refresh(context.Background())
	if ok || err == nil {
		t.Fatalf("a 503: %v %v", ok, err)
	}
	if model.Str(s.AuthRefreshToken) != "refresh" || model.Str(s.AuthAccessToken) != "access" || s.AuthExpiresAtUnixMs != 1234 {
		t.Errorf("the tokens were cleared: %+v", s)
	}
}

// Two sessions of one account renew together: the server revokes the refresh
// token when it is used, so the second must wait and use the new tokens.
func TestSessionRefreshIsCoordinatedAcrossSessions(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"accessToken":"new-access","refreshToken":"new-refresh","email":"me@x.com",`+
			`"accessTokenExpiresAt":"2030-01-01T00:00:00+00:00"}`)
	}))
	defer srv.Close()

	var mu sync.Mutex
	s := model.DefaultSettings()
	s.AuthAccessToken, s.AuthRefreshToken = model.Ptr("old-access"), model.Ptr("old-refresh")
	s.AuthExpiresAtUnixMs = time.Now().Add(-time.Hour).UnixMilli()
	settings := Settings{
		Get:    func() model.Settings { mu.Lock(); defer mu.Unlock(); return s },
		Update: func(f func(*model.Settings)) error { mu.Lock(); defer mu.Unlock(); f(&s); return nil },
	}
	session := func() *Session { return &Session{Client: New(srv.URL), Settings: settings} }

	var wg sync.WaitGroup
	tokens := make([]string, 2)
	errs := make([]error, 2)
	for i := range tokens {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i], errs[i] = session().AccessToken(context.Background())
		}(i)
	}
	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Errorf("the refresh token was spent %d times", n)
	}
	for i := range tokens {
		if errs[i] != nil || tokens[i] != "new-access" {
			t.Errorf("session %d: %q %v", i, tokens[i], errs[i])
		}
	}
	if got := model.Str(s.AuthRefreshToken); got != "new-refresh" {
		t.Errorf("stored refresh token %q", got)
	}
}

// Account calls are bounded: a server that accepts the request and never
// answers must not hold the sync gate forever.
func TestAccountCallsHaveADeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	c := New(srv.URL)
	c.accountTimeout = 30 * time.Millisecond
	started := time.Now()
	if _, err := c.Login(context.Background(), "a@b.com", "pw"); err == nil {
		t.Fatal("a hanging account call must fail")
	}
	if d := time.Since(started); d > time.Second {
		t.Errorf("the call took %v", d)
	}
	if _, err := c.CurrentEmail(context.Background(), "token"); err == nil {
		t.Error("a hanging /auth/me must fail too")
	}
}

// A blob check that cannot be read, or that has no list, must not look like
// "all blobs are there": the snapshot would be published without them.
func TestCheckMissingBlobsRejectsMalformedResponses(t *testing.T) {
	for _, body := range []string{
		`{"missing":`,
		`{}`,
		`{"missing":null}`,
		`{"missing":"aa"}`,
	} {
		t.Run(body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, body)
			}))
			defer srv.Close()
			if _, err := New(srv.URL).CheckMissingBlobs(bg, "t", []string{"aa"}); err == nil {
				t.Errorf("%s: an invalid answer was taken as an empty list", body)
			}
		})
	}
}

// A refresh that was in flight while the user signed into another account
// must not resurrect the old session.
func TestSessionRefreshDoesNotOverwriteANewSignIn(t *testing.T) {
	for _, status := range []int{200, 401} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				if status == 200 {
					io.WriteString(w, `{"accessToken":"old-access-2","refreshToken":"old-refresh-2","accessTokenExpiresAt":"2030-01-01T00:00:00+00:00"}`)
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{}`)
			}))
			defer srv.Close()

			var mu sync.Mutex
			s := model.DefaultSettings()
			s.AuthAccessToken, s.AuthRefreshToken = model.Ptr("old-access"), model.Ptr("old-refresh")
			settings := Settings{
				Get:    func() model.Settings { mu.Lock(); defer mu.Unlock(); return s },
				Update: func(f func(*model.Settings)) error { mu.Lock(); defer mu.Unlock(); f(&s); return nil },
			}
			sess := &Session{Client: New(srv.URL), Settings: settings}
			done := make(chan struct{})
			go func() {
				sess.Refresh(context.Background())
				close(done)
			}()
			<-entered
			// The user signs into another account while the request is in flight.
			mu.Lock()
			s.AuthAccessToken, s.AuthRefreshToken = model.Ptr("new-access"), model.Ptr("new-refresh")
			mu.Unlock()
			close(release)
			<-done

			if got := model.Str(s.AuthAccessToken); got != "new-access" {
				t.Errorf("access token %q: the old session was resurrected or the new one cleared", got)
			}
			if got := model.Str(s.AuthRefreshToken); got != "new-refresh" {
				t.Errorf("refresh token %q", got)
			}
		})
	}
}

// The event stream must not lose an update that arrives while onUpdated is
// still running: the sync it started may have read an older manifest.
func TestWatchKeepsAnUpdateThatArrivesDuringTheCallback(t *testing.T) {
	var calls atomic.Int32
	second := make(chan struct{})
	written := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/events" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		io.WriteString(w, "event: sync-updated\ndata: {}\n\n")
		fl.Flush()
		<-second
		io.WriteString(w, "event: sync-updated\ndata: {}\n\n")
		fl.Flush()
		once.Do(func() { close(written) })
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		New(srv.URL).Watch(ctx, &fakeTokens{token: "t"}, WatchOptions{
			InitialDelay: 10 * time.Millisecond, MaxDelay: 10 * time.Millisecond, Debounce: 10 * time.Millisecond,
		}, func() {
			if calls.Add(1) == 1 {
				<-release
			}
		})
		close(done)
	}()

	waitUntil(t, "the first callback", func() bool { return calls.Load() == 1 })
	close(second)
	<-written
	time.Sleep(30 * time.Millisecond) // the stream is read while the callback runs
	close(release)
	waitUntil(t, "the second callback", func() bool { return calls.Load() >= 2 })
	cancel()
	<-done
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestAccountIDReadsTheTokenSubject(t *testing.T) {
	jwt := func(payload string) string {
		return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
	}
	if got := AccountID(jwt(`{"sub":"user-a","email":"a@example.com"}`)); got != "user-a" {
		t.Errorf("subject: %q", got)
	}
	if got := AccountID(jwt(`{"email":"b@example.com"}`)); got != "b@example.com" {
		t.Errorf("email fallback: %q", got)
	}
	for _, token := range []string{"", "opaque", "a.b", "header.not-base64!.signature"} {
		if got := AccountID(token); got != "" {
			t.Errorf("%q: %q", token, got)
		}
	}
}

// A reconnect is a reason to reconcile: events published while the watcher
// was away are not replayed by the server.
func TestWatchReconcilesAfterConnecting(t *testing.T) {
	var calls atomic.Int32
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/events" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		once.Do(func() {
			io.WriteString(w, "event: connected\ndata: {}\n\n")
			fl.Flush()
		})
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		New(srv.URL).Watch(ctx, &fakeTokens{token: "t"}, WatchOptions{
			InitialDelay: 10 * time.Millisecond, MaxDelay: 10 * time.Millisecond, Debounce: 10 * time.Millisecond,
		}, func() { calls.Add(1) })
		close(done)
	}()
	waitUntil(t, "the reconciliation after connect", func() bool { return calls.Load() >= 1 })
	cancel()
	<-done
}

// A server that answers 200 and hangs up at once must not cause a tight
// reconnect loop.
func TestWatchBacksOffOnImmediateCleanEOF(t *testing.T) {
	var connects atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/events" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		connects.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	New(srv.URL).Watch(ctx, &fakeTokens{token: "t"}, WatchOptions{
		InitialDelay: 20 * time.Millisecond, MaxDelay: 40 * time.Millisecond,
	}, func() {})
	if n := connects.Load(); n > 30 {
		t.Errorf("connections %d: a clean EOF reconnected without a delay", n)
	}
}

// A 200 with no usable receipt is not a successful upload.
func TestUploadSnapshotRejectsAnEmptyReceipt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	src := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := os.WriteFile(src, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(srv.URL).UploadSnapshot(bg, "t", src); err == nil {
		t.Error("an empty upload receipt was accepted")
	}
}

// Renewed tokens that cannot be saved are an error: the server has spent the
// old refresh token, and a restart would find it refused.
func TestSessionRefreshReportsUnsavedTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"accessToken":"new","refreshToken":"new-refresh","accessTokenExpiresAt":"2030-01-02T03:04:05+00:00"}`)
	}))
	defer srv.Close()
	s := model.DefaultSettings()
	s.AuthRefreshToken = model.Ptr("old-refresh")
	sess := &Session{Client: New(srv.URL), Settings: Settings{
		Get:    func() model.Settings { return s },
		Update: func(f func(*model.Settings)) error { f(&s); return errors.New("disk full") },
	}}
	ok, err := sess.Refresh(context.Background())
	if !ok || err == nil {
		t.Fatalf("refresh: %v %v", ok, err)
	}
	if model.Str(s.AuthRefreshToken) != "new-refresh" {
		t.Fatal("the renewed tokens are not in memory")
	}
}
