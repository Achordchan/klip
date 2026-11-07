package update

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// CurrentRuntime is the runtime id of this program as the manifest names it:
// the ids of .NET, which 1.0.10 and older use.
func CurrentRuntime() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/amd64":
		return "win-x64"
	case "windows/arm64":
		return "win-arm64"
	case "windows/386":
		return "win-x86"
	case "linux/amd64":
		return "linux-x64"
	case "linux/arm64":
		return "linux-arm64"
	case "darwin/amd64":
		return "osx-x64"
	case "darwin/arm64":
		return "osx-arm64"
	}
	return runtime.GOOS + "-" + runtime.GOARCH
}

// Info is a version that is newer than the running one.
type Info struct {
	Version   string
	Mandatory bool
	Notes     string

	InstallerURL    string
	InstallerSHA256 string
	InstallerSize   int64

	PackageKind    string
	PackageRuntime string
	PackageURL     string
	PackageSHA256  string
	PackageSize    int64
}

// HasSupportedPackage says whether the light update fits this program: a zip
// for this runtime with an address and a checksum.
func (i Info) HasSupportedPackage(rt string) bool {
	return strings.EqualFold(i.PackageKind, "zip") && strings.EqualFold(i.PackageRuntime, rt) &&
		strings.TrimSpace(i.PackageURL) != "" && strings.TrimSpace(i.PackageSHA256) != ""
}

// Status is the outcome of a check.
type Status int

const (
	UpToDate Status = iota
	UpdateAvailable
	Failed
)

// Result is what Check found.
type Result struct {
	Status Status
	Info   *Info
	Error  string
}

// Client is UpdateService's network side.
type Client struct {
	HTTP        *http.Client
	ManifestURL string
	// Current is the running version ("1.0.11").
	Current string
	// Runtime is CurrentRuntime unless a test sets it.
	Runtime string

	// PublicKey, when set, is the release key: ManifestURL is then an
	// Envelope (manifest.signed.json), and a manifest whose signature it does
	// not accept is no manifest, whichever route brought it. The program
	// always sets it; without it the manifest is read as it is (tests).
	PublicKey ed25519.PublicKey
	// Mirrors are the GitHub proxies tried beside GitHub (DefaultMirrors in
	// the program), for an address on github.com only; MirrorsFile keeps the
	// ones a signed manifest named instead.
	Mirrors     []string
	MirrorsFile string
	// MirrorGrace is how long a check waits, once one route has answered,
	// for the others (1.5 s unless set): the newest version among them wins.
	MirrorGrace time.Duration

	mu      sync.Mutex
	learned []string // the mirrors a signed manifest named
	order   []string // the routes, fastest first, as the last check found them

	// Pauses, which tests shorten.
	ManifestTimeout time.Duration // 8 s for each try
	ManifestPause   func(attempt int) time.Duration
	DownloadPause   func(attempt int) time.Duration
	// DownloadIdle is how long a download may be silent before it is given
	// up (60 s unless set).
	DownloadIdle time.Duration
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

// defaultHTTP waits for the answer to a request for at most 30 s; how long a
// transfer may take is limited by the callers (a manifest try has its own
// timeout, a download an idle timer).
var defaultHTTP = func() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{Transport: t}
}()

func (c *Client) runtime() string {
	if c.Runtime != "" {
		return c.Runtime
	}
	return CurrentRuntime()
}

func (c *Client) manifestTimeout() time.Duration {
	if c.ManifestTimeout > 0 {
		return c.ManifestTimeout
	}
	return 8 * time.Second
}

func (c *Client) manifestPause(attempt int) time.Duration {
	if c.ManifestPause != nil {
		return c.ManifestPause(attempt)
	}
	return time.Duration(700*attempt) * time.Millisecond
}

func (c *Client) downloadPause(attempt int) time.Duration {
	if c.DownloadPause != nil {
		return c.DownloadPause(attempt)
	}
	return time.Duration(attempt) * time.Second
}

// httpError is HttpRequestException's: a refused status.
type httpError struct{ code int }

func (e *httpError) Error() string {
	return fmt.Sprintf("Response status code does not indicate success: %d (%s).", e.code, http.StatusText(e.code))
}

// transient is IsTransientHttpFailure: the statuses that are worth a second
// try, and any failure without a status (the connection).
func transient(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		switch he.code {
		case 408, 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	return true
}

// retry runs op up to attempts times, pausing between tries, as long as
// again says the error is worth it.
func retry[T any](ctx context.Context, attempts int, pause func(int) time.Duration, op func(attempt int) (T, error), again func(error) bool) (T, error) {
	var zero T
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		v, err := op(attempt)
		if err == nil {
			return v, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		last = err
		if attempt < attempts && again(err) {
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(pause(attempt)):
			}
			continue
		}
		break
	}
	return zero, last
}

// Check fetches the manifest and says whether a newer version is there, with
// the words of the C# client for each failure. status, when set, receives
// the progress texts of a manual check.
func (c *Client) Check(ctx context.Context, status func(string)) Result {
	if strings.TrimSpace(c.ManifestURL) == "" {
		return Result{Status: Failed, Error: "manifest url is empty"}
	}
	say := func(s string) {
		if status != nil {
			status(s)
		}
	}
	say("正在检查是否有新版本...")
	addrs := c.addresses(c.ManifestURL)
	var manifest *Manifest
	var err error
	if len(addrs) <= 1 {
		manifest, err = retry(ctx, 3, c.manifestPause, func(attempt int) (*Manifest, error) {
			if attempt > 1 {
				say(fmt.Sprintf("更新服务器响应较慢，正在第 %d 次重试...", attempt))
			}
			return c.fetchManifest(ctx, c.ManifestURL)
		}, func(err error) bool { return transient(err) || errors.Is(err, errTimeout) })
	} else {
		manifest, err = retry(ctx, 2, c.manifestPause, func(attempt int) (*Manifest, error) {
			if attempt > 1 {
				say(fmt.Sprintf("更新服务器响应较慢，正在第 %d 次重试...", attempt))
			}
			return c.race(ctx)
		}, func(error) bool { return true })
	}
	if err != nil {
		if ctx.Err() != nil {
			return Result{Status: Failed, Error: ctx.Err().Error()}
		}
		return Result{Status: Failed, Error: err.Error()}
	}
	if c.PublicKey != nil {
		// Only a signed manifest, and only the one taken, names the mirrors.
		c.learnMirrors(manifest.Mirrors)
	}
	return c.decide(manifest)
}

// fetchManifest is one try of one address: the manifest, its signature
// checked when there is a key.
func (c *Client) fetchManifest(ctx context.Context, addr string) (*Manifest, error) {
	actx, cancel := context.WithTimeout(ctx, c.manifestTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(actx, http.MethodGet, addr, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		if actx.Err() != nil && ctx.Err() == nil {
			return nil, errTimeout
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &httpError{resp.StatusCode}
	}
	// A manifest is a few kilobytes; a proxy's page of something else may be
	// anything.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if actx.Err() != nil && ctx.Err() == nil {
			return nil, errTimeout
		}
		return nil, err
	}
	if c.PublicKey != nil {
		if body, err = OpenEnvelope(c.PublicKey, body); err != nil {
			return nil, err
		}
	}
	return ParseManifest(body)
}

// race asks every route at once. Once one has answered with a manifest the
// others have MirrorGrace to answer too, and the newest version among them is
// taken: a proxy that serves an old manifest (it may cache, or hold back)
// does not hide a release that another route shows. The routes are kept in
// the order they answered in, for the downloads; the ones that failed come
// last.
func (c *Client) race(ctx context.Context) (*Manifest, error) {
	routes := c.routes()
	type answer struct {
		route string
		m     *Manifest
		err   error
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan answer, len(routes))
	n := 0
	for _, r := range routes {
		addr, ok := viaRoute(r, c.ManifestURL)
		if !ok {
			continue
		}
		n++
		go func() {
			m, err := c.fetchManifest(rctx, addr)
			ch <- answer{r, m, err}
		}()
	}
	grace := c.MirrorGrace
	if grace <= 0 {
		grace = 1500 * time.Millisecond
	}
	var good []answer
	var failed []string
	var last error
	var deadline <-chan time.Time
wait:
	for i := 0; i < n; i++ {
		var a answer
		select {
		case a = <-ch:
		case <-deadline:
			break wait
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if a.err != nil {
			failed = append(failed, a.route)
			last = a.err
			continue
		}
		good = append(good, a)
		if deadline == nil {
			deadline = time.After(grace)
		}
	}
	if len(good) == 0 {
		if last == nil {
			last = errors.New("no route to the update server")
		}
		return nil, last
	}
	best := good[0]
	bv, _ := ParseVersion(NormalizeVersion(best.m.Latest.Version))
	for _, a := range good[1:] {
		if v, ok := ParseVersion(NormalizeVersion(a.m.Latest.Version)); ok && v.Compare(bv) > 0 {
			best, bv = a, v
		}
	}
	// Fastest first among the routes that had the newest version, then the
	// ones with an older one, then the ones that failed or did not answer.
	order := make([]string, 0, len(routes))
	sort.SliceStable(good, func(i, j int) bool {
		return good[i].m.Latest.Version == best.m.Latest.Version && good[j].m.Latest.Version != best.m.Latest.Version
	})
	for _, a := range good {
		order = append(order, a.route)
	}
	order = append(order, failed...)
	c.mu.Lock()
	c.order = order
	c.mu.Unlock()
	return best.m, nil
}

var errTimeout = errors.New("检查更新超时")

func (c *Client) decide(m *Manifest) Result {
	fail := func(s string) Result { return Result{Status: Failed, Error: s} }
	if m == nil || !strings.EqualFold(m.AppID, AppID) {
		return fail("invalid manifest")
	}
	latest := NormalizeVersion(m.Latest.Version)
	if latest == "" {
		return fail("manifest missing version")
	}
	cur := NormalizeVersion(c.Current)
	lv, ok1 := ParseVersion(latest)
	cv, ok2 := ParseVersion(cur)
	if !ok1 || !ok2 {
		return fail("version parse failed (latest=" + latest + ", current=" + cur + ")")
	}
	if lv.Compare(cv) <= 0 {
		return Result{Status: UpToDate}
	}
	pkg := m.Latest.Package
	for i := range m.Latest.Packages {
		if p := &m.Latest.Packages[i]; strings.EqualFold(strings.TrimSpace(p.Runtime), c.runtime()) && strings.EqualFold(strings.TrimSpace(p.Kind), "zip") {
			pkg = p
			break
		}
	}
	if strings.TrimSpace(m.Latest.Installer.URL) == "" && (pkg == nil || strings.TrimSpace(pkg.URL) == "") {
		return fail("manifest missing update url")
	}
	info := &Info{
		Version:         latest,
		Mandatory:       m.Latest.Mandatory,
		Notes:           m.Latest.Notes,
		InstallerURL:    m.Latest.Installer.URL,
		InstallerSHA256: strings.TrimSpace(m.Latest.Installer.SHA256),
		InstallerSize:   m.Latest.Installer.Size,
	}
	if pkg != nil {
		info.PackageKind = strings.TrimSpace(pkg.Kind)
		info.PackageRuntime = strings.TrimSpace(pkg.Runtime)
		info.PackageURL = strings.TrimSpace(pkg.URL)
		info.PackageSHA256 = strings.TrimSpace(pkg.SHA256)
		info.PackageSize = pkg.Size
	}
	return Result{Status: UpdateAvailable, Info: info}
}
