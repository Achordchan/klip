// Package update checks for a new version and applies it: the update manifest
// on the release server, the zip package of a "light update" with its file
// list, and the helper that replaces the program's files after it exits. It
// is UpdateService and UpdateCoordinator of the C# client with the same
// formats and rules, because the 1.0.10 and older clients read the same
// manifest and package, and 1.0.11 is the first version their updater meets.
package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// AppID is the application id in the manifest and the package list. It is
// "maccy" in every version, so that a package built now is accepted by the
// clients that exist.
const AppID = "maccy"

// ExeName is the program file that a package must contain and that is
// started after an update.
const ExeName = "maccy.exe"

// Manifest is docs/updates/manifest.json.
type Manifest struct {
	AppID  string  `json:"appId"`
	Latest Release `json:"latest"`
}

// Release is the newest version.
type Release struct {
	Version     string    `json:"version"`
	Mandatory   bool      `json:"mandatory"`
	Notes       string    `json:"notes"`
	PublishedAt string    `json:"publishedAt"`
	Installer   Installer `json:"installer"`
	// Package is the light update for Windows x64, where 1.0.10 and older
	// look for it.
	Package *Package `json:"package,omitempty"`
	// Packages lists the light updates of every platform: older clients do
	// not read it, new ones take the entry of their own runtime.
	Packages []Package `json:"packages,omitempty"`
}

// Installer is the full installer, the fallback.
type Installer struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Package is a zip of files to lay over the installation.
type Package struct {
	Kind    string `json:"kind"`
	Runtime string `json:"runtime"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// ParseManifest reads a manifest. Names match without regard to case, as
// System.Text.Json is asked to do.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(lowerKeys(data), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// lowerKeys is a stand-in for PropertyNameCaseInsensitive: the keys of the
// manifest are all lower camel case, so a document whose keys differ in case
// is rewritten into that spelling before it is read.
func lowerKeys(data []byte) []byte {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return data
	}
	out, err := json.Marshal(fixKeys(v))
	if err != nil {
		return data
	}
	return out
}

var knownKeys = map[string]string{}

func init() {
	for _, k := range []string{"appId", "latest", "version", "mandatory", "notes", "publishedAt", "installer", "package", "packages",
		"url", "sha256", "size", "kind", "runtime", "files", "path", "legacyFiles"} {
		knownKeys[strings.ToLower(k)] = k
	}
}

func fixKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			if canon, ok := knownKeys[strings.ToLower(k)]; ok {
				k = canon
			}
			m[k] = fixKeys(x)
		}
		return m
	case []any:
		for i := range t {
			t[i] = fixKeys(t[i])
		}
	}
	return v
}

// PackageManifest is maccy-package.json inside the zip: what the zip holds,
// with a checksum for each file.
type PackageManifest struct {
	AppID   string        `json:"appId"`
	Version string        `json:"version"`
	Runtime string        `json:"runtime"`
	Files   []PackageFile `json:"files"`
	// LegacyFiles are files of an earlier program (the .NET one) that this
	// version no longer needs. 1.0.10 and older ignore the list.
	LegacyFiles []string `json:"legacyFiles,omitempty"`
}

// PackageFile is one file of a package.
type PackageFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ParsePackageManifest reads maccy-package.json.
func ParsePackageManifest(data []byte) (*PackageManifest, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // a file saved by Windows PowerShell starts with a BOM
	var m PackageManifest
	if err := json.Unmarshal(lowerKeys(data), &m); err != nil {
		return nil, err
	}
	if m.Files == nil && m.AppID == "" && m.Version == "" {
		return nil, errors.New("package manifest invalid")
	}
	return &m, nil
}
