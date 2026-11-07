//go:build windows

package capture

import "os"

// ownedByMe: the clipboard folder is under the user's own profile
// (%LocalAppData%), which other users cannot write to.
func ownedByMe(os.FileInfo) bool { return true }
