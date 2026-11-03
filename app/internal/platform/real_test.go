package platform

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo"

	"klip/internal/capture"
	"klip/internal/history"
	"klip/internal/model"
	"klip/internal/paths"
)

// The tests that touch the real system clipboard replace what is on it, so
// they run only when asked: KLIP_REAL_CLIPBOARD=1. They need a running MyGo
// application, which TestMain provides: it runs the tests on a goroutine once
// the application is ready, and quits when they are done.
var realRun = os.Getenv("KLIP_REAL_CLIPBOARD") == "1"

func init() {
	if realRun {
		runtime.LockOSThread() // the application loop wants the main thread
	}
}

func TestMain(m *testing.M) {
	if !realRun {
		os.Exit(m.Run())
	}
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
	mygo.App.OnWindowAllClosed(func() {})
	code := 1
	mygo.App.WhenReady(func() {
		go func() {
			code = m.Run()
			mygo.App.Quit()
		}()
	})
	if err := mygo.App.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "app:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func requireReal(t *testing.T) {
	t.Helper()
	if !realRun {
		t.Skip("set KLIP_REAL_CLIPBOARD=1 to use the real clipboard")
	}
}

// realRig is the whole capture path on the real clipboard: Watcher, MyGo
// adapter, capture.Service, history.Service.
type realRig struct {
	t    *testing.T
	h    *history.Service
	cap  *capture.Service
	l    paths.Layout
	stop context.CancelFunc
	done chan struct{}
}

func newRealRig(t *testing.T) *realRig {
	requireReal(t)
	// Put the user's text clipboard back afterwards, as far as we can.
	if old, err := tool("pbpaste").Output(); err == nil && runtime.GOOS == "darwin" {
		t.Cleanup(func() {
			c := tool("pbcopy")
			c.Stdin = bytes.NewReader(old)
			_ = c.Run()
		})
	}
	l := paths.Layout{Root: t.TempDir()}
	h := history.NewService(l)
	cs := capture.New(l, h, Clipboard{})
	cs.ForegroundApp = ForegroundApp
	ctx, cancel := context.WithCancel(context.Background())
	w := &Watcher{Interval: 20 * time.Millisecond, OnChange: func() {
		if err := cs.Capture(ctx); err != nil {
			t.Errorf("capture: %v", err)
		}
	}}
	r := &realRig{t: t, h: h, cap: cs, l: l, stop: cancel, done: make(chan struct{})}
	go func() { w.Run(ctx); close(r.done) }()
	time.Sleep(100 * time.Millisecond) // let the watcher record the counter
	t.Cleanup(func() { cancel(); <-r.done })
	return r
}

func (r *realRig) waitLen(n int) []model.Item {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.h.Len() == n {
			return r.h.Items()
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("history has %d items, want %d", r.h.Len(), n)
	return nil
}

// pbcopy and pbpaste convert through the locale, so give them a UTF-8 one.
func tool(name string) *exec.Cmd {
	c := exec.Command(name)
	c.Env = append(os.Environ(), "LC_ALL=en_US.UTF-8")
	return c
}

// pbcopy puts text on the clipboard as another program would; only macOS has
// the tool, elsewhere the adapter itself does it (the other side of each test
// still goes through the real clipboard).
func pbcopy(t *testing.T, s string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		if err := (Clipboard{}).WriteText(s); err != nil {
			t.Fatal(err)
		}
		return
	}
	c := tool("pbcopy")
	c.Stdin = strings.NewReader(s)
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
}

func pbpaste(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return (Clipboard{}).ReadText()
	}
	out, err := tool("pbpaste").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func testPNG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestRealAdapterRoundTrip(t *testing.T) {
	r := newRealRig(t)
	_ = r
	var c Clipboard
	if err := c.WriteText("klip adapter 剪贴板 ✓"); err != nil {
		t.Fatal(err)
	}
	if got := c.ReadText(); got != "klip adapter 剪贴板 ✓" {
		t.Fatalf("ReadText = %q", got)
	}
	if got := pbpaste(t); got != "klip adapter 剪贴板 ✓" {
		t.Fatalf("another program sees %q", got)
	}
	found := false
	for _, f := range c.Formats() {
		if f == "text/plain" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Formats = %v, no text/plain", c.Formats())
	}

	// A picture, offered together with a file.
	dir := t.TempDir()
	pngBytes := testPNG(t, 5, 3, color.RGBA{200, 10, 10, 255})
	fp := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(fp, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteImage(pngBytes, fp); err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(c.ReadImage()))
	if err != nil || img.Bounds().Dx() != 5 || img.Bounds().Dy() != 3 {
		t.Fatalf("image read back: %v %v", img, err)
	}
	files, err := c.ReadFiles()
	if err != nil || len(files) != 1 || !samePath(files[0], fp) {
		t.Fatalf("file offered with the image: %v %v", files, err)
	}

	// Nothing there is not an error.
	pbcopy(t, "just text")
	time.Sleep(50 * time.Millisecond)
	if files, err := c.ReadFiles(); err != nil || len(files) != 0 {
		t.Fatalf("ReadFiles on text = %v %v", files, err)
	}
	if b := c.ReadImage(); len(b) != 0 {
		t.Fatalf("ReadImage on text = %d bytes", len(b))
	}
}

func samePath(a, b string) bool {
	ra, e1 := filepath.EvalSymlinks(a)
	rb, e2 := filepath.EvalSymlinks(b)
	return e1 == nil && e2 == nil && ra == rb
}

func TestRealTextCaptureAndApply(t *testing.T) {
	r := newRealRig(t)

	pbcopy(t, "klip real one")
	items := r.waitLen(1)
	if model.Str(items[0].Text) != "klip real one" || items[0].Kind != model.KindText {
		t.Fatalf("first capture = %+v", items[0])
	}
	if runtime.GOOS == "darwin" && (items[0].SourceAppName == nil || *items[0].SourceAppName == "") {
		t.Fatalf("no source app on macOS: %+v", items[0])
	}

	pbcopy(t, "klip real two")
	items = r.waitLen(2)
	if model.Str(items[0].Text) != "klip real two" {
		t.Fatalf("newest first expected: %q", model.Str(items[0].Text))
	}

	// Copying the same text again moves it, it does not add.
	pbcopy(t, "klip real one")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if it := r.h.Items(); len(it) == 2 && model.Str(it[0].Text) == "klip real one" && it[0].CopyCount == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if it := r.h.Items(); len(it) != 2 || model.Str(it[0].Text) != "klip real one" || it[0].CopyCount != 2 {
		t.Fatalf("duplicate not merged: %+v", it)
	}

	// Applying an item writes the clipboard but must not capture it back.
	second := r.h.Items()[1] // "klip real two"
	if err := r.cap.Apply(second); err != nil {
		t.Fatal(err)
	}
	if got := pbpaste(t); got != "klip real two" {
		t.Fatalf("clipboard after apply = %q", got)
	}
	time.Sleep(600 * time.Millisecond)
	it := r.h.Items()
	if len(it) != 2 || model.Str(it[0].Text) != "klip real one" || it[1].CopyCount != 1 {
		t.Fatalf("our own write was captured: %+v", it)
	}
}

func TestRealImageAndFileCapture(t *testing.T) {
	r := newRealRig(t)
	var c Clipboard

	pngBytes := testPNG(t, 8, 6, color.RGBA{10, 200, 10, 255})
	if err := c.WriteImage(pngBytes, ""); err != nil {
		t.Fatal(err)
	}
	items := r.waitLen(1)
	if items[0].Kind != model.KindImage || model.Str(items[0].ImageFilePath) == "" {
		t.Fatalf("image capture = %+v", items[0])
	}
	if _, err := os.Stat(*items[0].ImageFilePath); err != nil {
		t.Fatalf("image file missing: %v", err)
	}

	dir := t.TempDir()
	doc := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(doc, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteFiles([]string{doc}); err != nil {
		t.Fatal(err)
	}
	items = r.waitLen(2)
	if items[0].Kind != model.KindFileList || len(items[0].FilePaths) != 1 || !samePath(items[0].FilePaths[0], doc) {
		t.Fatalf("file capture = %+v", items[0])
	}
	cached := filepath.Join(r.l.Files(), items[0].IDN(), "0000_note.txt")
	if b, err := os.ReadFile(cached); err != nil || string(b) != "hello" {
		t.Fatalf("cache copy %s: %q %v", cached, b, err)
	}

	// Apply both back and read what another reader gets.
	if err := r.cap.Apply(items[1]); err != nil {
		t.Fatal(err)
	}
	if img, _, err := image.Decode(bytes.NewReader(c.ReadImage())); err != nil || img.Bounds().Dx() != 8 {
		t.Fatalf("applied image: %v %v", img, err)
	}
	if err := r.cap.Apply(items[0]); err != nil {
		t.Fatal(err)
	}
	if files, err := c.ReadFiles(); err != nil || len(files) != 1 {
		t.Fatalf("applied files: %v %v", files, err)
	}
}

func TestRealForegroundApp(t *testing.T) {
	requireReal(t)
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("no foreground app on this platform")
	}
	app := ForegroundApp()
	if app == nil || app.Name == "" {
		t.Fatalf("ForegroundApp = %+v", app)
	}
	t.Logf("foreground: %+v", app)
}

func TestRealChangeSeq(t *testing.T) {
	requireReal(t)
	if ChangeSeq() < 0 {
		t.Skip("no change counter here")
	}
	a := ChangeSeq()
	if b := ChangeSeq(); b != a {
		t.Fatalf("counter moved without a copy: %d -> %d", a, b)
	}
	pbcopy(t, fmt.Sprint("seq ", time.Now().UnixNano()))
	time.Sleep(50 * time.Millisecond)
	if b := ChangeSeq(); b == a {
		t.Fatal("counter did not move after a copy")
	}
}
