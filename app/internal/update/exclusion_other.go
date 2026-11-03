//go:build !windows

package update

func holdUpdateExclusion() (func(), error) { return func() {}, nil }
func UpdateInProgress() bool               { return false }
