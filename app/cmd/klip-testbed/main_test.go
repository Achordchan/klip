package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHandlerServesTheAddressesOfTheClients(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"appId":"maccy"}`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "maccy-1.0.11-win-x64.zip"), []byte("ZIP"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("no"), 0o644)
	srv := httptest.NewServer(Handler(dir, func(string, ...any) {}))
	defer srv.Close()
	get := func(p string) (int, string) {
		r, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	// The address 1.0.10 has built in.
	if c, b := get("/Achordchan/maccy/raw/master/docs/updates/manifest.json"); c != 200 || b != `{"appId":"maccy"}` {
		t.Errorf("manifest: %d %q", c, b)
	}
	if c, b := get("/Achordchan/maccy/releases/download/v1.0.11/maccy-1.0.11-win-x64.zip"); c != 200 || b != "ZIP" {
		t.Errorf("zip: %d %q", c, b)
	}
	for _, p := range []string{"/secret.txt", "/Achordchan/maccy/releases/download/v1.0.11/missing.zip", "/releases/download/../secret.txt", "/"} {
		if c, _ := get(p); c != 404 && c != 400 {
			t.Errorf("%s: %d, want a refusal", p, c)
		}
	}
}

func TestCertificateIsForGiteeAndIsKept(t *testing.T) {
	dir := t.TempDir()
	c1, cer, err := LoadOrCreateCert(dir, "gitee.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cer); err != nil {
		t.Fatalf("no .cer for Windows: %v", err)
	}
	c2, _, err := LoadOrCreateCert(dir, "gitee.com")
	if err != nil || string(c1.Certificate[0]) != string(c2.Certificate[0]) {
		t.Fatalf("the certificate was made again: %v", err)
	}
	// A client that trusts the certificate and asks for gitee.com is served.
	leaf, _ := x509.ParseCertificate(c1.Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "gitee.com"}); err != nil {
		t.Fatalf("not valid for gitee.com: %v", err)
	}
	srv := httptest.NewUnstartedServer(Handler(t.TempDir(), func(string, ...any) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{c1}}
	srv.StartTLS()
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "gitee.com"}}}
	r, err := client.Get(srv.URL + "/x/manifest.json")
	if err != nil {
		t.Fatalf("TLS: %v", err)
	}
	r.Body.Close()
}

func TestKeysAndCertificatesAreNeverServed(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"key.pem", "cert.pem", "gitee.com.cer", "manifest.json", "klip.zip"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := Handler(dir, func(string, ...any) {})
	for name, want := range map[string]int{"key.pem": 404, "cert.pem": 404, "gitee.com.cer": 404, "klip.zip": 200} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/releases/download/"+name, nil))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", name, rec.Code, want)
		}
	}
}

func TestExpiredCertificateIsMadeAgain(t *testing.T) {
	dir := t.TempDir()
	first, _, err := LoadOrCreateCert(dir, "gitee.com")
	if err != nil {
		t.Fatal(err)
	}
	// Age it: certificate and key replaced by ones that ran out yesterday.
	old := expiredPair(t, dir)
	again, _, err := LoadOrCreateCert(dir, "gitee.com")
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(again.Certificate[0])
	if !leaf.NotAfter.After(time.Now()) || string(again.Certificate[0]) == string(old) || string(first.Certificate[0]) == string(again.Certificate[0]) {
		t.Fatalf("an expired certificate was reused: ends %v", leaf.NotAfter)
	}
}

// expiredPair writes a certificate that ended yesterday over the kept one.
func expiredPair(t *testing.T, dir string) []byte {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gitee.com"},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	_ = os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return der
}

func TestCertificateMaySignOnlyForTheTestedHost(t *testing.T) {
	c, _, err := LoadOrCreateCert(t.TempDir(), "gitee.com")
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(c.Certificate[0])
	if len(leaf.PermittedDNSDomains) != 1 || leaf.PermittedDNSDomains[0] != "gitee.com" || !leaf.PermittedDNSDomainsCritical {
		t.Fatalf("a trusted root with no limit on the names it may sign for: %v", leaf.PermittedDNSDomains)
	}
}
