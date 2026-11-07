//go:build !windows && !darwin && !linux

package storage

// No ACLs are looked at here: the mode bits are what makePrivate checks.
func clearACL(string) error       { return nil }
func hasACL(string) (bool, error) { return false, nil }
