package platform

import (
	"runtime"

	"github.com/egoist/mygo"

	"klip/internal/autostart"
)

// Autostart is the start-at-sign-in of this platform: on Windows the Run
// value "maccy" that 1.0.10 uses, on macOS MyGo's login item (which needs the
// packaged app on macOS 13 and later), on Linux an XDG autostart entry.
func Autostart() autostart.Service {
	if runtime.GOOS == "darwin" {
		return loginItem{}
	}
	return autostart.Default()
}

type loginItem struct{}

func (loginItem) IsEnabled() bool { return mygo.App.OpenAtLogin() }

func (loginItem) SetEnabled(on bool) error { return mygo.App.SetOpenAtLogin(on) }
