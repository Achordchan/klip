//go:build !windows

package capture

import (
	"os"
	"syscall"
)

// ownedByMe says whether the file belongs to the user running Klip.
func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
