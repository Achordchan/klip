//go:build windows

package storage

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// makePrivate gives dir a protected DACL that lets in only the system, the
// administrators and the current user, as the profile folder Klip leaves
// does: the settings hold the login's tokens, and an empty folder picked on a
// share or a public drive would otherwise pass its own ACL on to them (the
// 0600 mode of the files means nothing to Windows). What is created inside
// inherits it.
func makePrivate(dir string) error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", u.User.Sid.String()))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
