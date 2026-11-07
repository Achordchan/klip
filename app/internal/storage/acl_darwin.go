//go:build darwin

package storage

import (
	"fmt"
	"os/exec"
	"strings"
)

// clearACL removes every ACL entry of dir (chmod -N). Entries marked
// file_inherit or directory_inherit would otherwise be copied onto the
// settings file created inside, whatever its mode.
func clearACL(dir string) error {
	if out, err := exec.Command("/bin/chmod", "-N", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("无法清除目录的访问控制列表：%v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// hasACL says whether dir has ACL entries: ls -lde lists them, one per line,
// below the line of the folder itself.
func hasACL(dir string) (bool, error) {
	out, err := exec.Command("/bin/ls", "-lde", dir).Output()
	if err != nil {
		return false, fmt.Errorf("无法读取目录的访问控制列表：%v", err)
	}
	lines := 0
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) != "" {
			lines++
		}
	}
	return lines > 1, nil
}
