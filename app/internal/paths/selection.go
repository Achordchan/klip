package paths

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"klip/internal/durable"
)

// selectionName is the file that says where the data is when it is not in
// the default folder.
const selectionName = "storage-location.json"

// SelectionPath is where the choice of data folder is kept: in a folder of
// its own for the settings of the platform, outside every data folder. Not in
// the default data folder: after a move, that folder is the backup the user
// was told they may delete, and deleting it must not send the program back
// to it with the moved data left unread.
func SelectionPath() (string, error) {
	dir, err := selectionDir()
	return filepath.Join(dir, selectionName), err
}

func selectionDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, "AppData", "Local")
		}
		return filepath.Join(base, "Klip"), nil // the data is in ...\maccy
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Preferences", "Klip"), nil
	}
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "klip"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "klip"), nil
}

// legacySelectionPath is where a build before this one kept the choice: in
// the default data folder. It is still read when the new place has none.
func legacySelectionPath() (string, error) {
	l, err := Default()
	return filepath.Join(l.Root, selectionName), err
}

// Current is the data folder: the one chosen by a move, or the default.
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
		legacy, lerr := legacySelectionPath()
		if lerr != nil {
			return Layout{}, lerr
		}
		b, err = os.ReadFile(legacy)
	}
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
