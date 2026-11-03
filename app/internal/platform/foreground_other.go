//go:build !darwin && !windows

package platform

import "klip/internal/capture"

// ForegroundApp is not available on this platform (C# returns null too
// outside Windows).
func ForegroundApp() *capture.AppIdentity { return nil }
