//go:build !darwin && !windows

package platform

// ChangeSeq has no cheap source here: -1 makes the Watcher look every tick.
func ChangeSeq() int64 { return -1 }
