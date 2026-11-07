//go:build !windows

package storage

import "os"

// makePrivate lets only the current user into dir: the settings hold the
// login's tokens. The files and folders made inside are 0600 and 0700 too.
func makePrivate(dir string) error { return os.Chmod(dir, 0o700) }
