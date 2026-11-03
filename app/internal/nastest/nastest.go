// Package nastest starts the real nas-agent-py for tests, on a free port and
// with a temporary data folder, and drives its admin routes. It needs a
// Python with the server's requirements: point KLIP_NAS_PYTHON at it, for
// example
//
//	python3 -m venv /tmp/nasvenv && /tmp/nasvenv/bin/pip install -r nas-agent-py/requirements.txt
//	KLIP_NAS_PYTHON=/tmp/nasvenv/bin/python go test ./...
//
// Without it Start skips the test.
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
	"testing"
	"time"

	"klip/internal/syncclient"
)

// AdminKey is the ADMIN_KEY the test server runs with.
const AdminKey = "test-admin-key"

// Server is a running nas-agent-py.
type Server struct {
	URL string
	t   *testing.T
}

// Start runs the server for the test and stops it when the test ends.
func Start(t *testing.T) *Server {
	t.Helper()
	py := os.Getenv("KLIP_NAS_PYTHON")
	if py == "" {
		t.Skip("KLIP_NAS_PYTHON is not set: the real server is not run")
	}
	dir, err := serverDir()
	if err != nil {
		t.Skipf("server source not available: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	cmd := exec.Command(py, "-m", "uvicorn", "main:app", "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--log-level", "warning")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"STORAGE_ROOT="+t.TempDir(),
		"ADMIN_KEY="+AdminKey,
		"AUTH_SIGNING_KEY=test-signing-key-that-is-long-enough-for-hs256",
		"SMTP_HOST=", // email is not configured: password codes give 503
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			t.Logf("server output:\n%s", out.String())
		}
	})

	s := &Server{URL: fmt.Sprintf("http://127.0.0.1:%d", port), t: t}
	c := syncclient.New(s.URL)
	deadline := time.Now().Add(30 * time.Second)
	for !c.Health(context.Background()) {
		if time.Now().After(deadline) {
			t.Fatalf("the server did not start:\n%s", out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	return s
}

// serverDir finds nas-agent-py by walking up from the working directory.
func serverDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, "nas-agent-py")
		if _, err := os.Stat(filepath.Join(p, "app.py")); err == nil {
			return p, nil
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("nas-agent-py not found above %s", wd)
		}
	}
}

// Admin posts to an /admin route with the legacy key and returns the JSON.
func (s *Server) Admin(path string, payload any) map[string]any {
	s.t.Helper()
	b, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, s.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+AdminKey)
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
