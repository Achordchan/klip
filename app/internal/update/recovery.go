package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"klip/internal/durable"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type recoveryEntry struct {
	Path, Backup   string
	Existed, Moved bool
	Manifest       bool
	OriginalDigest string
}

// committedJournal is the journal of an update whose files are all in place
// and checked: what is left to do is to forget it, not to undo it. An
// uncommitted journal stays the bare list of entries that earlier versions
// write and read.
type committedJournal struct {
	Committed bool            `json:"committed"`
	Entries   []recoveryEntry `json:"entries"`
}

func recoveryPath(root string) string { return filepath.Join(root, ".klip-update-recovery.json") }

func recoveryEntries(items []rollbackItem) []recoveryEntry {
	entries := make([]recoveryEntry, len(items))
	for i, it := range items {
		entries[i] = recoveryEntry{it.path, it.backup, it.existed, it.moved, it.manifest, it.originalDigest}
	}
	return entries
}

func saveRecovery(root string, items []rollbackItem) error {
	b, err := json.Marshal(recoveryEntries(items))
	if err != nil {
		return err
	}
	return writeJournal(root, b)
}

// writeJournal and removeJournal are saveJournal and durable.Remove; tests
// make them fail.
var (
	writeJournal  = saveJournal
	removeJournal = durable.Remove
)

// saveCommitted marks the update in the journal as done, durably.
func saveCommitted(root string, items []rollbackItem) error {
	b, err := json.Marshal(committedJournal{Committed: true, Entries: recoveryEntries(items)})
	if err != nil {
		return err
	}
	return writeJournal(root, b)
}

func saveJournal(root string, b []byte) error { return saveDurable(root, recoveryPath(root), b) }

// rollbackPath is the rollback marker: the undo of an update that the
// helper began while the journal could not be made to say so (a committed
// journal that a scanner holds). Startup honours it before the journal, so
// an undo cut short is finished, whatever the journal says.
func rollbackPath(root string) string { return filepath.Join(root, ".klip-update-rollback.json") }

// saveRollback writes the rollback marker, durably.
func saveRollback(root string, items []rollbackItem) error {
	b, err := json.Marshal(recoveryEntries(items))
	if err != nil {
		return err
	}
	return saveDurable(root, rollbackPath(root), b)
}

// writeRollback is saveRollback; tests make it fail.
var writeRollback = saveRollback

func saveDurable(root, path string, b []byte) error {
	f, err := os.CreateTemp(root, ".recovery-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return durable.Rename(f.Name(), path)
}

// RecoverInterruptedUpdate restores a recorded transaction before normal startup.
func RecoverInterruptedUpdate(root string) error {
	// A rollback marker first: an undo was begun, and is finished now, even
	// over a journal that says the update was committed.
	if b, err := os.ReadFile(rollbackPath(root)); err == nil {
		var entries []recoveryEntry
		if err := json.Unmarshal(b, &entries); err != nil {
			return err
		}
		if err := restoreEntries(root, entries); err != nil {
			return err
		}
		_ = durable.Remove(recoveryPath(root)) // the old version is back, whatever it says
		// The marker must go, or every start would recover again; one that
		// stays is a failed recovery (the helper then does not start Klip).
		if err := durable.Remove(rollbackPath(root)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	b, err := os.ReadFile(recoveryPath(root))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var entries []recoveryEntry
	if trimmed := strings.TrimSpace(string(b)); strings.HasPrefix(trimmed, "{") {
		var j committedJournal
		if err = json.Unmarshal(b, &j); err != nil {
			return err
		}
		if j.Committed {
			// The new version is whole: keep it, forget the journal. One that
			// cannot be removed is no reason to fail: there is nothing to undo,
			// and startup passes over a committed journal (journalCommitted).
			_ = durable.Remove(recoveryPath(root))
			return nil
		}
		entries = j.Entries
	} else if err = json.Unmarshal(b, &entries); err != nil {
		return err
	}
	if err := restoreEntries(root, entries); err != nil {
		return err
	}
	return os.Remove(recoveryPath(root))
}

// restoreEntries puts back what the entries of a journal changed, last first.
func restoreEntries(root string, entries []recoveryEntry) error {
	restored := map[string]bool{}
	for _, e := range entries {
		if e.Moved {
			if _, err := os.Stat(e.Backup); os.IsNotExist(err) {
				digest, verifyErr := transitionDigest(e.Path)
				if verifyErr != nil || e.OriginalDigest == "" || digest != e.OriginalDigest {
					return fmt.Errorf("cannot verify transition recovery for %s: backup missing", e.Path)
				}
				restored[e.Path] = true
			}
		}
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		skip := false
		for path := range restored {
			rel, err := filepath.Rel(path, e.Path)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				skip = true
			}
		}
		if skip {
			continue
		}
		rel, err := filepath.Rel(root, e.Path)
		if err != nil {
			return err
		}
		if _, err = insideRoot(root, rel); err != nil && !e.Manifest {
			return err
		}
		if e.Moved {
			if _, err := os.Stat(e.Backup); os.IsNotExist(err) {
				continue
			}
			if err = os.RemoveAll(e.Path); err != nil {
				return err
			}
			if err = os.Rename(e.Backup, e.Path); err != nil {
				return err
			}
		} else if e.Existed {
			if err = copyFile(e.Backup, e.Path); err != nil {
				return err
			}
		} else {
			if err = os.Remove(e.Path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// recoveryNeeded says whether startup must recover first: a rollback marker,
// or a journal that is not committed. A committed journal alone is removed if
// it can be, and needs nothing.
func recoveryNeeded(root string) (bool, error) {
	if _, err := os.Stat(rollbackPath(root)); err == nil {
		return true, nil
	} else if !os.IsNotExist(err) {
		return true, err
	}
	if _, err := os.Stat(recoveryPath(root)); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return true, err
	}
	if journalCommitted(root) {
		_ = durable.Remove(recoveryPath(root))
		return false, nil
	}
	return true, nil
}

// journalCommitted says that the journal in root is that of an update that
// was finished and only not forgotten.
func journalCommitted(root string) bool {
	b, err := os.ReadFile(recoveryPath(root))
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(b)), "{") {
		return false
	}
	var j committedJournal
	return json.Unmarshal(b, &j) == nil && j.Committed
}

// StartRecovery launches a separate executable so Windows can replace the installed binary.
// A committed journal needs no recovery: it is removed if it can be, and the
// program starts either way (were it kept, every start would recover again).
func StartRecovery(root string) (bool, error) {
	needed, err := recoveryNeeded(root)
	if !needed || err != nil {
		return needed, err
	}
	exe, err := os.Executable()
	if err != nil {
		return true, err
	}
	dir, err := os.MkdirTemp("", "klip-recovery-")
	if err != nil {
		return true, err
	}
	helper := filepath.Join(dir, filepath.Base(exe))
	if err = copyFile(exe, helper); err != nil {
		return true, err
	}
	cmd := exec.Command(helper, "--recover-update", root, strconv.Itoa(os.Getpid()))
	return true, cmd.Start()
}

// transitionDigest records file names, types and bytes before a path transition.
// It lets a replay distinguish restored originals from missing backups.
func transitionDigest(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%d:%s:%d;", len(rel), rel, d.Type())
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%d:%s", len(target), target)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported transition entry: %s", path)
		}
		fmt.Fprintf(h, "%d:", info.Size())
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, f)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
