package update

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMissingTransitionBackupRequiresMatchingOriginal(t *testing.T) {
	for _, restored := range []bool{true, false} {
		t.Run(map[bool]string{true: "restored", false: "missing-backup"}[restored], func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "res")
			backup := filepath.Join(root, "transition")
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "asset"), []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
			digest, err := transitionDigest(target)
			if err != nil {
				t.Fatal(err)
			}
			if !restored {
				if err := os.RemoveAll(target); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("replacement"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := saveRecovery(root, []rollbackItem{{path: target, backup: backup, existed: true, moved: true, originalDigest: digest}, {path: target}}); err != nil {
				t.Fatal(err)
			}
			err = RecoverInterruptedUpdate(root)
			if restored && err != nil {
				t.Fatal(err)
			}
			if !restored {
				if err == nil {
					t.Fatal("missing backup accepted as restored")
				}
				if _, err := os.Stat(recoveryPath(root)); err != nil {
					t.Fatal("evidence removed")
				}
			}
		})
	}
}

func TestLiveRollbackMissingTransitionBackupIsNotSuccess(t *testing.T) {
	r := newRig(t, newPkg, nil)
	target := filepath.Join(r.install, "res")
	write(t, target, "original")
	h := r.hooks()
	copyHook := h.fill().Copy
	h.Copy = func(src, dst string) error {
		if dst == filepath.Join(target, "x.txt") {
			backups, _ := filepath.Glob(filepath.Join(r.install, ".update-transition-*"))
			for _, backup := range backups {
				if err := os.RemoveAll(backup); err != nil {
					t.Fatal(err)
				}
			}
			return errors.New("simulated missing backup")
		}
		return copyHook(src, dst)
	}
	h.Sleep = func(time.Duration) {}
	if err := RunPlan(r.plan, h); err == nil {
		t.Fatal("failed update succeeded")
	}
	if _, err := os.Stat(recoveryPath(r.install)); err != nil {
		t.Fatal("incomplete rollback lost journal")
	}
	if len(r.launched) != 0 {
		t.Fatal("incomplete installation restarted")
	}
}
