// Package syncclient talks to the sync server (nas-agent-py): accounts,
// subscription, the snapshot and its blobs, and the event stream that tells
// a device that another one has uploaded. It does what NasAgentClient,
// AuthService and SyncEventStreamService do in the C# client, with the same
// requests, timeouts and error texts, so that a Klip 1.0.11 device and a
// 1.0.10 one are the same to the server.
package syncclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"klip/internal/model"
)

// Timeouts of the C# client.
const (
	healthTimeout   = 8 * time.Second
	statusTimeout   = 20 * time.Second
	manifestTimeout = 30 * time.Second
	redeemTimeout   = 10 * time.Second
	blobCheckTimout = 30 * time.Second
	transferTimeout = 5 * time.Minute
	getRetryCount   = 1
	retryDelay      = 350 * time.Millisecond
)

// Client is a connection to one server. The zero value is not usable: use
// New.
type Client struct {
	baseURL string
	http    *http.Client
	// accountTimeout bounds the account calls. It is a field so that a test
	// can shorten it; New sets accountCallTimeout.
	accountTimeout time.Duration
}

// accountCallTimeout is the deadline of an account call. The HTTP client has
// no timeout of its own, and a refresh that never answers would hold the
// sync gate.
const accountCallTimeout = 30 * time.Second

// New returns a client for the server at baseURL, without a trailing slash.
// An empty address is the official server only for the account calls, as in
// C#; the others report ErrNoBaseURL.
func New(baseURL string) *Client {
	return NewWithHTTP(baseURL, nil)
}

// NewWithHTTP is New with the HTTP client to use. Its Timeout must be zero:
// every call sets its own.
func NewWithHTTP(baseURL string, h *http.Client) *Client {
	if h == nil {
		h = &http.Client{}
	}
	return &Client{
		baseURL:        strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:           h,
		accountTimeout: accountCallTimeout,
	}
}

// AccountID is the account an access token belongs to: the "sub" claim of
// the JWT, or its email when there is no subject. The signature is not
// verified: the value is used only to tell two sessions apart, never to trust
// the token. An opaque token gives "".
func AccountID(accessToken string) string {
	parts := strings.Split(strings.TrimSpace(accessToken), ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	if claims.Sub != "" {
		return claims.Sub
	}
	return claims.Email
}

// ErrNoBaseURL is returned when the client has no server address.
var ErrNoBaseURL = errors.New("missing base url")

// ErrNoToken is returned when a call that needs an access token has none.
var ErrNoToken = errors.New("missing access token")

// APIError is a failed request. Message says what was asked ("get manifest
// failed"); Body is what the server answered, with the WWW-Authenticate and
// X-Auth-Failure headers in front where the C# client adds them.
type APIError struct {
	StatusCode int
	Message    string
	Body       string
	// Code is the "code" of the JSON body ("subscription_expired",
	// "snapshot_too_large", ...), when it has one.
	Code string
	// Plain says that Message is already the text for the user (the "error"
	// of an account call), so Error returns just it, as the C# exception does.
	Plain bool
}

func (e *APIError) Error() string {
	if e.Plain {
		return e.Message
	}
	if e.Body == "" {
		return fmt.Sprintf("%s (%d)", e.Message, e.StatusCode)
	}
	return fmt.Sprintf("%s (%d): %s", e.Message, e.StatusCode, e.Body)
}

// IsStatus says whether err is an APIError with this status code.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode == status
}

func newAPIError(resp *http.Response, message string, authHeaders bool) *APIError {
	body := readBodySafe(resp)
	if authHeaders {
		www := strings.Join(resp.Header.Values("WWW-Authenticate"), " ")
		fail := strings.Join(resp.Header.Values("X-Auth-Failure"), " ")
		body = combine(combine(www, fail), body)
	}
	return &APIError{StatusCode: resp.StatusCode, Message: message, Body: body, Code: errorCode(body)}
}

func combine(a, b string) string {
	switch {
	case strings.TrimSpace(a) == "":
		if strings.TrimSpace(b) == "" {
			return ""
		}
		return b
	case strings.TrimSpace(b) == "":
		return a
	}
	return a + "\n" + b
}

// errorCode reads "code" from a JSON body, as NasAgentApiException does.
func errorCode(body string) string {
	i := strings.Index(body, "{")
	if i < 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(body[i:]), &m) != nil {
		return ""
	}
	return scalarString(m["code"])
}

func readBodySafe(resp *http.Response) string {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	return string(b)
}

// scalarString reads a JSON string or number as text, and anything else as
// "": the C# TryReadString.
func scalarString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	if string(raw) == "true" || string(raw) == "false" {
		return string(raw)
	}
	return ""
}

func scalarInt64(raw json.RawMessage) *int64 {
	if strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return &n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return &v
		}
	}
	return nil
}

func scalarBool(raw json.RawMessage) *bool {
	if strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return &b
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if v, err := strconv.ParseBool(s); err == nil {
			return &v
		}
	}
	return nil
}

func nullableString(raw json.RawMessage) *string {
	var s *string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return nil
}

// decodeObject reads a JSON object; anything else is an empty one, as
// ParseJsonOrEmpty does.
// readObject reads the JSON object of a successful answer: a body cut short
// (the connection dropped, a timeout) or one that is not an object is an
// error, not an empty answer.
func readObject(resp *http.Response, what string) (map[string]json.RawMessage, error) {
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &m); err != nil || m == nil {
		return nil, fmt.Errorf("%s: malformed response", what)
	}
	return m, nil
}

func decodeObject(b []byte) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &m) != nil || m == nil {
		return map[string]json.RawMessage{}
	}
	return m
}

// newRequest builds a request to path with the bearer token, when there is
// one.
func (c *Client) newRequest(ctx context.Context, method, path, token string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if t := strings.TrimSpace(token); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	return req, nil
}

func (c *Client) needServer(token string, needToken bool) error {
	if c.baseURL == "" {
		return ErrNoBaseURL
	}
	if needToken && strings.TrimSpace(token) == "" {
		return ErrNoToken
	}
	return nil
}

// getWithRetry is SendGetWithRetryAsync: one more try after a timeout or a
// connection error, and a timeout reported as such.
func (c *Client) getWithRetry(ctx context.Context, path, token string, timeout time.Duration, retries int) (*http.Response, context.CancelFunc, error) {
	for attempt := 0; ; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		req, err := c.newRequest(cctx, http.MethodGet, path, token, nil)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		resp, err := c.http.Do(req)
		if err == nil {
			return resp, cancel, nil
		}
		cancel()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if attempt < retries {
			select {
			case <-time.After(retryDelay):
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("request timeout: GET %s%s: %w", c.baseURL, path, err)
		}
		return nil, nil, err
	}
}

// gzipReader turns a file into a gzip stream, read as it is compressed.
func gzipReader(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		zw, _ := gzip.NewWriterLevel(pw, gzip.BestSpeed)
		_, err := io.Copy(zw, f)
		if cerr := zw.Close(); err == nil {
			err = cerr
		}
		f.Close()
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// saveBody writes a response body, gunzipped when the server compressed it,
// to target, through a temporary file of its own beside it that then takes
// target's place: two downloads of the same target do not share one, and the
// target is never missing in between.
func saveBody(resp *http.Response, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(target), filepath.Base(target)+".*.download")
	if err != nil {
		return err
	}
	tmp := out.Name()
	var in io.Reader = resp.Body
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Encoding")), "gzip") {
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			out.Close()
			os.Remove(tmp)
			return err
		}
		defer zr.Close()
		in = zr
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ClearAuthSession forgets the tokens, as AppSettingsService.ClearAuthSession
// does.
func ClearAuthSession(s *model.Settings, clearEmail bool) {
	s.AuthAccessToken = nil
	s.AuthRefreshToken = nil
	s.AuthIdToken = nil
	s.AuthExpiresAtUnixMs = 0
	if clearEmail {
		s.AuthUserEmail = nil
	}
}
