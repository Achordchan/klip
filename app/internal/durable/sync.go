package durable

import (
	"io/fs"
	"os"
	"path/filepath"
)

// SyncTree flushes all staged file contents before publication.
func SyncTree(path string) error {
	var dirs []string
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncDirectory(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// SyncParent persists the directory entry of a newly written file.
func SyncParent(path string) error { return syncDirectory(filepath.Dir(path)) }
