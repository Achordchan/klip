//go:build !windows

package shelfwin

import "github.com/egoist/mygo/ui"

// SystemIcon has no icons of the system to offer here: the window draws a
// badge with the extension.
func SystemIcon(string) *ui.Bitmap { return nil }
