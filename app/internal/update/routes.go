package update

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultMirrors are the GitHub proxies tried beside GitHub itself, for the
// users who reach GitHub slowly or not at all (in China): the address of a
// GitHub file put after the prefix, "https://ghfast.top/https://github.com/…".
// These three answered with the right files when they were chosen
// (2026-11-08); a signed manifest can name others (Manifest.Mirrors), which
// are then kept.
//
// A proxy is run by somebody else and can serve anything: that is why what
// comes through it is trusted only when signed (the manifest, see Envelope)
// or of the checksum a signed manifest gives (the packages).
var DefaultMirrors = []string{
	"https://ghfast.top/",
	"https://gh-proxy.com/",
	"https://ghproxy.net/",
}

// validMirror is a prefix the program may put before a GitHub address: an
// https address of a host, with a path that ends in a slash, nothing else.
func validMirror(m string) bool {
	if len(m) > 200 || strings.ContainsAny(m, " \t\r\n\"'<>\\") {
		return false
	}
	u, err := url.Parse(m)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil &&
		u.RawQuery == "" && u.Fragment == "" && strings.HasSuffix(u.Path, "/")
}

// viaRoute is the address of u through the route prefix ("" is GitHub
// itself). Only an address on github.com goes through a proxy: anything else
// (a test server, a download on another host) is fetched as it is or not by
// a proxy at all.
func viaRoute(prefix, u string) (string, bool) {
	if prefix == "" {
		return u, true
	}
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || !strings.EqualFold(p.Host, "github.com") {
		return "", false
	}
	return prefix + u, true
}

// mirrors are the proxies to try: the ones a signed manifest named last
// (kept in MirrorsFile), else Mirrors.
func (c *Client) mirrors() []string {
	c.mu.Lock()
	learned := c.learned
	c.mu.Unlock()
	if learned != nil {
		return learned
	}
	if c.MirrorsFile != "" {
		if b, err := os.ReadFile(c.MirrorsFile); err == nil {
			var list []string
			if json.Unmarshal(b, &list) == nil {
				if list = cleanMirrors(list); len(list) > 0 {
					c.mu.Lock()
					c.learned = list
					c.mu.Unlock()
					return list
				}
			}
		}
	}
	return cleanMirrors(c.Mirrors)
}

// cleanMirrors keeps the valid prefixes, each once, in their order.
func cleanMirrors(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range list {
		m = strings.TrimSpace(m)
		if validMirror(m) && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// learnMirrors keeps the proxies a signed manifest names, for this run and
// the next ones. A manifest that names none leaves the list as it is.
func (c *Client) learnMirrors(list []string) {
	list = cleanMirrors(list)
	if len(list) == 0 {
		return
	}
	c.mu.Lock()
	c.learned = list
	c.mu.Unlock()
	if c.MirrorsFile != "" {
		if b, err := json.Marshal(list); err == nil {
			_ = os.MkdirAll(filepath.Dir(c.MirrorsFile), 0o755)
			_ = writeFileAtomic(c.MirrorsFile, b)
		}
	}
}

// routes are GitHub itself and the proxies, in the order of the last check
// (fastest first), else GitHub first.
func (c *Client) routes() []string {
	all := append([]string{""}, c.mirrors()...)
	c.mu.Lock()
	order := c.order
	c.mu.Unlock()
	if len(order) == 0 {
		return all
	}
	known := map[string]bool{}
	for _, r := range all {
		known[r] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, r := range order {
		if known[r] && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, r := range all {
		if !seen[r] {
			out = append(out, r)
		}
	}
	return out
}

// addresses are u through every route that can carry it, best first.
func (c *Client) addresses(u string) []string {
	var out []string
	for _, r := range c.routes() {
		if a, ok := viaRoute(r, u); ok {
			out = append(out, a)
		}
	}
	return out
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
