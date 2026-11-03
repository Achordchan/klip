package update

import (
	"os"
	"path/filepath"
	"testing"
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
