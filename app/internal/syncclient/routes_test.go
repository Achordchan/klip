package syncclient

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every route the client calls is a route of nas-agent-py, with the method
// the client uses. The check reads app.py, so a route renamed on either side
// fails here without a server.
func TestRoutesExistInServer(t *testing.T) {
	b, err := os.ReadFile("../../../nas-agent-py/app.py")
	if err != nil {
		t.Skipf("server source not available: %v", err)
	}
	re := regexp.MustCompile(`@app\.(get|post|put|delete)\("([^"]+)"\)`)
	have := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		have[strings.ToUpper(m[1])+" "+m[2]] = true
	}

	want := []string{
		"GET /health",
		"POST /auth/register", "POST /auth/login", "POST /auth/refresh", "GET /auth/me",
		"POST /auth/password/code", "POST /auth/password/reset", "POST /auth/password/change",
		"GET /subscription/status", "POST /card/redeem",
		"GET /sync/manifest", "GET /sync/snapshot", "PUT /sync/snapshot", "GET /sync/events",
		"POST /sync/blobs/check", "PUT /sync/blobs/{blob_sha256}", "GET /sync/blobs/{blob_sha256}",
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("the server has no route %s", w)
		}
	}

	// And the other way: every sync, auth and account route of the server is
	// one the client knows, so that a new route is noticed.
	known := map[string]bool{}
	for _, w := range want {
		known[w] = true
	}
	for r := range have {
		path := r[strings.Index(r, " ")+1:]
		if (strings.HasPrefix(path, "/sync/") || strings.HasPrefix(path, "/auth/") || strings.HasPrefix(path, "/subscription/") ||
			strings.HasPrefix(path, "/card/")) && !known[r] {
			t.Errorf("the server has %s, which the client does not use", r)
		}
	}
}

// The sync server's event names and the one the client reacts to.
func TestServerEventNames(t *testing.T) {
	b, err := os.ReadFile("../../../nas-agent-py/app.py")
	if err != nil {
		t.Skipf("server source not available: %v", err)
	}
	if !strings.Contains(string(b), `"sync-updated"`) || !strings.Contains(string(b), `"connected"`) {
		t.Error("the server no longer publishes sync-updated / connected")
	}
}
