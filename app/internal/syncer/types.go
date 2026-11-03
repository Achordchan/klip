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
	// Now is time.Now unless a test sets it.
	Now func() time.Time
}
