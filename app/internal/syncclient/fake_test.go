package syncclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"klip/internal/model"
)

var bg = context.Background()

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestAuthErrorsAndTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/auth/login":
			if !strings.Contains(string(body), `"email":"a@b.com"`) { // trimmed
				t.Errorf("login body %s", body)
			}
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"invalid email or password"}`)
		case "/auth/register":
			io.WriteString(w, `{"accessToken":"tok","refreshToken":"ref","email":"a@b.com","accessTokenExpiresAt":"2030-01-02T03:04:05.123456+00:00"}`)
		case "/auth/refresh":
			io.WriteString(w, `{"refreshToken":"r"}`) // no accessToken
		case "/auth/password/code":
			if !strings.Contains(string(body), `"purpose":"change"`) || strings.Contains(string(body), "email") {
				t.Errorf("change code body %s", body)
			}
			io.WriteString(w, `{"success":true,"cooldownSeconds":42}`)
		case "/auth/password/reset":
			w.WriteHeader(503)
			io.WriteString(w, `not json`)
		case "/auth/me":
			if r.Header.Get("Authorization") != "Bearer good" {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, `{"id":"u","email":"me@x.com"}`)
		}
	}))
	defer srv.Close()
	c := New(srv.URL + "/")

	_, err := c.Login(bg, "  a@b.com ", "pw")
	if err == nil || err.Error() != "invalid email or password" || !IsStatus(err, 400) {
		t.Errorf("login error: %v", err)
	}
	tok, err := c.Register(bg, "a@b.com", "password")
	if err != nil || tok.AccessToken != "tok" || tok.RefreshToken != "ref" || tok.Email != "a@b.com" || tok.ExpiresAt.Year() != 2030 {
		t.Errorf("register: %+v %v", tok, err)
	}
	if _, err := c.Refresh(bg, "x"); err == nil || err.Error() != "token response missing accessToken" {
		t.Errorf("refresh without token: %v", err)
	}
	if _, err := c.Refresh(bg, "  "); err == nil {
		t.Error("an empty refresh token is refused before asking")
	}
	if n, err := c.SendPasswordCode(bg, " Change ", "ignored@x.com", "tok"); err != nil || n != 42 {
		t.Errorf("code: %d %v", n, err)
	}
	if err := c.ResetPassword(bg, "a@b.com", "123456", "newpassword"); err == nil || err.Error() != "auth failed (503)" {
		t.Errorf("a body that is not JSON still gives the status: %v", err)
	}
	if e, _ := c.CurrentEmail(bg, "good"); e != "me@x.com" {
		t.Errorf("email %q", e)
	}
	if e, err := c.CurrentEmail(bg, "bad"); e != "" || err != nil {
		t.Errorf("rejected token: %q %v", e, err)
	}
}

func TestAccountCallsFallBackToOfficialServer(t *testing.T) {
	c := New("")
	if got := c.accountURL(); got != "https://maccy.baymaxgroup.com" {
		t.Errorf("%s", got)
	}
	if _, err := c.GetManifestJSON(bg, "t"); err != nil {
		t.Errorf("no server: nothing to ask, as in C#: %v", err)
	}
	if _, err := c.UploadSnapshot(bg, "t", "x"); !errors.Is(err, ErrNoBaseURL) {
		t.Errorf("%v", err)
	}
	if c.Health(bg) {
		t.Error("health without a server")
	}
}

func TestStatusAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/subscription/status":
			switch r.Header.Get("Authorization") {
			case "Bearer ok":
				io.WriteString(w, `{"subscribed":true,"expiresAt":"2030-01-01T00:00:00+00:00","tier":"Mini","storageBytes":"123","storageLimitBytes":104857600,"retentionDays":30,"overLimit":false}`)
			case "Bearer none":
				w.WriteHeader(401)
			case "Bearer forbidden":
				w.WriteHeader(403)
			default:
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				w.Header().Set("X-Auth-Failure", "expired")
				w.WriteHeader(500)
				io.WriteString(w, "boom")
			}
		case "/sync/manifest":
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(403)
			io.WriteString(w, `{"code":"subscription_expired","error":"subscription expired"}`)
		case "/card/redeem":
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"card not found or used"}`)
		case "/health":
			io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)

	st, err := c.SubscriptionStatus(bg, "ok")
	if err != nil || !st.Subscribed || *st.Tier != "Mini" || *st.StorageBytes != 123 || *st.StorageLimitBytes != 104857600 ||
		*st.RetentionDays != 30 || *st.OverLimit || *st.ExpiresAt != "2030-01-01T00:00:00+00:00" {
		t.Errorf("status: %+v %v", st, err)
	}
	for _, tok := range []string{"none", "forbidden"} {
		if st, err := c.SubscriptionStatus(bg, tok); err != nil || st.Subscribed || st.Tier != nil {
			t.Errorf("%s: a refused token is 'not subscribed': %+v %v", tok, st, err)
		}
	}
	_, err = c.SubscriptionStatus(bg, "other")
	var ae *APIError
	if !errors.As(err, &ae) || ae.StatusCode != 500 || ae.Message != "get subscription status failed" ||
		ae.Body != "Bearer error=\"invalid_token\"\nexpired\nboom" {
		t.Errorf("auth headers go in front of the body: %#v", ae)
	}

	_, err = c.GetManifestJSON(bg, "t")
	if !errors.As(err, &ae) || ae.StatusCode != 403 || ae.Code != "subscription_expired" || !strings.HasPrefix(ae.Body, "Bearer\n{") {
		t.Errorf("manifest: %#v", ae)
	}

	r, err := c.RedeemCard(bg, "t", "ABCD")
	if err != nil || r.Success || !strings.Contains(r.Error, "card not found or used") {
		t.Errorf("redeem: %+v %v", r, err)
	}
	if r, _ := c.RedeemCard(bg, "t", "  "); r != nil {
		t.Error("an empty code is not asked")
	}
	if !c.Health(bg) {
		t.Error("health")
	}
}

func TestGzipUploadAndDownload(t *testing.T) {
	var gotBody []byte
	var gotEncoding, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/sync/snapshot":
			gotEncoding, gotType = r.Header.Get("Content-Encoding"), r.Header.Get("Content-Type")
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Errorf("body is not gzip: %v", err)
				return
			}
			gotBody, _ = io.ReadAll(zr)
			io.WriteString(w, `{"version":"1780000000000","sha256":"abc","size":11}`)
		case r.URL.Path == "/sync/snapshot":
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				t.Error("download asks for gzip")
			}
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(gz(t, "from server"))
		case r.URL.Path == "/sync/blobs/plain":
			io.WriteString(w, "not compressed") // a server that ignores Accept-Encoding
		case r.URL.Path == "/sync/blobs/missing":
			w.WriteHeader(404)
		case r.URL.Path == "/sync/blobs/check":
			io.WriteString(w, `{"missing":["aa", 5, "", "bb"]}`)
		}
	}))
	defer srv.Close()
	c := New(srv.URL)
	dir := t.TempDir()

	src := filepath.Join(dir, "s.sqlite")
	os.WriteFile(src, []byte("hello world"), 0o644)
	res, err := c.UploadSnapshot(bg, "t", src)
	if err != nil || res.Version != "1780000000000" || res.SHA256 != "abc" || *res.Size != 11 {
		t.Fatalf("upload: %+v %v", res, err)
	}
	if string(gotBody) != "hello world" || gotEncoding != "gzip" || gotType != "application/octet-stream" {
		t.Errorf("server saw %q %q %q", gotBody, gotEncoding, gotType)
	}

	dst := filepath.Join(dir, "sub", "down.sqlite")
	if err := c.DownloadSnapshot(bg, "t", dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "from server" {
		t.Errorf("downloaded %q", b)
	}
	if _, err := os.Stat(dst + ".download"); err == nil {
		t.Error("the temporary file is left")
	}
	// An existing file is replaced; an uncompressed answer is taken as it is.
	if err := c.DownloadBlob(bg, "t", "plain", dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "not compressed" {
		t.Errorf("replaced with %q", b)
	}
	if err := c.DownloadBlob(bg, "t", "missing", dst); !IsStatus(err, 404) {
		t.Errorf("404: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "not compressed" {
		t.Errorf("a failed download must leave the file alone: %q", b)
	}

	missing, err := c.CheckMissingBlobs(bg, "t", []string{"aa", "bb"})
	if err != nil || strings.Join(missing, ",") != "aa,bb" {
		t.Errorf("missing %v %v", missing, err)
	}
	if m, err := c.CheckMissingBlobs(bg, "t", nil); err != nil || m != nil {
		t.Errorf("no hashes, no call: %v %v", m, err)
	}
	if _, err := c.UploadSnapshot(bg, "t", filepath.Join(dir, "none")); err == nil {
		t.Error("a missing file")
	}
	if err := c.UploadBlob(bg, "t", " ", src); err == nil {
		t.Error("an empty hash")
	}
}

func TestParseEvents(t *testing.T) {
	stream := ": keepalive\n\nevent: connected\ndata: {\"serverTime\":\"x\"}\n\n" +
		"event:sync-updated\r\ndata:  {\"a\":1}\r\ndata: line2\r\n\r\n" +
		"id: 7\nretry: 5\ndata: no event name\n\n" +
		"event: tail\ndata: unfinished"
	var got []Event
	if err := parseEvents(strings.NewReader(stream), func(e Event) { got = append(got, e) }); err != nil {
		t.Fatal(err)
	}
	want := []Event{
		{"", ""}, // the comment, then an empty line: C# sends it too, and ignores it by name
		{"connected", `{"serverTime":"x"}`},
		{"sync-updated", "{\"a\":1}\nline2"},
		{"", "no event name"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d events: %+v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: %+v want %+v", i, got[i], want[i])
		}
	}
}

// fakeTokens is a TokenSource that counts.
type fakeTokens struct {
	mu        sync.Mutex
	token     string
	refreshes int
	canRenew  bool
}

func (f *fakeTokens) AccessToken(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token, nil
}
func (f *fakeTokens) Refresh(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshes++
	if f.canRenew {
		f.token = "fresh"
	}
	return f.canRenew, nil
}

func TestWatchRefreshesReconnectsAndDebounces(t *testing.T) {
	var connects atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connects.Add(1)
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(401) // the first connection: an expired token
			return
		}
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: connected\ndata: {}\n\n")
		if n == 2 {
			// Three updates in a burst are one call; the stream then ends and
			// the client has to come back.
			for i := 0; i < 3; i++ {
				io.WriteString(w, "event: sync-updated\ndata: {}\n\n")
			}
			fl.Flush()
			time.Sleep(50 * time.Millisecond)
			return
		}
		io.WriteString(w, "event: sync-updated\ndata: {}\n\n")
		fl.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ts := &fakeTokens{token: "stale", canRenew: true}
	c := New(srv.URL)
	ctx, cancel := context.WithCancel(bg)
	var updates atomic.Int32
	done := make(chan struct{})
	go func() {
		c.Watch(ctx, ts, WatchOptions{InitialDelay: 10 * time.Millisecond, MaxDelay: 40 * time.Millisecond, Debounce: 20 * time.Millisecond},
			func() { updates.Add(1) })
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for updates.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch does not stop with its context")
	}
	if ts.refreshes != 1 {
		t.Errorf("refreshes %d", ts.refreshes)
	}
	if connects.Load() < 3 {
		t.Errorf("connections %d: refused, a burst, then the one that stays", connects.Load())
	}
	if got := updates.Load(); got != 2 {
		t.Errorf("updates %d: the burst is one, the last stream's is another", got)
	}
}

func TestWatchBacksOffWhenRefreshFails(t *testing.T) {
	var connects atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connects.Add(1)
		w.WriteHeader(401)
	}))
	defer srv.Close()
	ts := &fakeTokens{token: "stale", canRenew: false}
	ctx, cancel := context.WithTimeout(bg, 400*time.Millisecond)
	defer cancel()
	New(srv.URL).Watch(ctx, ts, WatchOptions{InitialDelay: 50 * time.Millisecond, MaxDelay: 100 * time.Millisecond}, func() {})
	// Delays of 50, 100, 100, ... ms: about five tries, not a tight loop.
	if n := connects.Load(); n < 3 || n > 8 {
		t.Errorf("connections %d", n)
	}
}

func TestSessionRefreshStoresAndSignsOut(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(401)
			io.WriteString(w, `{}`)
			return
		}
		io.WriteString(w, `{"accessToken":"new-access","refreshToken":"new-refresh","email":"me@x.com","accessTokenExpiresAt":"2030-01-01T00:00:00+00:00"}`)
	}))
	defer srv.Close()

	s := model.DefaultSettings()
	s.AuthAccessToken, s.AuthRefreshToken = model.Ptr("old"), model.Ptr("old-refresh")
	s.NasAgentBaseUrl = "http://elsewhere"
	now := time.UnixMilli(1_000_000)
	sess := &Session{
		Client:   New(srv.URL),
		Settings: Settings{Get: func() model.Settings { return s }, Update: func(f func(*model.Settings)) error { f(&s); return nil }},
		Now:      func() time.Time { return now },
	}

	// A token with more than 30 s left is used as it is.
	s.AuthExpiresAtUnixMs = now.UnixMilli() + 31_000
	if tok, err := sess.AccessToken(bg); err != nil || tok != "old" {
		t.Errorf("valid token: %q %v", tok, err)
	}
	// With less, it is renewed and stored.
	s.AuthExpiresAtUnixMs = now.UnixMilli() + 29_000
	tok, err := sess.AccessToken(bg)
	if err != nil || tok != "new-access" || model.Str(s.AuthRefreshToken) != "new-refresh" ||
		model.Str(s.AuthUserEmail) != "me@x.com" || s.AuthExpiresAtUnixMs != 1893456000000 || s.NasAgentBaseUrl != model.OfficialSyncBaseUrl {
		t.Errorf("renewed: %q %v %+v", tok, err, s)
	}

	// The server refuses the refresh token: signed out, email kept.
	fail.Store(true)
	ok, err := sess.Refresh(bg)
	if ok || err != nil || s.AuthAccessToken != nil || s.AuthRefreshToken != nil || model.Str(s.AuthUserEmail) != "me@x.com" || s.AuthExpiresAtUnixMs != 0 {
		t.Errorf("signed out: %v %v %+v", ok, err, s)
	}

	// No answer at all (the server is down): the tokens stay.
	s.AuthAccessToken, s.AuthRefreshToken = model.Ptr("a"), model.Ptr("r")
	srv.Close()
	ok, err = sess.Refresh(bg)
	if ok || err == nil || model.Str(s.AuthRefreshToken) != "r" {
		t.Errorf("offline: %v %v %+v", ok, err, s)
	}
	// And with no refresh token there is nothing to do.
	s.AuthRefreshToken = nil
	if ok, err := sess.Refresh(bg); ok || err != nil {
		t.Errorf("%v %v", ok, err)
	}
}

func TestGetRetriesOnceThenReportsTimeout(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close() // a dropped connection
	}))
	defer srv.Close()
	_, err := New(srv.URL).GetManifestJSON(bg, "t")
	if err == nil || hits.Load() != 2 {
		t.Errorf("hits %d, err %v: one retry after a connection error", hits.Load(), err)
	}
}

// A 200 with something that is not the auth server's JSON is not a success.
func TestPasswordCallsRejectANonJSONSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html>captive portal</html>`)
	}))
	defer srv.Close()
	c := New(srv.URL + "/")
	if _, err := c.SendPasswordCode(bg, "reset", "a@b.com", ""); err == nil {
		t.Error("an HTML 200 was taken as a sent code")
	}
	if err := c.ResetPassword(bg, "a@b.com", "123456", "newpassword"); err == nil {
		t.Error("an HTML 200 was taken as a reset")
	}
	if err := c.ChangePassword(bg, "tok", "123456", "newpassword"); err == nil {
		t.Error("an HTML 200 was taken as a change")
	}
}

// A subscription or a redeemed card whose answer is cut short, or is not an
// object, is an error, not "not subscribed" or "not redeemed".
func TestCutShortAnswersAreErrors(t *testing.T) {
	for _, body := range []string{"cut", "bad"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if body == "bad" {
				w.Write([]byte("<html>proxy</html>"))
				return
			}
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"subscribed":tr`))
		}))
		c := New(srv.URL)
		if st, err := c.SubscriptionStatus(context.Background(), "token"); err == nil {
			t.Errorf("%s: subscription %+v without an error", body, st)
		}
		if r, err := c.RedeemCard(context.Background(), "token", "CODE"); err == nil {
			t.Errorf("%s: redeem %+v without an error", body, r)
		}
		srv.Close()
	}
}

// Downloads of the same target at the same time each write a file of their
// own: every one goes through, and the target is one of them, whole.
func TestConcurrentDownloadsOfOneTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "blobs", "abc")
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := strings.Repeat(string(rune('a'+i)), 64*1024)
			resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
			errs <- saveBody(resp, target)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(target)
	if err != nil || len(b) != 64*1024 || strings.Count(string(b), string(b[0])) != len(b) {
		t.Fatalf("the target: %d bytes, %v", len(b), err)
	}
	ents, _ := os.ReadDir(filepath.Dir(target))
	if len(ents) != 1 {
		t.Fatalf("left behind: %d files", len(ents))
	}
}
