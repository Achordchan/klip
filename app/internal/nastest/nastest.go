// Package nastest starts the real sync server (server/, a Cloudflare Worker)
// for tests: `wrangler dev` on a free port, with a temporary data folder for
// its local D1, R2 and Durable Objects, and drives its admin routes. It needs
// Node and the server's packages, and is opt-in:
//
//	(cd server && npm install)
//	KLIP_TEST_SERVER=1 go test ./...
//
// Without KLIP_TEST_SERVER Start skips the test.
//
// To run the same tests against a deployed server instead, set
// KLIP_TEST_SERVER_URL to its address and KLIP_TEST_ADMIN_KEY to its
// ADMIN_KEY. They create accounts there: use a test deployment, or remove
// them afterwards.
package nastest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"klip/internal/syncclient"
)

// AdminKey is the ADMIN_KEY the test server runs with.
const AdminKey = "test-admin-key"

// Server is a running sync server.
type Server struct {
	URL      string
	adminKey string
	t        *testing.T
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// Start runs the server for the test and stops it when the test ends.
func Start(t *testing.T) *Server {
	t.Helper()
	if url := os.Getenv("KLIP_TEST_SERVER_URL"); url != "" {
		key := os.Getenv("KLIP_TEST_ADMIN_KEY")
		if key == "" {
			t.Fatal("KLIP_TEST_SERVER_URL needs KLIP_TEST_ADMIN_KEY")
		}
		return &Server{URL: strings.TrimRight(url, "/"), adminKey: key, t: t}
	}
	if os.Getenv("KLIP_TEST_SERVER") == "" {
		t.Skip("KLIP_TEST_SERVER is not set: the real server is not run")
	}
	dir, err := serverDir()
	if err != nil {
		t.Skipf("server source not available: %v", err)
	}
	wrangler := filepath.Join(dir, "node_modules", ".bin", "wrangler")
	if runtime.GOOS == "windows" {
		wrangler += ".cmd"
	}
	if _, err := os.Stat(wrangler); err != nil {
		t.Skipf("run npm install in %s first: %v", dir, err)
	}

	state := t.TempDir()
	env := append(os.Environ(), "WRANGLER_SEND_METRICS=false", "CI=1")
	var out bytes.Buffer
	migrate := exec.Command(wrangler, "d1", "migrations", "apply", "DB", "--local", "--persist-to", state)
	migrate.Dir, migrate.Env, migrate.Stdout, migrate.Stderr = dir, env, &out, &out
	if err := migrate.Run(); err != nil {
		t.Fatalf("d1 migrations: %v\n%s", err, out.String())
	}
	out.Reset()

	port := freePort(t)
	cmd := exec.Command(wrangler, "dev", "--local",
		"--ip", "127.0.0.1", "--port", fmt.Sprint(port), "--inspector-port", fmt.Sprint(freePort(t)),
		"--persist-to", state, "--show-interactive-dev-session=false", "--log-level", "warn",
		"--var", "ADMIN_KEY:"+AdminKey,
		"--var", "AUTH_SIGNING_KEY:test-signing-key-that-is-long-enough-for-hs256",
		"--var", "SMTP_HOST:", // email is not configured: password codes give 503
	)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdout, cmd.Stderr = &out, &out
	ownProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		killProcessGroup(cmd)
		cmd.Wait()
		if t.Failed() {
			t.Logf("server output:\n%s", out.String())
		}
	})

	s := &Server{URL: fmt.Sprintf("http://127.0.0.1:%d", port), adminKey: AdminKey, t: t}
	c := syncclient.New(s.URL)
	deadline := time.Now().Add(60 * time.Second)
	for !c.Health(context.Background()) {
		if time.Now().After(deadline) {
			t.Fatalf("the server did not start:\n%s", out.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	return s
}

// serverDir finds server/ by walking up from the working directory.
func serverDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, "server")
		if _, err := os.Stat(filepath.Join(p, "wrangler.jsonc")); err == nil {
			return p, nil
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("server/ not found above %s", wd)
		}
	}
}

// Admin posts to an /admin route with the legacy key and returns the JSON.
func (s *Server) Admin(path string, payload any) map[string]any {
	s.t.Helper()
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, s.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+s.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		s.t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
	}
	var m map[string]any
	json.Unmarshal(body, &m)
	return m
}

// Subscribe registers an account and gives it a month on the Mini tier. It
// returns its tokens.
func (s *Server) Subscribe(ctx context.Context, email, password string) syncclient.TokenSet {
	s.t.Helper()
	c := syncclient.New(s.URL)
	tok, err := c.Register(ctx, email, password)
	if err != nil {
		s.t.Fatal(err)
	}
	s.Admin("/admin/user/upgrade", map[string]any{"emailOrUserId": email, "durationDays": 30, "tier": "Mini"})
	return tok
}
