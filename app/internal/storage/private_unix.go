//go:build !windows

package storage

import (
	"errors"
	"os"
	"syscall"
)

// makePrivate lets only the current user into dir: the settings hold the
// login's tokens. The mode bits alone are not enough where the folder carries
// an ACL (inherited from its parent, or set on it): clearACL removes it, and
// the result is checked before anything is copied. A folder that cannot be
// made private is refused.
func makePrivate(dir string) error {
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if err := clearACL(dir); err != nil {
		return err
	}
	return checkPrivate(dir)
}

func checkPrivate(dir string) error {
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st.Mode().Perm() != 0o700 {
		return errors.New("目录权限未能设为仅当前用户可访问")
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return errors.New("目录不属于当前用户")
	}
	has, err := hasACL(dir)
	if err != nil {
		return err
	}
	if has {
		return errors.New("目录带有访问控制列表（ACL），无法确认仅当前用户可访问")
	}
	return nil
}
