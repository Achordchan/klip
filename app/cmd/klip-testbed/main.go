// Command klip-testbed serves a release to a client that believes it talks to
// Gitee, so that an installed 1.0.10 can be made to update itself to a
// release that has not been published. 1.0.10 has the address of its
// manifest built in (https://gitee.com/...), so the testbed answers on that
// name: put "127.0.0.1 gitee.com" in the hosts file, trust the certificate it
// writes, and run it as administrator (it listens on port 443).
//
//	klip-testbed -dir artifacts/testbed
//
// The folder holds manifest.json and the files it names (the installer and
// the zip). See docs/upgrade-verification-1.0.11.md.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func main() {
	dir := flag.String("dir", "artifacts/testbed", "folder with manifest.json and the files it names")
	addr := flag.String("addr", "127.0.0.1:443", "address to listen on (the machine itself; the hosts file points gitee.com here)")
	certDir := flag.String("cert-dir", "", "where to keep the certificate and its key (default: a private folder in the user profile)")
	plain := flag.Bool("http", false, "serve plain HTTP (for tests of the testbed itself)")
	flag.Parse()
	if *certDir == "" {
		// The key of a certificate that people are told to trust must not sit in
		// a folder others can read: the profile of the user is private, a
		// checkout often is not (and a Windows file takes the access rights of
		// its folder, whatever mode it is created with).
		if cfg, err := os.UserConfigDir(); err == nil {
			*certDir = filepath.Join(cfg, "klip-testbed")
		} else {
			*certDir = filepath.Join(*dir, "cert")
		}
	}

	h := Handler(*dir, log.Printf)
	if *plain {
		log.Printf("serving %s on http://%s", *dir, *addr)
		log.Fatal(http.ListenAndServe(*addr, h))
	}
	cert, cerPath, err := LoadOrCreateCert(*certDir, "gitee.com")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("certificate: %s\n", cerPath)
	fmt.Println(`trust it for the current user:   certutil -user -addstore Root "` + cerPath + `"`)
	fmt.Println(`and remove it when done:         certutil -user -delstore Root gitee.com`)
	fmt.Printf("serving %s on https://%s (hosts file: 127.0.0.1 gitee.com)\n", *dir, *addr)
	srv := &http.Server{Addr: *addr, Handler: h, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

// Handler answers the addresses that the clients use: the manifest wherever
// it is asked for, and a release file by its name below /releases/download/.
func Handler(dir string, logf func(string, ...any)) http.Handler {
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var file string
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifest.json"):
			file = "manifest.json"
		case strings.Contains(r.URL.Path, "/releases/download/"):
			file = path.Base(r.URL.Path)
		}
		if file == "" || strings.ContainsAny(file, `/\`) || file == "." || file == ".." || !servable(file) {
			logf("404 %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		full := filepath.Join(dir, file)
		if _, err := os.Stat(full); err != nil {
			logf("404 %s (no %s)", r.URL.Path, full)
			http.NotFound(w, r)
			return
		}
		logf("200 %s -> %s", r.URL.Path, full)
		root, err := os.OpenRoot(dir)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer root.Close()
		relative, err := filepath.Rel(dir, full)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		info, err := root.Lstat(relative)
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		openedFile, err := root.Open(relative)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer openedFile.Close()
		opened, err := openedFile.Stat()
		if err != nil || !os.SameFile(info, opened) {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, info.Name(), info.ModTime(), openedFile)
	})
}

// servable says whether a file may be handed out: the manifest and what a
// release is made of. Anything else is refused whatever folder it sits in, so
// that pointing -cert-dir at the served folder cannot give away the key of the
// certificate that people are told to trust.
func servable(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".json", ".zip", ".exe", ".msi", ".7z":
		return true
	}
	return false
}

// LoadOrCreateCert keeps a self-signed certificate for host in dir (cert.pem,
// key.pem, and host.cer for Windows to import).
func LoadOrCreateCert(dir, host string) (tls.Certificate, string, error) {
	certPath, keyPath, cerPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), filepath.Join(dir, host+".cer")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		// Kept only while it is good for another day: an old one would fail
		// every upgrade test and restarting would not mend it.
		if leaf, perr := x509.ParseCertificate(c.Certificate[0]); perr == nil && time.Now().Add(24*time.Hour).Before(leaf.NotAfter) {
			return c, cerPath, nil
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tls.Certificate{}, "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: host, Organization: []string{"Klip upgrade testbed"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(0, 0, 30),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, DNSNames: []string{host}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		// Trusted as a root, it may sign for this name only.
		PermittedDNSDomainsCritical: true, PermittedDNSDomains: []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(cerPath, der, 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	return c, cerPath, err
}
