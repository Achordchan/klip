//go:build linux

package storage

import (
	"errors"

	"golang.org/x/sys/unix"
)

// POSIX ACLs are kept in these extended attributes: the access ACL of dir and
// the default ACL its new files and folders inherit.
var aclAttrs = []string{"system.posix_acl_access", "system.posix_acl_default"}

func absent(err error) bool {
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}

func clearACL(dir string) error {
	for _, a := range aclAttrs {
		if err := unix.Removexattr(dir, a); err != nil && !absent(err) {
			return err
		}
	}
	return nil
}

func hasACL(dir string) (bool, error) {
	for _, a := range aclAttrs {
		if _, err := unix.Getxattr(dir, a, nil); err == nil {
			return true, nil
		} else if !absent(err) {
			return false, err
		}
	}
	return false, nil
}
