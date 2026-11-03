package syncclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// Manifest is manifest.json of the server: which snapshot is the latest.
type Manifest struct {
	Schema  int    `json:"schema"`
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	Latest  struct {
		Version            string `json:"version"`
		UpdatedAt          string `json:"updatedAt"`
		DeviceID           string `json:"deviceId"`
		CreatedFromVersion string `json:"createdFromVersion"`
		Snapshot           struct {
			Type     string `json:"type"`
			FileName string `json:"fileName"`
			SHA256   string `json:"sha256"`
			Size     int64  `json:"size"`
		} `json:"snapshot"`
	} `json:"latest"`
}

// ParseManifest reads the text GetManifestJSON returns.
func ParseManifest(text string) (Manifest, error) {
	var m Manifest
	err := json.Unmarshal([]byte(text), &m)
	return m, err
}

// UploadResult is the answer to a snapshot upload; the fields the server
// leaves out are empty.
type UploadResult struct {
	Version string
	SHA256  string
	Size    *int64
}

// GetManifestJSON returns the manifest as the server sends it. A server
// without a snapshot answers 404, which is an *APIError.
func (c *Client) GetManifestJSON(ctx context.Context, accessToken string) (string, error) {
	if err := c.needServer(accessToken, true); err != nil {
		return "", nil // C# answers null
	}
	resp, cancel, err := c.getWithRetry(ctx, "/sync/manifest", accessToken, manifestTimeout, getRetryCount)
	if err != nil {
		return "", err
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", newAPIError(resp, "get manifest failed", true)
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// UploadSnapshot sends the snapshot file, gzipped, and replaces the one of
// the account. The other devices are told through the event stream.
func (c *Client) UploadSnapshot(ctx context.Context, accessToken, snapshotPath string) (*UploadResult, error) {
	if err := c.needServer(accessToken, true); err != nil {
		return nil, err
	}
	if _, err := os.Stat(snapshotPath); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()

	body, err := gzipReader(snapshotPath)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPut, "/sync/snapshot", strings.TrimSpace(accessToken), body)
	if err != nil {
		body.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, newAPIError(resp, "upload snapshot failed", true)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("upload response: %w", err)
	}
	obj := decodeObject(b)
	res := &UploadResult{
		Version: scalarString(obj["version"]),
		SHA256:  scalarString(obj["sha256"]),
		Size:    scalarInt64(obj["size"]),
	}
	if strings.TrimSpace(res.Version) == "" && strings.TrimSpace(res.SHA256) == "" {
		return nil, errors.New("upload snapshot: response has no snapshot identifier")
	}
	return res, nil
}

// DownloadSnapshot saves the latest snapshot to targetPath.
func (c *Client) DownloadSnapshot(ctx context.Context, accessToken, targetPath string) error {
	return c.download(ctx, "/sync/snapshot", accessToken, targetPath, "download snapshot failed", true)
}

// CheckMissingBlobs says which of the hashes the server does not have.
func (c *Client) CheckMissingBlobs(ctx context.Context, accessToken string, hashes []string) ([]string, error) {
	if err := c.needServer(accessToken, true); err != nil {
		return nil, err
	}
	if len(hashes) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, blobCheckTimout)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"hashes": hashes})
	req, err := c.newRequest(ctx, http.MethodPost, "/sync/blobs/check", strings.TrimSpace(accessToken), strings.NewReader(string(payload)))
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
		return nil, newAPIError(resp, "check missing blobs failed", false)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	raw, ok := decodeObject(b)["missing"]
	if !ok {
		return nil, errors.New("check missing blobs: response has no missing list")
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil || list == nil {
		return nil, errors.New("check missing blobs: malformed missing list")
	}
	var missing []string
	for _, raw := range list {
		var s string
		if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			missing = append(missing, s)
		}
	}
	return missing, nil
}

// UploadBlob sends a file under its SHA-256, gzipped. The server checks the
// hash.
func (c *Client) UploadBlob(ctx context.Context, accessToken, sha256, sourcePath string) error {
	if err := c.needServer(accessToken, true); err != nil {
		return err
	}
	if strings.TrimSpace(sha256) == "" {
		return errors.New("missing blob hash")
	}
	if _, err := os.Stat(sourcePath); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	body, err := gzipReader(sourcePath)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPut, "/sync/blobs/"+sha256, strings.TrimSpace(accessToken), body)
	if err != nil {
		body.Close()
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newAPIError(resp, "upload blob failed", false)
	}
	return nil
}

// DownloadBlob saves the blob with this SHA-256 to targetPath.
func (c *Client) DownloadBlob(ctx context.Context, accessToken, sha256, targetPath string) error {
	if strings.TrimSpace(sha256) == "" {
		return errors.New("missing blob hash")
	}
	return c.download(ctx, "/sync/blobs/"+sha256, accessToken, targetPath, "download blob failed", false)
}

func (c *Client) download(ctx context.Context, path, accessToken, targetPath, failure string, authHeaders bool) error {
	if err := c.needServer(accessToken, true); err != nil {
		return err
	}
	if strings.TrimSpace(targetPath) == "" {
		return errors.New("targetPath is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, transferTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, path, strings.TrimSpace(accessToken), nil)
	if err != nil {
		return err
	}
	// Asking for gzip by hand means unpacking it by hand: saveBody does.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newAPIError(resp, failure, authHeaders)
	}
	return saveBody(resp, targetPath)
}
