// Package version holds the version of the running program. It is set when
// the program is built: go build -ldflags "-X klip/internal/version.Version=1.0.12".
package version

// Version is the version of this build, as the manifest names versions.
var Version = "1.0.11"
