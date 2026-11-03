// Package settings reads and writes settings.json.
package settings

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"klip/internal/durable"
	"klip/internal/model"
)

// Load reads the file. A missing or unreadable file gives the defaults, as
// in the C# client; the error says why, for a log. The legacy sync address
// is migrated, and written back when it changed.
func Load(path string) (model.Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.DefaultSettings(), ignoreMissing(err)
	}
	// The file holds the access and the refresh token: tighten what an older
	// version may have left world-readable, even when nothing is saved now.
	_ = os.Chmod(filepath.Dir(path), 0o700)
	_ = os.Chmod(path, 0o600)
	s, err := model.ParseSettings(data)
	if err != nil {
		return model.DefaultSettings(), err
	}
	if s.MigrateLegacySyncBaseUrl() {
		_ = Save(path, s)
	}
	return s, nil
}

// Save writes the file through a temporary one, so that a crash does not
// leave half a file.
func Save(path string, s model.Settings) error {
	s.MigrateLegacySyncBaseUrl()
	// The order is taken before serialization: a slow older save must not
	// enter the write path after a newer one and overwrite it.
	key := path
	if abs, err := filepath.Abs(path); err == nil {
		key = abs
	}
	stateAny, _ := saveStates.LoadOrStore(key, &saveState{})
	state := stateAny.(*saveState)
	seq := saveSeq.Add(1)
	data, err := s.Marshal()
	if err != nil {
		return err
	}
	return writeAtomic(path, data, seq, state)
}

func ignoreMissing(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

type saveState struct {
	mu      sync.Mutex
	written uint64
}

var (
	saveSeq    atomic.Uint64
	saveStates sync.Map // absolute path -> *saveState
)

func writeAtomic(path string, data []byte, seq uint64, state *saveState) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if seq <= state.written {
		return nil // a newer save of these settings already committed
	}
	// The file holds the access and the refresh token: the folder and the
	// file are for this user only, also when an older version created them
	// world-readable.
	_ = os.Chmod(dir, 0o700)
	// One temporary file per save: concurrent saves must not rename each
	// other's file.
	f, err := os.CreateTemp(dir, "settings.json.tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := durable.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	state.written = seq
	// The rename gives the file the mode of the temporary one, but make sure
	// again: the path may be on a file system that keeps the old mode.
	_ = os.Chmod(path, 0o600)
	return nil
}
