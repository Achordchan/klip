package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
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
	manifest, err := retry(ctx, 3, c.manifestPause, func(attempt int) (*Manifest, error) {
		if attempt > 1 {
			say(fmt.Sprintf("更新服务器响应较慢，正在第 %d 次重试...", attempt))
		}
		actx, cancel := context.WithTimeout(ctx, c.manifestTimeout())
		defer cancel()
		req, err := http.NewRequestWithContext(actx, http.MethodGet, c.ManifestURL, nil)
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
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			if actx.Err() != nil && ctx.Err() == nil {
				return nil, errTimeout
			}
			return nil, err
		}
		return ParseManifest(body)
	}, func(err error) bool { return transient(err) || errors.Is(err, errTimeout) })
	if err != nil {
		if ctx.Err() != nil {
			return Result{Status: Failed, Error: ctx.Err().Error()}
		}
		return Result{Status: Failed, Error: err.Error()}
	}
	return c.decide(manifest)
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
