package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeNet answers requests by their full address, after a delay: GitHub and
// the proxies, without the network.
type fakeNet map[string]fakeAnswer

type fakeAnswer struct {
	delay  time.Duration
	status int
	body   []byte
}

func (f fakeNet) RoundTrip(r *http.Request) (*http.Response, error) {
	a, ok := f[r.URL.String()]
	if !ok {
		return nil, &httpError{502}
	}
	select {
	case <-time.After(a.delay):
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	st := a.status
	if st == 0 {
		st = 200
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(bytes.NewReader(a.body)), Header: http.Header{}, Request: r}, nil
}

const ghManifest = "https://github.com/Achordchan/klip/releases/latest/download/manifest.signed.json"

func signedManifest(t *testing.T, key ed25519.PrivateKey, version string, extra map[string]any) []byte {
	t.Helper()
	m := map[string]any{"appId": "maccy", "latest": map[string]any{"version": version, "notes": "n",
		"installer": map[string]any{"url": "https://github.com/Achordchan/klip/releases/download/v" + version + "/Klip-" + version + "-setup.exe"}}}
	for k, v := range extra {
		m[k] = v
	}
	raw, _ := json.Marshal(m)
	env, err := SignManifest(key, raw)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func mirrorClient(net fakeNet, pub ed25519.PublicKey, mirrors ...string) *Client {
	return &Client{HTTP: &http.Client{Transport: net}, ManifestURL: ghManifest, Current: "1.0.11", Runtime: "win-x64",
		PublicKey: pub, Mirrors: mirrors, MirrorGrace: 300 * time.Millisecond,
		ManifestPause: func(int) time.Duration { return time.Millisecond }, DownloadPause: func(int) time.Duration { return time.Millisecond }}
}

// The routes are raced: a proxy that answers first with an older manifest
// does not hide the newer one GitHub gives a moment later, a proxy's page of
// something else and a manifest signed by another key count for nothing, and
// the routes are ranked for the downloads.
func TestRaceTakesTheNewestSignedManifest(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	net := fakeNet{
		ghManifest:                         {delay: 120 * time.Millisecond, body: signedManifest(t, priv, "1.0.13", nil)},
		"https://old.test/" + ghManifest:   {delay: 5 * time.Millisecond, body: signedManifest(t, priv, "1.0.12", nil)},
		"https://page.test/" + ghManifest:  {delay: 1 * time.Millisecond, body: []byte("<html>not found</html>")},
		"https://evil.test/" + ghManifest:  {delay: 2 * time.Millisecond, body: signedManifest(t, other, "9.9.9", nil)},
		"https://plain.test/" + ghManifest: {delay: 3 * time.Millisecond, body: []byte(`{"appId":"maccy","latest":{"version":"8.0.0","installer":{"url":"https://x/y.exe"}}}`)},
	}
	c := mirrorClient(net, pub, "https://old.test/", "https://page.test/", "https://evil.test/", "https://plain.test/")
	r := c.Check(context.Background(), nil)
	if r.Status != UpdateAvailable || r.Info.Version != "1.0.13" {
		t.Fatalf("%+v %+v", r, r.Info)
	}
	got := c.routes()
	if got[0] != "" || got[1] != "https://old.test/" {
		t.Fatalf("routes %q: the one with the newest version first, then the other that answered", got)
	}
	for _, bad := range got[2:] {
		if bad == "" || bad == "https://old.test/" {
			t.Fatalf("routes %q", got)
		}
	}
}

func TestNoSignedManifestOnAnyRouteIsAFailure(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	net := fakeNet{
		ghManifest:                       {body: []byte(`{"appId":"maccy","latest":{"version":"9.0.0","installer":{"url":"https://x/y.exe"}}}`)},
		"https://evil.test/" + ghManifest: {body: signedManifest(t, other, "9.9.9", nil)},
	}
	r := mirrorClient(net, pub, "https://evil.test/").Check(context.Background(), nil)
	if r.Status != Failed {
		t.Fatalf("an unsigned or foreign manifest was taken: %+v %+v", r, r.Info)
	}
	// One route only (no proxies): the same.
	r = mirrorClient(fakeNet{ghManifest: net[ghManifest]}, pub).Check(context.Background(), nil)
	if r.Status != Failed {
		t.Fatalf("GitHub's own unsigned manifest was taken: %+v", r)
	}
}

// A download goes through the routes in their order and moves on when one
// delivers another file than the signed checksum says (or fails).
func TestDownloadMovesOnToTheNextRoute(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	good := []byte("the real package")
	sum := sha256.Sum256(good)
	pkg := "https://github.com/Achordchan/klip/releases/download/v1.0.13/Klip-1.0.13-win-x64.zip"
	net := fakeNet{
		ghManifest:                        {delay: 50 * time.Millisecond, body: signedManifest(t, priv, "1.0.13", nil)},
		"https://fast.test/" + ghManifest: {delay: 1 * time.Millisecond, body: signedManifest(t, priv, "1.0.13", nil)},
		"https://fast.test/" + pkg:        {body: []byte("something else, same size")[:len(good)]},
		"https://dead.test/" + ghManifest: {status: 502},
		pkg:                               {body: good},
	}
	c := mirrorClient(net, pub, "https://fast.test/", "https://dead.test/")
	if r := c.Check(context.Background(), nil); r.Status != UpdateAvailable {
		t.Fatalf("%+v", r)
	}
	if got := c.routes(); got[0] != "https://fast.test/" {
		t.Fatalf("routes %q", got)
	}
	target := filepath.Join(t.TempDir(), "p.zip")
	if err := c.download(context.Background(), pkg, target, hex.EncodeToString(sum[:]), int64(len(good)), nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); !bytes.Equal(b, good) {
		t.Fatalf("got %q", b)
	}
	// Every route wrong: no file.
	bad := fakeNet{pkg: {body: []byte("x")}}
	c2 := mirrorClient(bad, pub)
	target2 := filepath.Join(t.TempDir(), "p.zip")
	if err := c2.download(context.Background(), pkg, target2, hex.EncodeToString(sum[:]), int64(len(good)), nil); err == nil {
		t.Fatal("a file of another checksum was kept")
	}
	if _, err := os.Stat(target2); !os.IsNotExist(err) {
		t.Fatal("a wrong file was left")
	}
}

// The proxies a signed manifest names replace the built-in ones, now and
// in the next run; invalid entries are dropped, and a manifest that names
// none keeps what there was.
func TestMirrorsOfASignedManifestAreKept(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	file := filepath.Join(t.TempDir(), "mirrors.json")
	net := fakeNet{ghManifest: {body: signedManifest(t, priv, "1.0.13", map[string]any{
		"mirrors": []string{"https://new.test/", "http://plain.test/", "https://new.test/", "javascript:alert(1)", "https://noslash.test"}})}}
	c := mirrorClient(net, pub, "https://builtin.test/")
	c.MirrorsFile = file
	if r := c.Check(context.Background(), nil); r.Status != UpdateAvailable {
		t.Fatalf("%+v", r)
	}
	if got := strings.Join(c.mirrors(), ","); got != "https://new.test/" {
		t.Fatalf("mirrors %s", got)
	}
	next := mirrorClient(fakeNet{}, pub, "https://builtin.test/")
	next.MirrorsFile = file
	if got := strings.Join(next.mirrors(), ","); got != "https://new.test/" {
		t.Fatalf("the next run's mirrors %s", got)
	}
	// A manifest without a list leaves it.
	net2 := fakeNet{ghManifest: {body: signedManifest(t, priv, "1.0.14", nil)}, "https://new.test/" + ghManifest: {body: signedManifest(t, priv, "1.0.14", nil)}}
	c3 := mirrorClient(net2, pub, "https://builtin.test/")
	c3.MirrorsFile = file
	if r := c3.Check(context.Background(), nil); r.Status != UpdateAvailable || strings.Join(c3.mirrors(), ",") != "https://new.test/" {
		t.Fatalf("%+v %v", r, c3.mirrors())
	}
}

func TestOnlyGitHubAddressesGoThroughAProxy(t *testing.T) {
	for _, tc := range []struct {
		prefix, url string
		ok          bool
	}{
		{"", "http://127.0.0.1:8765/manifest.json", true},
		{"https://m.test/", "https://github.com/a/b/releases/download/v1/x.zip", true},
		{"https://m.test/", "https://GitHub.com/a/b", true},
		{"https://m.test/", "http://github.com/a/b", false},
		{"https://m.test/", "https://github.com.evil.test/a", false},
		{"https://m.test/", "https://objects.githubusercontent.com/x", false},
		{"https://m.test/", "http://127.0.0.1:8765/manifest.json", false},
	} {
		if _, ok := viaRoute(tc.prefix, tc.url); ok != tc.ok {
			t.Errorf("%q + %q: %v", tc.prefix, tc.url, ok)
		}
	}
	for m, ok := range map[string]bool{"https://ghfast.top/": true, "https://x.test/p/": true, "http://x.test/": false,
		"https://x.test": false, "https://x.test/?a=1": false, "https://u:p@x.test/": false, "https://x.test/ a/": false} {
		if validMirror(m) != ok {
			t.Errorf("validMirror(%q) = %v", m, !ok)
		}
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	raw := []byte(`{"appId":"maccy","latest":{"version":"1.0.13","installer":{"url":"https://x/y.exe"}}}`)
	env, err := SignManifest(priv, raw)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenEnvelope(pub, env)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("%v %q", err, got)
	}
	// One byte of the manifest changed, its signature kept: refused.
	var e Envelope
	_ = json.Unmarshal(env, &e)
	e.Manifest = strings.Replace(e.Manifest, e.Manifest[10:11], string(rune(e.Manifest[10]^1)), 1)
	tampered, _ := json.Marshal(e)
	if _, err := OpenEnvelope(pub, tampered); err == nil {
		t.Fatal("a changed manifest passed")
	}
	if _, err := ParsePublicKey(ReleaseKey); err != nil {
		t.Fatalf("the release key built in: %v", err)
	}
}
