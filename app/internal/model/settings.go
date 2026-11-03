package model

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// OfficialSyncBaseUrl is ServerDefaults.OfficialSyncBaseUrl. The server keeps
// its old name: it is a live address.
const OfficialSyncBaseUrl = "https://maccy.baymaxgroup.com"

// Settings is AppSettings. Names and order are those of the C# class.
// Properties this version does not know are kept in Extra and written back,
// so that a newer or older client's settings survive a round trip.
type Settings struct {
	Theme                    string
	StartWithWindows         bool
	AuthAccessToken          *string
	AuthRefreshToken         *string
	AuthIdToken              *string
	AuthExpiresAtUnixMs      int64
	AuthUserEmail            *string
	NasAgentBaseUrl          string
	SyncLastLocalFingerprint *string
	SyncLastRemoteVersion    *string
	SyncLastRemoteUpdatedAt  *string
	SyncLastRemoteSha256     *string
	MaxItems                 int32
	MaxMegabytes             int32
	CaptureText              bool
	CaptureImages            bool
	CaptureFiles             bool
	CaptureFileExtensions    string
	CaptureFileMaxMegabytes  int32
	MergeDuplicates          bool
	ExcludePinnedFromLimits  bool
	ShelfEnabled             bool
	ShelfTriggerModifier     string

	Extra map[string]json.RawMessage `json:"-"`
}

// DefaultSettings returns what new AppSettings() holds.
func DefaultSettings() Settings {
	return Settings{
		Theme:                   "System",
		NasAgentBaseUrl:         OfficialSyncBaseUrl,
		MaxItems:                200,
		MaxMegabytes:            300,
		CaptureText:             true,
		CaptureImages:           true,
		CaptureFiles:            true,
		CaptureFileExtensions:   ".pdf,.ppt,.pptx,.doc,.docx,.xls,.xlsx,.txt,.md,.csv,.zip,.rar,.7z,.png,.jpg,.jpeg,.gif,.bmp,.webp",
		CaptureFileMaxMegabytes: 20,
		MergeDuplicates:         true,
		ExcludePinnedFromLimits: true,
		ShelfEnabled:            true,
		ShelfTriggerModifier:    "Ctrl",
	}
}

// fields lists the known properties in the C# order, with where each one
// goes. C# matches names case-sensitively, and so does this.
func (s *Settings) fields() []struct {
	name string
	ptr  any
} {
	return []struct {
		name string
		ptr  any
	}{
		{"Theme", &s.Theme},
		{"StartWithWindows", &s.StartWithWindows},
		{"AuthAccessToken", &s.AuthAccessToken},
		{"AuthRefreshToken", &s.AuthRefreshToken},
		{"AuthIdToken", &s.AuthIdToken},
		{"AuthExpiresAtUnixMs", &s.AuthExpiresAtUnixMs},
		{"AuthUserEmail", &s.AuthUserEmail},
		{"NasAgentBaseUrl", &s.NasAgentBaseUrl},
		{"SyncLastLocalFingerprint", &s.SyncLastLocalFingerprint},
		{"SyncLastRemoteVersion", &s.SyncLastRemoteVersion},
		{"SyncLastRemoteUpdatedAt", &s.SyncLastRemoteUpdatedAt},
		{"SyncLastRemoteSha256", &s.SyncLastRemoteSha256},
		{"MaxItems", &s.MaxItems},
		{"MaxMegabytes", &s.MaxMegabytes},
		{"CaptureText", &s.CaptureText},
		{"CaptureImages", &s.CaptureImages},
		{"CaptureFiles", &s.CaptureFiles},
		{"CaptureFileExtensions", &s.CaptureFileExtensions},
		{"CaptureFileMaxMegabytes", &s.CaptureFileMaxMegabytes},
		{"MergeDuplicates", &s.MergeDuplicates},
		{"ExcludePinnedFromLimits", &s.ExcludePinnedFromLimits},
		{"ShelfEnabled", &s.ShelfEnabled},
		{"ShelfTriggerModifier", &s.ShelfTriggerModifier},
	}
}

// ParseSettings reads a settings document over the defaults: properties the
// document lacks keep their default, as in C#. A property of the wrong type
// fails the whole read, as it does there.
func ParseSettings(data []byte) (Settings, error) {
	s := DefaultSettings()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &doc); err != nil {
		return DefaultSettings(), err
	}
	known := map[string]any{}
	for _, f := range s.fields() {
		known[f.name] = f.ptr
	}
	for name, raw := range doc {
		if ptr, ok := known[name]; ok {
			if err := json.Unmarshal(raw, ptr); err != nil {
				return DefaultSettings(), err
			}
			continue
		}
		if s.Extra == nil {
			s.Extra = map[string]json.RawMessage{}
		}
		s.Extra[name] = raw
	}
	return s, nil
}

// Marshal writes the settings the way System.Text.Json writes AppSettings:
// compact, in the class's order, nulls included; then the properties kept in
// Extra, by name.
func (s Settings) Marshal() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	add := func(name string, raw []byte) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(name)
		b.Write(k)
		b.WriteByte(':')
		b.Write(raw)
	}
	for _, f := range s.fields() {
		raw, err := json.Marshal(f.ptr)
		if err != nil {
			return nil, err
		}
		add(f.name, raw)
	}
	names := make([]string, 0, len(s.Extra))
	for name := range s.Extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		add(name, s.Extra[name])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// MigrateLegacySyncBaseUrl is AppSettingsService.MigrateLegacySyncBaseUrl:
// the sync address is not a setting, it is always the official one. It says
// whether it changed anything.
func (s *Settings) MigrateLegacySyncBaseUrl() bool {
	cur := strings.TrimRight(strings.TrimSpace(s.NasAgentBaseUrl), "/")
	off := strings.TrimRight(strings.TrimSpace(OfficialSyncBaseUrl), "/")
	if strings.EqualFold(cur, off) && s.NasAgentBaseUrl == OfficialSyncBaseUrl {
		return false
	}
	s.NasAgentBaseUrl = OfficialSyncBaseUrl
	return true
}

// Portable returns the settings that travel in a snapshot: the ones
// SnapshotSqliteService.CopySettings copies, over a default AppSettings.
func (s Settings) Portable() Settings {
	p := DefaultSettings()
	p.CopyPortable(s)
	return p
}

// RestorePortable puts back old's value of each setting a snapshot carries
// where s still has the value installed put there: a setting changed since
// keeps its new value.
func (s *Settings) RestorePortable(installed, old Settings) {
	restore := func(cur *string, inst, prev string) {
		if *cur == inst {
			*cur = prev
		}
	}
	restoreBool := func(cur *bool, inst, prev bool) {
		if *cur == inst {
			*cur = prev
		}
	}
	restoreInt := func(cur *int32, inst, prev int32) {
		if *cur == inst {
			*cur = prev
		}
	}
	restore(&s.Theme, installed.Theme, old.Theme)
	restoreBool(&s.StartWithWindows, installed.StartWithWindows, old.StartWithWindows)
	restoreInt(&s.MaxItems, installed.MaxItems, old.MaxItems)
	restoreInt(&s.MaxMegabytes, installed.MaxMegabytes, old.MaxMegabytes)
	restoreBool(&s.CaptureText, installed.CaptureText, old.CaptureText)
	restoreBool(&s.CaptureImages, installed.CaptureImages, old.CaptureImages)
	restoreBool(&s.CaptureFiles, installed.CaptureFiles, old.CaptureFiles)
	restore(&s.CaptureFileExtensions, installed.CaptureFileExtensions, old.CaptureFileExtensions)
	restoreInt(&s.CaptureFileMaxMegabytes, installed.CaptureFileMaxMegabytes, old.CaptureFileMaxMegabytes)
	restoreBool(&s.MergeDuplicates, installed.MergeDuplicates, old.MergeDuplicates)
	restoreBool(&s.ExcludePinnedFromLimits, installed.ExcludePinnedFromLimits, old.ExcludePinnedFromLimits)
	restoreBool(&s.ShelfEnabled, installed.ShelfEnabled, old.ShelfEnabled)
	restore(&s.ShelfTriggerModifier, installed.ShelfTriggerModifier, old.ShelfTriggerModifier)
}

// CopyPortable copies the settings a snapshot carries from src.
func (s *Settings) CopyPortable(src Settings) {
	s.Theme = src.Theme
	s.StartWithWindows = src.StartWithWindows
	s.MaxItems = src.MaxItems
	s.MaxMegabytes = src.MaxMegabytes
	s.CaptureText = src.CaptureText
	s.CaptureImages = src.CaptureImages
	s.CaptureFiles = src.CaptureFiles
	s.CaptureFileExtensions = src.CaptureFileExtensions
	s.CaptureFileMaxMegabytes = src.CaptureFileMaxMegabytes
	s.MergeDuplicates = src.MergeDuplicates
	s.ExcludePinnedFromLimits = src.ExcludePinnedFromLimits
	s.ShelfEnabled = src.ShelfEnabled
	s.ShelfTriggerModifier = src.ShelfTriggerModifier
}
