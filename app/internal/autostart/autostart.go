// Package autostart starts Klip when the user signs in. On Windows it is the
// registry value of WindowsAutoStartService, under the name "maccy" that
// 1.0.10 and older use, so that turning it on or off in either version acts on
// the same entry and an upgrade keeps what the user chose.
package autostart

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Service turns the start at sign-in on and off.
type Service interface {
	IsEnabled() bool
	SetEnabled(on bool) error
}

// ValueName is the name of the Run value, as in C#.
const ValueName = "maccy"

// RunKey is HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run, as much of
// it as the service needs.
type RunKey interface {
	// Get returns the value, ok false when there is none.
	Get(name string) (value string, ok bool)
	Set(name, value string) error
	Delete(name string) error
}

// Registry is WindowsAutoStartService over a RunKey.
type Registry struct {
	Key RunKey
	// Exe is the program to start (os.Executable when empty).
	Exe string
}

// IsEnabled is true when the value exists and is not blank.
func (r Registry) IsEnabled() bool {
	v, ok := r.Key.Get(ValueName)
	return ok && strings.TrimSpace(v) != ""
}

// SetEnabled writes the value (the quoted path of the program) or removes it.
// Turning it on without knowing the program path does nothing, as in C#.
func (r Registry) SetEnabled(on bool) error {
	if !on {
		return r.Key.Delete(ValueName)
	}
	exe := r.Exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	if strings.TrimSpace(exe) == "" {
		return nil
	}
	return r.Key.Set(ValueName, `"`+exe+`"`)
}

// Desktop is an XDG autostart entry (Linux): a .desktop file in the user's
// autostart folder.
type Desktop struct {
	// Dir is the autostart folder (~/.config/autostart when empty).
	Dir string
	// Exe is the program to start (os.Executable when empty).
	Exe string
}

func (d Desktop) file() (string, error) {
	dir := d.Dir
	if dir == "" {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			base = filepath.Join(home, ".config")
		}
		dir = filepath.Join(base, "autostart")
	}
	return filepath.Join(dir, "klip.desktop"), nil
}

// IsEnabled is true when the entry exists and is not switched off.
func (d Desktop) IsEnabled() bool {
	f, err := d.file()
	if err != nil {
		return false
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return false
	}
	// A startup manager switches an entry off by a line in it, leaving the file.
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.EqualFold(line, "Hidden=true") || strings.EqualFold(line, "X-GNOME-Autostart-enabled=false") {
			return false
		}
	}
	return true
}

// SetEnabled writes or removes the entry.
func (d Desktop) SetEnabled(on bool) error {
	f, err := d.file()
	if err != nil {
		return err
	}
	if !on {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	exe := d.Exe
	if exe == "" {
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		return err
	}
	body := "[Desktop Entry]\nType=Application\nName=Klip\nExec=" + desktopQuote(exe) + "\nX-GNOME-Autostart-enabled=true\n"
	return os.WriteFile(f, []byte(body), 0o644)
}

// desktopQuote quotes a path for the Exec key of a .desktop file. A value
// there goes through two readings: first the escapes of the string value
// (\\ for a backslash), then the quoting rules of Exec (inside quotes a
// backslash makes the next character itself, and " ` $ \ must have one).
// So the Exec-level backslashes are written doubled in the file.
func desktopQuote(p string) string {
	if !strings.ContainsAny(p, " \t\"'\\$`<>~|&;*?#()%") {
		return p
	}
	exec := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`, `%`, `%%`).Replace(p)
	return strings.ReplaceAll(`"`+exec+`"`, `\`, `\\`)
}
