//go:build darwin

package storage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrationClearsInheritedACL(t *testing.T) {
	dst := t.TempDir()
	if out, err := exec.Command("/bin/chmod", "+a", "everyone allow read,list,file_inherit,directory_inherit", dst).CombinedOutput(); err != nil {
		t.Skipf("cannot set an ACL here: %v %s", err, out)
	}
	if has, _ := hasACL(dst); !has {
		t.Fatal("the test ACL is not seen")
	}
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "history.json"), []byte("[]"), 0600)
	os.WriteFile(filepath.Join(src, "settings.json"), []byte(`{"AuthRefreshToken":"secret"}`), 0600)
	if err := Migrate(src, dst, filepath.Join(t.TempDir(), "location.json"), nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dst, filepath.Join(dst, "settings.json")} {
		out, err := exec.Command("/bin/ls", "-lde", p).Output()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "everyone") {
			t.Fatalf("%s still lets everyone in:\n%s", p, out)
		}
	}
}
