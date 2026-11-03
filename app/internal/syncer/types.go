// Package syncer decides when and how the history is synchronized with the
// server, and does it: it is SyncService of the C# client together with the
// flow that its callers (the main window's auto sync and the preferences'
// "sync now") run around it. The server calls are syncclient's, the file
// format is snapshot's; what is here is the logic between them: which side is
// newer, what to do about a conflict, and what is remembered afterwards in
// the SyncLast* settings.
package syncer

import (
	"context"
	"errors"
	"strings"
	"time"

	"klip/internal/model"
	"klip/internal/paths"
	"klip/internal/syncclient"
)

// Errors with the texts the C# client shows.
var (
	ErrSubscriptionExpired = errors.New("subscription expired")
	ErrNotLoggedIn         = errors.New("not logged in")
	// ErrSessionChanged says the signed-in account changed while the sync was
	// running: its requests and its state belong to the old session, so the
	// workflow stops instead of using the new account's credentials.
	ErrSessionChanged = errors.New("sync session changed")
	// ErrRestoreSuperseded says another sync ran between a restore and the
	// recording of its state: that sync's state is newer, and is kept.
	ErrRestoreSuperseded = errors.New("sync ran after the restore")
	// ErrNoFlush says a download or a merge was asked of a Syncer that
	// cannot save the history: it would record as saved a history that is
	// not, and delete the files the saved one still names.
	ErrNoFlush = errors.New("syncer: Config.Flush is required to apply a download or a merge")
)

// Direction is what a sync has to do.
type Direction int

const (
	None Direction = iota
	Upload
	Download
	Merge
)

func (d Direction) String() string {
	return [...]string{"none", "upload", "download", "merge"}[d]
}

// Progress is reported while a sync runs. Code names the step, Percent says
// how far it is.
type Progress struct {
	Code    string
	Message string
	Percent int
}

// ManifestInfo is the part of the server's manifest that the decision uses.
type ManifestInfo struct {
	Version        string
	UpdatedAt      *time.Time
	SnapshotSHA256 string
	SnapshotSize   *int64
	RawJSON        string
}

// Decision says what to do and why. Reason is one of remote_missing,
// first_sync_merge, first_sync_remote_exists, remote_rolled_back,
// state_matched, local_changed_only, remote_changed_only and
// timestamp_fallback.
type Decision struct {
	Direction        Direction
	LocalFingerprint string
	Manifest         ManifestInfo
	Reason           string
}

// History is the clipboard history that is synchronized: the owner of the
// list of items.
type History interface {
	// Items returns the items, newest first. The caller may keep the slice.
	Items() []model.Item
	// Mutate replaces the history with f applied to it as it is at that
	// moment, under the history's own lock, so that a capture or a note edit
	// made while a merge is prepared is part of the merge instead of being
	// overwritten. It does not delete the files of the items that are gone:
	// the caller removes them with RemoveFiles only after the new history is
	// safely saved, so a failed save can still recover them.
	Mutate(ctx context.Context, f func(items []model.Item) ([]model.Item, error)) error
	// RemoveFiles deletes payload files (images, cached files) that no item
	// refers to any more.
	RemoveFiles(paths []string) error
	// ReplaceAll replaces the history and removes the files of the items that
	// are gone.
	ReplaceAll(ctx context.Context, items []model.Item) error
	// ApplyLimits sets the history's limits from the settings: the number
	// of items, the size, and whether duplicates are merged.
	ApplyLimits(model.Settings)
	// ApplyLimitsAndSnapshot applies the limits and returns the items after
	// them and the ids the limits actually removed, atomically: a later
	// Items() read cannot tell a limit removal from a user deletion.
	ApplyLimitsAndSnapshot(model.Settings) ([]model.Item, []string)
}

// Tombstone is an item the user deleted at At. It stands for the item of
// that id and for any item that MergeItems takes for the same one (same
// MergeKey): two devices that copied the same text give it two ids. The items
// of the same content the user kept when deleting this one (Spare: with
// duplicates not merged, the history may hold several) are not it. Only an
// item captured at or before At is the deleted one; a copy made after it (or
// an item used, and so moved up, after it) is new, and stays.
type Tombstone struct {
	ID    string
	Key   string // MergeKey of the item
	At    time.Time
	Spare []string
}

// Covers says that t stands for it (see Tombstone).
func (t Tombstone) Covers(it model.Item) bool {
	if it.CapturedAt.After(t.At) {
		return false
	}
	if strings.EqualFold(t.ID, it.ID) {
		return true
	}
	if t.Key == "" || !strings.EqualFold(t.Key, MergeKey(it)) {
		return false
	}
	for _, id := range t.Spare {
		if strings.EqualFold(id, it.ID) {
			return false
		}
	}
	return true
}

// Config is what a Syncer works with.
type Config struct {
	Layout   paths.Layout
	Settings syncclient.Settings
	History  History
	// Flush writes the history to disk (ClipboardPersistenceService.FlushAsync).
	// A download or a merge needs it (ErrNoFlush).
	Flush func(ctx context.Context) error
	// Client talks to the server at BaseURL, which is the official one in the
	// app; tests point both at a server of their own.
	Client  *syncclient.Client
	BaseURL string
	// SuppressAutoSync is called with true before the sync changes the
	// history and with false after, so that the change does not start a sync.
	SuppressAutoSync func(on bool)
	// Deleted gives what the user deleted on this device (see Tombstone): a
	// merge or a download leaves those items out, instead of bringing them
	// back. It may be nil.
	Deleted func() []Tombstone
	// Dropped is called when a merge or a download left out such items, which
	// the server still has, before the result is written down as synced: the
	// server needs an upload. When it cannot write that down (an error), no
	// sync is written down as done until an upload went through: the history,
	// which lacks the items, then differs from the last state written, and the
	// next sync uploads it, also after a restart. It may be nil.
	Dropped func() error
	// Account names the account the work running is for, which Deleted and
	// Dropped are about; it may be nil (one account).
	Account func() string
	// Uploaded is called after an upload went through (or was found not to be
	// needed), with the items the server now has. It may be nil.
	Uploaded func(items []model.Item)
	// Now is time.Now unless a test sets it.
	Now func() time.Time
}
