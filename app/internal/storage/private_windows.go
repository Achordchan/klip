//go:build windows

package storage

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// makePrivate makes the current user the owner of dir and gives it a
// protected DACL that lets in only the system, the administrators and the
// current user, as the profile folder Klip leaves does: the settings hold the
// login's tokens, and an empty folder picked on a share or a public drive
// would otherwise pass its own ACL on to them (the 0600 mode of the files
// means nothing to Windows). What is created inside inherits it.
//
// The owner matters as much as the DACL: whoever owns the folder may rewrite
// its DACL at any time. Taking ownership needs the right to (full control
// gives it); a folder whose owner stays somebody else is refused.
func makePrivate(dir string) error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	me := u.User.Sid
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)", me.String()))
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		me, nil, dacl, nil); err != nil {
		return err
	}
	// Read back what Windows has now, not what was asked for.
	got, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := got.Owner()
	if err != nil {
		return err
	}
	if owner == nil || !owner.Equals(me) {
		return errors.New("目录的所有者不是当前用户")
	}
	control, _, err := got.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("目录仍继承上级的访问权限")
	}
	return nil
}
