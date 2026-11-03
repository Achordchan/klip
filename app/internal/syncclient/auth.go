package syncclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"klip/internal/model"
)

// TokenSet is what the server answers to a login, a registration or a
// refresh.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
	Email        string
}

// accountURL is the address for the account calls: like AuthService, they
// fall back to the official server.
func (c *Client) accountURL() string {
	if c.baseURL == "" {
		return strings.TrimRight(model.OfficialSyncBaseUrl, "/")
	}
	return c.baseURL
}

// Login signs in with an email and a password.
func (c *Client) Login(ctx context.Context, email, password string) (TokenSet, error) {
	return c.tokenRequest(ctx, "/auth/login", map[string]any{"email": strings.TrimSpace(email), "password": password})
}

// Register creates an account and signs in.
func (c *Client) Register(ctx context.Context, email, password string) (TokenSet, error) {
	return c.tokenRequest(ctx, "/auth/register", map[string]any{"email": strings.TrimSpace(email), "password": password})
}

// Refresh trades a refresh token for new tokens. The server revokes the old
// refresh token.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (TokenSet, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return TokenSet{}, errors.New("refreshToken is empty")
	}
	return c.tokenRequest(ctx, "/auth/refresh", map[string]any{"refreshToken": strings.TrimSpace(refreshToken)})
}

// CurrentEmail asks who the access token is. It says "" when the server turns
// the token down (400, 401, 403); any other failure is an error.
func (c *Client) CurrentEmail(ctx context.Context, accessToken string) (string, error) {
	if strings.TrimSpace(accessToken) == "" {
		return "", nil
	}
	ctx, cancel := c.withAccountTimeout(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.accountURL()+"/auth/me", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
			return "", nil // the token is turned down
		}
		// The server is busy or failing (5xx, 429...): that says nothing
		// about the token, and the caller must not take it for a refusal.
		return "", &APIError{StatusCode: resp.StatusCode, Message: "account check failed"}
	}
	b, _ := io.ReadAll(resp.Body)
	var s string
	if json.Unmarshal(decodeObject(b)["email"], &s) != nil {
		return "", nil
	}
	return s, nil
}

// SendPasswordCode asks for a verification code by email. purpose is "reset"
// or "change" ("change" needs the access token). It returns the seconds to
// wait before asking again.
func (c *Client) SendPasswordCode(ctx context.Context, purpose, email, accessToken string) (int, error) {
	purpose = strings.ToLower(strings.TrimSpace(purpose))
	payload := map[string]any{"purpose": purpose}
	if purpose != "change" {
		payload["email"] = strings.TrimSpace(email)
	}
	obj, err := c.jsonRequest(ctx, "/auth/password/code", payload, accessToken)
	if err != nil {
		return 0, err
	}
	if err := successResponse(obj); err != nil {
		return 0, err
	}
	var n int
	if json.Unmarshal(obj["cooldownSeconds"], &n) == nil {
		return max(0, n), nil
	}
	return 60, nil
}

// ResetPassword sets a new password with the code that was mailed.
func (c *Client) ResetPassword(ctx context.Context, email, code, newPassword string) error {
	obj, err := c.jsonRequest(ctx, "/auth/password/reset", map[string]any{
		"email": strings.TrimSpace(email), "code": strings.TrimSpace(code), "newPassword": newPassword,
	}, "")
	if err != nil {
		return err
	}
	return successResponse(obj)
}

// ChangePassword changes the password of the signed-in account with the code
// that was mailed.
func (c *Client) ChangePassword(ctx context.Context, accessToken, code, newPassword string) error {
	obj, err := c.jsonRequest(ctx, "/auth/password/change", map[string]any{
		"code": strings.TrimSpace(code), "newPassword": newPassword,
	}, accessToken)
	if err != nil {
		return err
	}
	return successResponse(obj)
}

// withAccountTimeout bounds an account call. A client built by hand, without
// New, gets the default.
func (c *Client) withAccountTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := c.accountTimeout
	if timeout <= 0 {
		timeout = accountCallTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func (c *Client) tokenRequest(ctx context.Context, path string, payload map[string]any) (TokenSet, error) {
	obj, err := c.jsonRequest(ctx, path, payload, "")
	if err != nil {
		return TokenSet{}, err
	}
	t := TokenSet{
		AccessToken:  scalarString(obj["accessToken"]),
		RefreshToken: scalarString(obj["refreshToken"]),
		Email:        scalarString(obj["email"]),
		ExpiresAt:    time.Now().UTC().Add(2 * time.Hour),
	}
	if strings.TrimSpace(t.AccessToken) == "" {
		return TokenSet{}, errors.New("token response missing accessToken")
	}
	if text := scalarString(obj["accessTokenExpiresAt"]); strings.TrimSpace(text) != "" {
		if v, err := model.ParseTime(text); err == nil {
			t.ExpiresAt = v
		}
	}
	return t, nil
}

// jsonRequest posts a JSON body. A failure carries the "error" of the
// answer, or "auth failed (status)".
// successResponse requires the explicit success flag of a password operation:
// a proxy's HTML page or a truncated body is not a success.
func successResponse(obj map[string]json.RawMessage) error {
	var ok bool
	if json.Unmarshal(obj["success"], &ok) != nil || !ok {
		return errors.New("auth server returned no success response")
	}
	return nil
}

func (c *Client) jsonRequest(ctx context.Context, path string, payload any, accessToken string) (map[string]json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ctx, cancel := c.withAccountTimeout(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.accountURL()+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if t := strings.TrimSpace(accessToken); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	text, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, rerr
	}
	obj := decodeObject(text)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := scalarString(obj["error"])
		if strings.TrimSpace(msg) == "" {
			msg = fmt.Sprintf("auth failed (%d)", resp.StatusCode)
		}
		return nil, &APIError{StatusCode: resp.StatusCode, Message: msg, Body: string(text), Code: scalarString(obj["code"]), Plain: true}
	}
	return obj, nil
}
