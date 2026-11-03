package syncclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// SubscriptionStatus is what /subscription/status says. The fields the server
// leaves out are nil.
type SubscriptionStatus struct {
	Subscribed        bool
	ExpiresAt         *string
	Tier              *string
	StorageBytes      *int64
	StorageLimitBytes *int64
	RetentionDays     *int
	OverLimit         *bool
}

// RedeemResult is the answer to a card redemption. Error is the body of a
// refusal.
type RedeemResult struct {
	Success   bool
	ExpiresAt *string
	Error     string
}

// Health says whether the server answers /health with {"ok":true}.
func (c *Client) Health(ctx context.Context) bool {
	if c.baseURL == "" {
		return false
	}
	resp, cancel, err := c.getWithRetry(ctx, "/health", "", healthTimeout, 0)
	if err != nil {
		return false
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false
	}
	b, _ := io.ReadAll(resp.Body)
	var ok bool
	return json.Unmarshal(decodeObject(b)["ok"], &ok) == nil && ok
}

// SubscriptionStatus asks for the subscription of the account. A rejected
// token (401 or 403) is "not subscribed", not an error.
func (c *Client) SubscriptionStatus(ctx context.Context, accessToken string) (*SubscriptionStatus, error) {
	if err := c.needServer(accessToken, true); err != nil {
		return nil, nil // C# answers null: nothing to ask
	}
	resp, cancel, err := c.getWithRetry(ctx, "/subscription/status", accessToken, statusTimeout, getRetryCount)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return &SubscriptionStatus{}, nil
		}
		return nil, newAPIError(resp, "get subscription status failed", true)
	}
	obj, err := readObject(resp, "get subscription status")
	if err != nil {
		return nil, err
	}
	st := &SubscriptionStatus{
		ExpiresAt:         nullableString(obj["expiresAt"]),
		Tier:              nil,
		StorageBytes:      scalarInt64(obj["storageBytes"]),
		StorageLimitBytes: scalarInt64(obj["storageLimitBytes"]),
		OverLimit:         scalarBool(obj["overLimit"]),
	}
	if v := scalarBool(obj["subscribed"]); v != nil {
		st.Subscribed = *v
	}
	if t := scalarString(obj["tier"]); t != "" {
		st.Tier = &t
	}
	if n := scalarInt64(obj["retentionDays"]); n != nil {
		d := int(*n)
		st.RetentionDays = &d
	}
	return st, nil
}

// RedeemCard redeems a card code. It returns nil when there is nothing to
// ask (no server, token or code).
func (c *Client) RedeemCard(ctx context.Context, accessToken, code string) (*RedeemResult, error) {
	if c.needServer(accessToken, true) != nil || strings.TrimSpace(code) == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, redeemTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"code": code})
	req, err := c.newRequest(ctx, http.MethodPost, "/card/redeem", strings.TrimSpace(accessToken), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &RedeemResult{Error: readBodySafe(resp)}, nil
	}
	obj, err := readObject(resp, "redeem card")
	if err != nil {
		return nil, err
	}
	r := &RedeemResult{ExpiresAt: nullableString(obj["expiresAt"])}
	if v := scalarBool(obj["success"]); v != nil {
		r.Success = *v
	}
	return r, nil
}
