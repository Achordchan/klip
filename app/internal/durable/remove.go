package durable

import "os"

// Remove durably retires a name before dependent cleanup proceeds.
func Remove(path string) error {
	// A durable rename to a retired name also covers Windows, where directory
	// flushing is unavailable. A crash may leave only an inert cleanup file.
	retired := path + ".retired"
	if err := Rename(path, retired); err != nil {
		return err
	}
	if err := os.Remove(retired); err != nil && !os.IsNotExist(err) {
		return err
	}
	return SyncParent(path)
}
