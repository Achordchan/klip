package paths

import (
	"encoding/json"
	"errors"
	"klip/internal/durable"
	"os"
	"path/filepath"
)

// SelectionPath stays in the platform default folder, not in the movable data.
func SelectionPath() (string, error) {
	l, err := Default()
	return filepath.Join(l.Root, "storage-location.json"), err
}
func Current() (Layout, error) {
	def, err := Default()
	if err != nil {
		return Layout{}, err
	}
	p, err := SelectionPath()
	if err != nil {
		return Layout{}, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return def, nil
	}
	if err != nil {
		return Layout{}, err
	}
	var v struct{ Root string }
	if err = json.Unmarshal(b, &v); err != nil {
		return Layout{}, err
	}
	if !filepath.IsAbs(v.Root) {
		return Layout{}, errors.New("invalid storage location")
	}
	st, err := os.Stat(v.Root)
	if err != nil {
		return Layout{}, err
	}
	if !st.IsDir() {
		return Layout{}, errors.New("storage location is not a directory")
	}
	return Layout{Root: filepath.Clean(v.Root)}, nil
}
func SaveSelection(path, root string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(struct{ Root string }{root})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".storage-location-*")
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
