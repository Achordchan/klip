//go:build !windows

package shelf

// StartHook is not available here (the shelf is a Windows feature).
func StartHook(Events) (func(), error) { return nil, ErrUnsupported }

// ModifierDown is not available here.
func ModifierDown(string) bool { return false }
