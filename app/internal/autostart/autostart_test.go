package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeKey map[string]string

func (k fakeKey) Get(n string) (string, bool) { v, ok := k[n]; return v, ok }
func (k fakeKey) Set(n, v string) error       { k[n] = v; return nil }
func (k fakeKey) Delete(n string) error       { delete(k, n); return nil }

func TestRegistryUsesTheNameOfOldVersions(t *testing.T) {
	k := fakeKey{}
	r := Registry{Key: k, Exe: `C:\Users\me\AppData\Local\Programs\maccy\Klip.exe`}
	if r.IsEnabled() {
		t.Fatal("empty key is not enabled")
	}
	if err := r.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	// Byte for byte what 1.0.10 writes: value "maccy", the path in quotes.
	if got, ok := k["maccy"]; !ok || got != `"C:\Users\me\AppData\Local\Programs\maccy\Klip.exe"` || len(k) != 1 {
		t.Fatalf("registry: %#v", k)
	}
	if !r.IsEnabled() {
		t.Fatal("not enabled after SetEnabled(true)")
	}
	if err := r.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if len(k) != 0 || r.IsEnabled() {
		t.Fatalf("not removed: %#v", k)
	}
	if err := r.SetEnabled(false); err != nil {
		t.Fatalf("removing what is not there: %v", err)
	}
}

func TestRegistryReadsWhatOldVersionsWrote(t *testing.T) {
	r := Registry{Key: fakeKey{"maccy": `"C:\old\maccy.exe"`}}
	if !r.IsEnabled() {
		t.Fatal("1.0.10's entry must count")
	}
	for _, v := range []string{"", "   "} {
		if (Registry{Key: fakeKey{"maccy": v}}).IsEnabled() {
			t.Fatalf("blank value %q counts as enabled", v)
		}
	}
	if (Registry{Key: fakeKey{"Klip": `"x"`}}).IsEnabled() {
		t.Fatal("another value name must not count")
	}
}

func TestRegistryDefaultsToThisProgram(t *testing.T) {
	k := fakeKey{}
	if err := (Registry{Key: k}).SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if k["maccy"] != `"`+exe+`"` {
		t.Fatalf("got %q, want the test binary %q", k["maccy"], exe)
	}
}

func TestDesktop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "autostart")
	d := Desktop{Dir: dir, Exe: "/opt/my apps/klip"}
	if d.IsEnabled() {
		t.Fatal("enabled before")
	}
	if err := d.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "klip.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[Desktop Entry]", "Type=Application", `Exec="/opt/my apps/klip"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %q in\n%s", want, data)
		}
	}
	if !d.IsEnabled() {
		t.Fatal("not enabled after")
	}
	if err := d.SetEnabled(false); err != nil || d.IsEnabled() {
		t.Fatalf("not removed: %v", err)
	}
	if err := d.SetEnabled(false); err != nil {
		t.Fatalf("removing twice: %v", err)
	}
	if got := desktopQuote("/usr/bin/klip"); got != "/usr/bin/klip" {
		t.Fatalf("plain path quoted: %q", got)
	}
}

func TestDesktopQuoteEscapesPercent(t *testing.T) {
	if got := desktopQuote("/opt/klip%20/klip"); got != `"/opt/klip%%20/klip"` {
		t.Fatalf("a percent is a field code in an Exec line: %q", got)
	}
}

func TestDesktopQuoteSurvivesBothReadings(t *testing.T) {
	for in, want := range map[string]string{
		"/opt/$apps/klip": `"/opt/\\$apps/klip"`,
		"/opt/a`b/klip":   "\"/opt/a\\\\`b/klip\"",
		"/opt/my apps/k":  `"/opt/my apps/k"`,
		`/opt/a"b/klip`:   `"/opt/a\\"b/klip"`,
	} {
		if got := desktopQuote(in); got != want {
			t.Errorf("desktopQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestSwitchedOffEntryIsNotEnabled(t *testing.T) {
	dir := t.TempDir()
	d := Desktop{Dir: dir, Exe: "/usr/bin/klip"}
	if err := d.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if !d.IsEnabled() {
		t.Fatal("a fresh entry is on")
	}
	f, _ := d.file()
	b, _ := os.ReadFile(f)
	_ = os.WriteFile(f, append(b, []byte("Hidden=true\n")...), 0o644) // as a startup manager does it
	if d.IsEnabled() {
		t.Fatal("an entry marked Hidden is off")
	}
}

func TestRepairPointsTheOldEntryAtThisProgram(t *testing.T) {
	exe := `C:\Users\me\AppData\Local\Klip\Klip.exe`
	k := fakeKey{"maccy": `"C:\Users\me\AppData\Local\Klip\maccy.exe"`}
	if err := (Registry{Key: k, Exe: exe}).Repair(); err != nil {
		t.Fatal(err)
	}
	if k["maccy"] != `"`+exe+`"` {
		t.Fatalf("value %q", k["maccy"])
	}
	// The same folder in another spelling (case, slashes) is the same.
	k = fakeKey{"maccy": `"c:/users/me/appdata/local/klip/MACCY.EXE"`}
	if err := (Registry{Key: k, Exe: exe}).Repair(); err != nil || k["maccy"] != `"`+exe+`"` {
		t.Fatalf("value %q (%v)", k["maccy"], err)
	}
	// Any entry that is not the old program file stays: this one, in another
	// spelling of the case, the Klip.exe the user installed while this is a
	// build run from a temporary folder (go run), another program the user
	// put there.
	for _, v := range []string{
		`"c:\users\me\appdata\local\klip\klip.exe"`,
		`"C:\Users\me\AppData\Local\Klip\Klip.exe"`,
		`"D:\tools\other.exe"`,
		// The C# program of another folder: an installation that is still
		// there, while this is a build of go run.
		`"C:\Users\me\AppData\Local\Klip\maccy.exe"`,
		`"maccy.exe"`,
	} {
		k := fakeKey{"maccy": v}
		if err := (Registry{Key: k, Exe: `C:\Users\me\AppData\Local\Temp\go-build1\exe\klip.exe`}).Repair(); err != nil || k["maccy"] != v {
			t.Fatalf("%s rewritten to %q (%v)", v, k["maccy"], err)
		}
	}
	// No entry, or a blank one: the user did not ask to start at sign-in.
	for _, k := range []fakeKey{{}, {"maccy": "  "}} {
		before := len(k)
		if err := (Registry{Key: k, Exe: exe}).Repair(); err != nil || len(k) != before || strings.TrimSpace(k["maccy"]) != "" {
			t.Fatalf("an entry was made: %v %v", k, err)
		}
	}
}
