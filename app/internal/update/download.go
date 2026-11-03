package update

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Progress is told how much of a download is in; total is 0 when unknown.
type Progress func(read, total int64)

// ProtectedRoots and ProtectedFiles are what a package must never hold or
// replace: the user's data, which lives in the install folder of a default
// installation. They are the lists of UpdateService.
var (
	ProtectedRoots = []string{"images", "files", "blobs", "sync", "updates"}
	ProtectedFiles = []string{"settings.json", "history.json", "preferences_last_error.txt", "sync_last_error.txt", "auth_last_error.txt", "capture_debug.log"}
)

// IsProtected says whether a relative path (either slash) is user data.
func IsProtected(rel string) bool {
	p := strings.ToLower(strings.TrimLeft(strings.ReplaceAll(rel, "\\", "/"), "/"))
	for _, f := range ProtectedFiles {
		if p == f || strings.HasPrefix(p, f+"/") {
			return true
		}
	}
	for _, r := range ProtectedRoots {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// windowsAlias says whether a name has a spelling that Windows reads as some
// other file: a trailing dot or space ("settings.json." is settings.json), a
// colon (a stream of a file, or a drive), characters a file name cannot have,
// or an 8.3 short name (SETTIN~1.JSO). The data of the user is protected by
// name, so a name that is not what it will turn into is refused.
func windowsAlias(part string) bool {
	if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
		return true
	}
	if strings.ContainsAny(part, ":<>\"|?*") {
		return true
	}
	for i := 0; i+1 < len(part); i++ {
		if part[i] == '~' && part[i+1] >= '0' && part[i+1] <= '9' {
			return true
		}
	}
	return false
}

// NormalizePath is NormalizePackagePath: a relative path without "." and
// "..", with either slash, in the system's form.
func NormalizePath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("package path is empty")
	}
	n := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(p), "/", string(filepath.Separator)), "\\", string(filepath.Separator))
	if filepath.IsAbs(n) || filepath.VolumeName(n) != "" {
		return "", errors.New("package path must be relative: " + p)
	}
	var parts []string
	for _, part := range strings.Split(strings.TrimLeft(n, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		if part == "." || part == ".." {
			return "", errors.New("package path is unsafe: " + p)
		}
		if windowsAlias(part) {
			return "", errors.New("package path is unsafe on Windows: " + p)
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "", errors.New("package path is unsafe: " + p)
	}
	return filepath.Join(parts...), nil
}

// insideRoot is GetPathInsideRoot.
func insideRoot(root, rel string) (string, error) {
	r, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full := filepath.Join(r, rel)
	prefix := strings.TrimRight(r, string(filepath.Separator)) + string(filepath.Separator)
	if !strings.HasPrefix(strings.ToLower(full), strings.ToLower(prefix)) {
		return "", errors.New("package path escapes root: " + rel)
	}
	// Spelling inside the root is not enough: a link or a junction on the way
	// would carry the write somewhere else (the user's data, another folder).
	cur := r
	for _, part := range strings.Split(strings.TrimPrefix(full[len(r):], string(filepath.Separator)), string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if err != nil {
			break // from here on nothing exists yet
		}
		if st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return "", errors.New("package path goes through a link: " + rel)
		}
	}
	return full, nil
}

// SHA256File is the checksum of a file in upper-case hex, the way the
// manifest and package lists spell it (comparison ignores case).
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil))), nil
}

// download is DownloadAndVerifyFileAsync: three tries, a temporary file next
// to the target, the size and the checksum checked before it takes the
// target's place.
func (c *Client) download(ctx context.Context, url, target, wantSHA string, wantSize int64, progress Progress) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	temp := target + ".download"
	_ = os.Remove(temp)
	_, err := retry(ctx, 3, c.downloadPause, func(int) (struct{}, error) {
		_ = os.Remove(temp)
		// A transfer that stops for a while is given up and tried again: the
		// timer is pushed back at every piece that arrives.
		actx, cancel := context.WithCancel(ctx)
		defer cancel()
		idle := c.DownloadIdle
		if idle <= 0 {
			idle = 60 * time.Second
		}
		watchdog := time.AfterFunc(idle, cancel)
		defer watchdog.Stop()
		req, err := http.NewRequestWithContext(actx, http.MethodGet, url, nil)
		if err != nil {
			return struct{}{}, err
		}
		resp, err := c.http().Do(req)
		if err != nil {
			return struct{}{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return struct{}{}, &httpError{resp.StatusCode}
		}
		total := wantSize
		if resp.ContentLength > 0 {
			total = resp.ContentLength
		}
		out, err := os.Create(temp)
		if err != nil {
			return struct{}{}, err
		}
		defer out.Close()
		var read int64
		buf := make([]byte, 80*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				watchdog.Reset(idle)
				if wantSize > 0 && read+int64(n) > wantSize {
					return struct{}{}, errors.New("download exceeds expected size")
				}
				if _, werr := out.Write(buf[:n]); werr != nil {
					return struct{}{}, werr
				}
				read += int64(n)
				if progress != nil {
					progress(read, total)
				}
			}
			if rerr == io.EOF {
				return struct{}{}, nil
			}
			if rerr != nil {
				return struct{}{}, rerr
			}
		}
	}, transient)
	if err != nil {
		_ = os.Remove(temp)
		return err
	}
	if wantSize > 0 {
		if st, err := os.Stat(temp); err != nil || st.Size() != wantSize {
			_ = os.Remove(temp)
			return errors.New("file size mismatch")
		}
	}
	if strings.TrimSpace(wantSHA) != "" {
		got, err := SHA256File(temp)
		if err != nil || !strings.EqualFold(got, strings.TrimSpace(wantSHA)) {
			_ = os.Remove(temp)
			return errors.New("sha256 mismatch")
		}
	}
	_ = os.Remove(target)
	return os.Rename(temp, target)
}

// DownloadInstaller fetches the full installer into folder.
func (c *Client) DownloadInstaller(ctx context.Context, info Info, folder string, progress Progress) (string, error) {
	if err := validateInstaller(info); err != nil {
		return "", err
	}
	target := filepath.Join(folder, "maccy-"+info.Version+"-setup.exe")
	if err := c.download(ctx, info.InstallerURL, target, info.InstallerSHA256, info.InstallerSize, progress); err != nil {
		return "", err
	}
	return target, nil
}

// Prepared is a package that is downloaded, unpacked and checked.
type Prepared struct {
	Version         string
	PackagePath     string
	StagingPath     string
	PackageManifest string
	Manifest        *PackageManifest
}

// DownloadPackage fetches the zip, unpacks it into a new folder under
// updateRoot/staging and checks it against its own file list.
func (c *Client) DownloadPackage(ctx context.Context, info Info, updateRoot string, progress Progress) (*Prepared, error) {
	rt := c.runtime()
	if !info.HasSupportedPackage(rt) {
		return nil, errors.New("manifest package is not supported by this client")
	}
	pkgDir := filepath.Join(updateRoot, "packages")
	stagingRoot := filepath.Join(updateRoot, "staging")
	for _, d := range []string{pkgDir, stagingRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	zipPath := filepath.Join(pkgDir, "maccy-"+info.Version+"-"+info.PackageRuntime+".zip")
	if err := c.download(ctx, info.PackageURL, zipPath, info.PackageSHA256, info.PackageSize, progress); err != nil {
		return nil, err
	}
	return PreparePackage(info, zipPath, stagingRoot)
}

// PreparePackage unpacks and checks a zip that is already on disk.
func PreparePackage(info Info, zipPath, stagingRoot string) (prep *Prepared, err error) {
	var id [16]byte
	_, _ = rand.Read(id[:])
	staging := filepath.Join(stagingRoot, info.Version+"-"+hex.EncodeToString(id[:]))
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return nil, err
	}
	// What was unpacked is only kept for a package that is good: a failed one
	// would hold the space the installer fallback needs, once for each try.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := extractSafely(zipPath, staging); err != nil {
		return nil, err
	}
	mpath := filepath.Join(staging, "maccy-package.json")
	data, err := os.ReadFile(mpath)
	if err != nil {
		return nil, errors.New("package manifest missing")
	}
	pm, err := ParsePackageManifest(data)
	if err != nil {
		return nil, errors.New("package manifest invalid")
	}
	if err := ValidatePackage(info, staging, pm); err != nil {
		return nil, err
	}
	return &Prepared{Version: info.Version, PackagePath: zipPath, StagingPath: staging, PackageManifest: mpath, Manifest: pm}, nil
}

// extractSafely unpacks every entry below root and refuses a name that
// leaves it or a file that is there already.
func extractSafely(zipPath, root string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	var expanded uint64
	for _, f := range zr.File {
		if f.UncompressedSize64 > 512*1024*1024 || expanded > 2*1024*1024*1024-f.UncompressedSize64 {
			return errors.New("update archive expansion limit exceeded")
		}
		expanded += f.UncompressedSize64
	}
	for _, f := range zr.File {
		rel, err := NormalizePath(f.Name)
		if err != nil {
			return err
		}
		dest, err := insideRoot(root, rel)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/") {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := extractOne(f, dest); err != nil {
			return err
		}
	}
	return nil
}

func extractOne(f *zip.File, dest string) error {
	in, err := f.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, 512*1024*1024+1))
	if err == nil && n > 512*1024*1024 {
		err = errors.New("update entry expansion limit exceeded")
	}
	if err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ValidatePackage is ValidatePackageManifest: the package is ours (app id,
// version, runtime), has files, none of them user data, each present with
// the size and checksum the list gives, and the program among them.
func ValidatePackage(info Info, staging string, m *PackageManifest) error {
	if !strings.EqualFold(m.AppID, AppID) {
		return errors.New("package app id mismatch")
	}
	if !strings.EqualFold(NormalizeVersion(m.Version), info.Version) {
		return errors.New("package version mismatch")
	}
	if !strings.EqualFold(m.Runtime, info.PackageRuntime) {
		return errors.New("package runtime mismatch")
	}
	if len(m.Files) == 0 {
		return errors.New("package file list is empty")
	}
	hasExe := false
	seen := map[string]string{}
	for _, f := range m.Files {
		rel, err := NormalizePath(f.Path)
		if err != nil {
			return err
		}
		if IsProtected(rel) {
			return errors.New("package contains protected data path: " + f.Path)
		}
		// Two entries that name the same file (Windows ignores case and slash
		// direction) would be copied over each other.
		k := strings.ToLower(strings.ReplaceAll(rel, "\\", "/"))
		if prev, dup := seen[k]; dup {
			return errors.New("package lists the same file twice: " + prev + " and " + f.Path)
		}
		seen[k] = f.Path
		if strings.EqualFold(strings.ReplaceAll(rel, "\\", "/"), ExeName) {
			hasExe = true
		}
		abs, err := insideRoot(staging, rel)
		if err != nil {
			return err
		}
		st, err := os.Stat(abs)
		if err != nil || st.IsDir() {
			return errors.New("package file missing: " + f.Path)
		}
		if f.Size >= 0 && f.Size != st.Size() {
			return errors.New("package file size mismatch: " + f.Path)
		}
		if strings.TrimSpace(f.SHA256) == "" {
			return errors.New("package file sha256 missing: " + f.Path)
		}
		got, err := SHA256File(abs)
		if err != nil || !strings.EqualFold(got, strings.TrimSpace(f.SHA256)) {
			return errors.New("package file sha256 mismatch: " + f.Path)
		}
	}
	if !hasExe {
		return errors.New("package missing " + ExeName)
	}
	// The files to remove are checked now too: a bad name found while applying
	// would come after the program has exited and the new files are in place.
	for _, old := range m.LegacyFiles {
		if _, err := NormalizePath(old); err != nil {
			return err
		}
	}
	return nil
}

func validateInstaller(info Info) error {
	if strings.TrimSpace(info.InstallerURL) == "" {
		return errors.New("manifest missing installer url")
	}
	sum, err := hex.DecodeString(strings.TrimSpace(info.InstallerSHA256))
	if err != nil || len(sum) != sha256.Size {
		return errors.New("installer requires a valid SHA-256 checksum")
	}
	u, err := url.Parse(info.InstallerURL)
	if err != nil {
		return err
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return errors.New("installer requires HTTPS")
	}

	return nil
}
