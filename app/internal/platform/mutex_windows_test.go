//go:build windows

package platform

import (
	"os"
	"os/exec"
	"testing"
)

// The mutex is let go of before the process ends: a program started again
// meanwhile (App.Relaunch after a migration) takes it, as a first instance.
func TestAppMutexIsLetGoBeforeTheProcessEnds(t *testing.T) {
	if os.Getenv("KLIP_MUTEX_CHILD") == "1" {
		if HoldAppMutex() {
			os.Exit(3) // "already running"
		}
		os.Exit(0)
	}
	if HoldAppMutex() {
		t.Skip("a Klip runs on this machine")
	}
	child := func() int {
		cmd := exec.Command(os.Args[0], "-test.run=TestAppMutexIsLetGoBeforeTheProcessEnds")
		cmd.Env = append(os.Environ(), "KLIP_MUTEX_CHILD=1")
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return 0
	}
	if code := child(); code != 3 {
		t.Fatalf("while it is held, a second process is not told so (exit %d)", code)
	}
	ReleaseAppMutex()
	if code := child(); code != 0 {
		t.Fatalf("once let go of, a new process still finds it held (exit %d)", code)
	}
	ReleaseAppMutex() // twice is harmless
}
