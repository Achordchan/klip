package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/image/bmp"

	"klip/internal/history"
	"klip/internal/model"
	"klip/internal/paths"
)

type fakeClip struct {
	formats []string
	text    string
	files   []string
	image   []byte

	wroteText  *string
	wroteFiles []string
	wroteImage []byte
	wroteWith  string
	reads      int
	filesErr   error
	onRead     func() // runs inside ReadText, as a write that comes during a read
}

func (c *fakeClip) Formats() []string { return c.formats }
func (c *fakeClip) ReadText() string {
	c.reads++
	if c.onRead != nil {
		c.onRead()
	}
	return c.text
}
func (c *fakeClip) ReadFiles() ([]string, error) { c.reads++; return c.files, c.filesErr }
func (c *fakeClip) ReadImage() []byte            { c.reads++; return c.image }
func (c *fakeClip) WriteText(s string) error     { c.wroteText = &s; return nil }
func (c *fakeClip) WriteFiles(p []string) error  { c.wroteFiles = p; return nil }
func (c *fakeClip) WriteImage(b []byte, f string) error {
	c.wroteImage, c.wroteWith = b, f
	return nil
}

type rig struct {
	t    *testing.T
	l    paths.Layout
	h    *history.Service
	clip *fakeClip
	s    *Service
	now  time.Time
	n    int
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, l: paths.Layout{Root: t.TempDir()}, clip: &fakeClip{}, now: time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)}
	r.h = history.NewService(r.l)
	r.s = New(r.l, r.h, r.clip)
	r.s.Now = func() time.Time { return r.now }
	r.s.NewID = func() string {
		r.n++
		return "00000000-0000-0000-0000-0000000000" + string(rune('0'+r.n/10)) + string(rune('0'+r.n%10))
	}
	return r
}

func (r *rig) capture() {
	r.t.Helper()
	r.now = r.now.Add(time.Second)
	if err := r.s.Capture(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func upperSHA(b []byte) string {
	s := sha256.Sum256(b)
	return strings.ToUpper(hex.EncodeToString(s[:]))
}

func tinyPNG(t *testing.T) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		img.Set(x, x, color.RGBA{200, 10, 10, 255})
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func write(t *testing.T, p string, data []byte) string {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTextCapture(t *testing.T) {
	r := newRig(t)
	r.s.ForegroundApp = func() *AppIdentity { return &AppIdentity{"Notepad", `C:\Windows\notepad.exe`} }
	r.clip.formats, r.clip.text = []string{"text/plain"}, "你好, Klip 😀"
	r.capture()

	items := r.h.Items()
	if len(items) != 1 {
		t.Fatalf("%d items", len(items))
	}
	it := items[0]
	want := "text:" + upperSHA([]byte("你好, Klip 😀"))
	if it.Kind != model.KindText || model.Str(it.Text) != "你好, Klip 😀" || model.Str(it.ContentHash) != want ||
		it.ApproxBytes != int64(model.UTF16Len("你好, Klip 😀"))*2 || it.CopyCount != 1 || it.Pinned {
		t.Errorf("item %+v", it)
	}
	if it.ApproxBytes != 22 { // 11 UTF-16 units: the emoji is two
		t.Errorf("size %d", it.ApproxBytes)
	}
	if !it.CapturedAt.Equal(r.now) || it.FirstCapturedAt == nil || !it.FirstCapturedAt.Equal(r.now) {
		t.Errorf("times %+v", it)
	}
	if model.Str(it.SourceAppName) != "Notepad" || model.Str(it.SourceAppPath) != `C:\Windows\notepad.exe` {
		t.Errorf("source %+v", it)
	}

	// The same text again, on top: nothing is added or touched.
	r.capture()
	if got := r.h.Items(); len(got) != 1 || got[0].CopyCount != 1 {
		t.Errorf("the same text again: %+v", got)
	}
	// Other text, then the first again: that is a copy of an older item.
	r.clip.text = "other"
	r.capture()
	r.clip.text = "你好, Klip 😀"
	r.capture()
	if got := r.h.Items(); len(got) != 2 || got[0].CopyCount != 2 {
		t.Errorf("older duplicate: %d items, %+v", len(got), got[0])
	}
	// With merging off the same text on top is added again.
	lim := r.h.Limits()
	lim.MergeDuplicates = false
	r.h.SetLimits(lim)
	r.capture()
	if r.h.Len() != 3 {
		t.Errorf("%d items", r.h.Len())
	}
}

func TestTextCaptureOffAndBlank(t *testing.T) {
	r := newRig(t)
	r.clip.formats, r.clip.text = []string{"text/plain"}, "x"
	r.s.CaptureText = false
	r.capture()
	if r.h.Len() != 0 || r.clip.reads > 1 { // the one read is the try as a picture
		t.Errorf("text capture is off: %d items, %d reads", r.h.Len(), r.clip.reads)
	}
	r.s.CaptureText = true
	r.clip.text = " \n\t"
	r.capture()
	if r.h.Len() != 0 {
		t.Error("blank text is nothing")
	}
}

func TestOwnWritesAreNotCaptured(t *testing.T) {
	r := newRig(t)
	r.clip.formats, r.clip.text = []string{"text/plain"}, "from us"
	r.s.MarkWriteByUs()
	r.now = r.now.Add(100 * time.Millisecond)
	if err := r.s.Capture(context.Background()); err != nil || r.h.Len() != 0 {
		t.Errorf("within the window: %d items", r.h.Len())
	}
	r.now = r.now.Add(700 * time.Millisecond)
	r.s.Capture(context.Background())
	if r.h.Len() != 1 {
		t.Error("after the window the change counts")
	}
}

func TestFailedApplyIsNotMarkedAsOurs(t *testing.T) {
	r := newRig(t)
	r.s.Seq = func() int64 { return 7 }
	r.clip.formats, r.clip.text = []string{"text/plain"}, "from elsewhere"
	// An image whose file is gone writes nothing, so nothing is ours.
	it := model.Item{Kind: model.KindImage, ImageFilePath: model.Ptr(filepath.Join(r.l.Images(), "gone.png"))}
	if err := r.s.Apply(it); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(time.Second)
	r.s.Capture(context.Background())
	if r.h.Len() != 1 {
		t.Fatal("a copy from elsewhere must be kept after an Apply that wrote nothing")
	}
}

func TestOwnWriteIsFoundByCounter(t *testing.T) {
	r := newRig(t)
	seq := int64(10)
	r.s.Seq = func() int64 { return seq }
	r.clip.formats, r.clip.text = []string{"text/plain"}, "from us"
	if err := r.s.Apply(model.Item{Kind: model.KindText, Text: model.Ptr("from us")}); err != nil {
		t.Fatal(err)
	}
	seq = 11 // our write moved the counter
	r.s.ownSeq.Store(seq + 2)
	r.now = r.now.Add(time.Second) // a slow poll: far past the 250 ms
	r.s.Capture(context.Background())
	if r.h.Len() != 0 {
		t.Fatal("our own change, seen late, must not be captured")
	}
	seq = 12                                              // somebody else copied
	r.now = r.now.Add(-time.Second + 10*time.Millisecond) // right after our write
	r.s.Capture(context.Background())
	if r.h.Len() != 1 {
		t.Fatal("a change after ours counts at once, even inside the window")
	}
}

func TestImageCapture(t *testing.T) {
	r := newRig(t)
	data := tinyPNG(t)
	r.clip.formats, r.clip.image = []string{"image/png"}, data
	r.capture()
	it := r.h.Items()[0]
	p := model.Str(it.ImageFilePath)
	if it.Kind != model.KindImage || filepath.Dir(p) != r.l.Images() || !regexp.MustCompile(`^\d+_[0-9a-f]{32}\.png$`).MatchString(filepath.Base(p)) {
		t.Fatalf("item %+v", it)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, data) {
		t.Error("a PNG is stored as it came")
	}
	if model.Str(it.ContentHash) != "img:"+upperSHA(data) || it.ApproxBytes != int64(len(data)) || it.Text != nil {
		t.Errorf("item %+v", it)
	}
	// The same picture again is a copy of that item, and its second file goes.
	r.capture()
	if r.h.Len() != 1 || r.h.Items()[0].CopyCount != 2 {
		t.Errorf("duplicate image: %d items", r.h.Len())
	}
	if ents, _ := os.ReadDir(r.l.Images()); len(ents) != 1 {
		t.Errorf("%d image files", len(ents))
	}

	// A format that is not PNG is converted; garbage is nothing.
	var b bytes.Buffer
	bmp.Encode(&b, image.NewRGBA(image.Rect(0, 0, 3, 3)))
	r2 := newRig(t)
	r2.clip.formats, r2.clip.image = []string{"image/bmp"}, b.Bytes()
	r2.capture()
	got, _ := os.ReadFile(model.Str(r2.h.Items()[0].ImageFilePath))
	if _, err := png.Decode(bytes.NewReader(got)); err != nil {
		t.Errorf("the stored image is not a PNG: %v", err)
	}
	r3 := newRig(t)
	r3.clip.formats, r3.clip.image = []string{"image/png"}, []byte("not an image")
	r3.capture()
	if r3.h.Len() != 0 {
		t.Error("garbage is not captured")
	}
	// Odd format names (screenshot tools): the image is still tried, when
	// there is no text or file.
	r4 := newRig(t)
	r4.clip.formats, r4.clip.image = []string{"CF_DIBV5"}, data
	r4.capture()
	if r4.h.Len() != 1 {
		t.Error("an image under an odd name")
	}
	r5 := newRig(t)
	r5.clip.formats, r5.clip.image, r5.clip.text = []string{"text/html", "text/plain"}, data, " "
	r5.capture() // text is blank: what is left is tried as a picture, whatever the formats say
	if r5.h.Len() != 1 {
		t.Error("an image beside blank text is kept")
	}
	// A picture that is cut off after its header is not a picture.
	r6 := newRig(t)
	r6.clip.formats, r6.clip.image = []string{"image/png"}, data[:len(data)/2]
	r6.capture()
	if r6.h.Len() != 0 {
		t.Error("a truncated PNG must not be kept")
	}
	r5 = newRig(t)
	r5.clip.formats, r5.clip.image, r5.clip.text = []string{"text/html", "text/plain"}, data, " "
	r5.s.CaptureImages = false
	r4.s.CaptureImages = false
	r4.capture()
	if r4.h.Len() != 1 {
		t.Error("images off: nothing new")
	}
}

func TestFileCapture(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	good := write(t, filepath.Join(dir, "report.pdf"), []byte("pdf-bytes"))
	other := write(t, filepath.Join(dir, "notes.TXT"), []byte("hello"))
	exe := write(t, filepath.Join(dir, "setup.exe"), []byte("MZ"))
	big := write(t, filepath.Join(dir, "big.zip"), bytes.Repeat([]byte("z"), 2000))
	noext := write(t, filepath.Join(dir, "README"), []byte("r"))
	sub := filepath.Join(dir, "folder.pdf")
	os.MkdirAll(sub, 0o755)

	r.clip.formats = []string{"text/uri-list"}
	r.clip.files = []string{good, sub, exe, other, big, noext, "  "}
	r.s.CaptureFileMaxBytes = 1000
	r.s.SetFileExtensions(".pdf,txt; zip")
	r.capture()

	items := r.h.Items()
	if len(items) != 1 {
		t.Fatalf("%d items", len(items))
	}
	it := items[0]
	// Kept: the pdf and the txt (extensions are case-insensitive, the dot is
	// optional). Gone: the folder, the exe, the zip over 1000 bytes, the file
	// without an extension, the blank.
	if it.Kind != model.KindFileList || strings.Join(it.FilePaths, "|") != good+"|"+other {
		t.Fatalf("paths %v", it.FilePaths)
	}
	if model.Str(it.ContentHash) != "files:"+upperSHA([]byte(good+"\n"+other)) {
		t.Errorf("hash %s", model.Str(it.ContentHash))
	}
	cache := filepath.Join(r.l.Files(), it.IDN())
	for i, name := range []string{"report.pdf", "notes.TXT"} {
		if b, err := os.ReadFile(filepath.Join(cache, "000"+string(rune('0'+i))+"_"+name)); err != nil || len(b) == 0 {
			t.Errorf("cached copy %s: %v", name, err)
		}
	}
	if it.ApproxBytes != int64(len("pdf-bytes")+len("hello")) {
		t.Errorf("size %d", it.ApproxBytes)
	}

	// Nothing allowed is nothing captured, and no cache is made.
	r2 := newRig(t)
	r2.clip.formats, r2.clip.files = []string{"text/uri-list"}, []string{exe}
	r2.capture()
	if r2.h.Len() != 0 {
		t.Error("only a dangerous file")
	}
	// An empty list is every extension but the dangerous ones.
	r3 := newRig(t)
	r3.clip.formats, r3.clip.files = []string{"text/uri-list"}, []string{noext, exe, other}
	r3.capture()
	if got := r3.h.Items()[0].FilePaths; strings.Join(got, "|") != noext+"|"+other {
		t.Errorf("any extension: %v", got)
	}
	// A dangerous extension in the list is ignored.
	r4 := newRig(t)
	r4.s.SetFileExtensions("exe, .pdf")
	r4.clip.formats, r4.clip.files = []string{"text/uri-list"}, []string{exe, good}
	r4.capture()
	if got := r4.h.Items()[0].FilePaths; len(got) != 1 || got[0] != good {
		t.Errorf("%v", got)
	}
	// Missing files must not publish an unusable capture.
	r5 := newRig(t)
	r5.clip.formats, r5.clip.files = []string{"text/uri-list"}, []string{filepath.Join(dir, "ghost.txt")}
	if err := r5.s.Capture(context.Background()); err == nil {
		t.Fatal("missing source must report storage failure")
	}
	if got := r5.h.Items(); len(got) != 0 {
		t.Errorf("ghost: %+v", got)
	}
}

func TestSingleImageFileBecomesAnImage(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var jb bytes.Buffer
	jpeg.Encode(&jb, img, nil)
	jpg := write(t, filepath.Join(dir, "photo.JPG"), jb.Bytes())
	r.clip.formats, r.clip.files = []string{"text/uri-list"}, []string{jpg}
	r.capture()
	it := r.h.Items()[0]
	if it.Kind != model.KindImage {
		t.Fatalf("kind %v", it.Kind)
	}
	got, _ := os.ReadFile(model.Str(it.ImageFilePath))
	if _, err := png.Decode(bytes.NewReader(got)); err != nil {
		t.Errorf("not a PNG: %v", err)
	}
	if !strings.HasPrefix(model.Str(it.ContentHash), "img:") || filepath.Dir(model.Str(it.ImageFilePath)) != r.l.Images() {
		t.Errorf("%+v", it)
	}

	// An image file that cannot be read is an ordinary file.
	r2 := newRig(t)
	bad := write(t, filepath.Join(dir, "broken.png"), []byte("not a png"))
	r2.clip.formats, r2.clip.files = []string{"text/uri-list"}, []string{bad}
	r2.s.SetFileExtensions("png")
	r2.capture()
	if got := r2.h.Items(); len(got) != 1 || got[0].Kind != model.KindFileList {
		t.Errorf("broken image: %+v", got)
	}
	// With images off, it is a file.
	r3 := newRig(t)
	r3.s.CaptureImages = false
	r3.clip.formats, r3.clip.files = []string{"text/uri-list"}, []string{jpg}
	r3.s.SetFileExtensions("jpg")
	r3.capture()
	if got := r3.h.Items(); len(got) != 1 || got[0].Kind != model.KindFileList {
		t.Errorf("images off: %+v", got)
	}
}

func TestTextWinsOverFilesAndImages(t *testing.T) {
	r := newRig(t)
	r.clip.formats = []string{"text/plain", "image/png", "text/uri-list"}
	r.clip.text, r.clip.image, r.clip.files = "words", tinyPNG(t), []string{"/x"}
	r.capture()
	if got := r.h.Items(); len(got) != 1 || got[0].Kind != model.KindText {
		t.Errorf("%+v", got)
	}
}

func TestApply(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "a.txt"), []byte("src-a"))
	srcB := write(t, filepath.Join(dir, "b.txt"), []byte("src-b"))
	id := "16fd2706-8baf-433b-82eb-8c7fada847da"
	cached := write(t, filepath.Join(r.l.Files(), strings.ReplaceAll(id, "-", ""), "0000_a.txt"), []byte("cached-a"))

	text := "hello"
	r.clip.formats, r.clip.text = []string{"text/plain"}, "hello" // what the clipboard reads back
	if err := r.s.Apply(model.Item{Kind: model.KindText, Text: &text}); err != nil || r.clip.wroteText == nil || *r.clip.wroteText != "hello" {
		t.Errorf("text: %v", err)
	}
	// Writing is marked, so the change is not captured back.
	r.clip.formats, r.clip.text = []string{"text/plain"}, "hello"
	r.now = r.now.Add(100 * time.Millisecond)
	r.s.Capture(context.Background())
	if r.h.Len() != 0 {
		t.Error("our own write was captured")
	}

	// Files: the cached copy is preferred, the source is the fallback, what is
	// in neither is left out.
	it := model.Item{ID: id, Kind: model.KindFileList, FilePaths: []string{src, srcB, filepath.Join(dir, "gone.txt")}}
	if err := r.s.Apply(it); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.clip.wroteFiles, "|") != cached+"|"+srcB {
		t.Errorf("files %v", r.clip.wroteFiles)
	}
	r.clip.wroteFiles = nil
	r.s.Apply(model.Item{ID: id, Kind: model.KindFileList, FilePaths: []string{filepath.Join(dir, "gone.txt")}})
	if r.clip.wroteFiles != nil {
		t.Error("nothing to write when nothing is left")
	}

	// Image: the PNG and the file it came from.
	png := tinyPNG(t)
	ip := write(t, filepath.Join(r.l.Images(), "x.png"), png)
	if err := r.s.Apply(model.Item{Kind: model.KindImage, ImageFilePath: &ip}); err != nil || !bytes.Equal(r.clip.wroteImage, png) || r.clip.wroteWith != ip {
		t.Errorf("image: %v", err)
	}
	r.clip.wroteImage = nil
	gone := filepath.Join(r.l.Images(), "gone.png")
	r.s.Apply(model.Item{Kind: model.KindImage, ImageFilePath: &gone})
	if r.clip.wroteImage != nil {
		t.Error("a missing image is not written")
	}
	if err := r.s.Apply(model.Item{Kind: 9}); err == nil {
		t.Error("an unknown kind")
	}
}

func TestApplySettings(t *testing.T) {
	r := newRig(t)
	st := model.DefaultSettings()
	st.CaptureText, st.CaptureImages, st.CaptureFiles = false, false, true
	st.CaptureFileExtensions = ".pdf"
	st.CaptureFileMaxMegabytes, st.MaxMegabytes = 50, 10 // the smaller one counts
	r.s.ApplySettings(st)
	if r.s.CaptureText || r.s.CaptureImages || !r.s.CaptureFiles || r.s.CaptureFileMaxBytes != 10*1024*1024 || !r.s.allowed[".pdf"] || len(r.s.allowed) != 1 {
		t.Errorf("%+v", r.s)
	}
}

// What is the same in C#: the list of types that are never captured, the
// hash prefixes, the order in which the formats are tried.
func TestMatchesCSharpSource(t *testing.T) {
	b, err := os.ReadFile("../../../klip/Services/ClipboardCaptureService.cs")
	if err != nil {
		t.Skipf("C# source not available: %v", err)
	}
	src := string(b)
	set := src[strings.Index(src, "DangerousFileExtensions = new"):]
	set = set[:strings.Index(set, "};")]
	var cs []string
	for _, m := range regexp.MustCompile(`"(\.[a-z0-9]+)"`).FindAllStringSubmatch(set, -1) {
		cs = append(cs, m[1])
	}
	if len(cs) != len(dangerousExtensions) {
		t.Errorf("C# lists %d dangerous extensions, Go %d", len(cs), len(dangerousExtensions))
	}
	for _, e := range cs {
		if !dangerousExtensions[e] {
			t.Errorf("%s is dangerous in C# only", e)
		}
	}
	for _, want := range []string{`ComputeHash("text", text)`, `ComputeHash("files", string.Join("\n", paths))`, `ComputeFileHash("img", path)`,
		`text.Length * 2`, `TimeSpan.FromMilliseconds(250)`, `Convert.ToHexString(hash)`, `i.ToString("D4") + "_" + name`} {
		if !strings.Contains(src, want) {
			t.Errorf("C# no longer has %s", want)
		}
	}
	// Text is tried first, then files, then images.
	text, files, images := strings.Index(src, "if (CaptureText &&"), strings.Index(src, "if (CaptureFiles)"), strings.Index(src, "if (CaptureImages)")
	if !(text > 0 && text < files && files < images) {
		t.Error("the order of the kinds changed in C#")
	}
}

func TestSpecialFilesAreNotCopied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no device files")
	}
	if isFile("/dev/null") {
		t.Error("a device is not a file")
	}
	if err := copyFile("/dev/zero", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("a device must not be copied")
	}
}

func TestApplyDuringCaptureIsNotCaptured(t *testing.T) {
	r := newRig(t)
	r.clip.formats, r.clip.text = []string{"text/plain"}, "applied"
	r.clip.onRead = func() { // Apply runs while Capture is reading
		r.clip.onRead = nil
		_ = r.s.Apply(model.Item{Kind: model.KindText, Text: model.Ptr("applied")})
	}
	r.s.Capture(context.Background())
	if r.h.Len() != 0 {
		t.Fatal("what Apply wrote during the read must not become an item")
	}
}

func TestApplyThatLostTheClipboardIsNotOurs(t *testing.T) {
	r := newRig(t)
	r.clip.formats, r.clip.text = []string{"text/plain"}, "somebody else"
	text := "ours"
	if err := r.s.Apply(model.Item{Kind: model.KindText, Text: &text}); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(10 * time.Millisecond)
	r.s.Capture(context.Background())
	if r.h.Len() != 1 {
		t.Fatal("a copy that replaced our write must be captured, window or not")
	}
}

func TestSamePicture(t *testing.T) {
	a := tinyPNG(t)
	if !samePicture(a, a) {
		t.Error("the same picture")
	}
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		img.Set(3-x, x, color.RGBA{10, 10, 200, 255})
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	if samePicture(a, b.Bytes()) {
		t.Error("another picture of the same size is not the same")
	}
}

func TestChangeDuringApplyIsRetried(t *testing.T) {
	r := newRig(t)
	r.clip.formats, r.clip.text = []string{"text/plain"}, "from elsewhere"
	retried := make(chan struct{}, 1)
	r.s.Retry = func() { retried <- struct{}{} }
	r.clip.onRead = func() {} // Apply is in progress when the watcher looks
	r.s.applying.Add(1)
	r.s.Capture(context.Background())
	r.s.applying.Add(-1)
	if r.h.Len() != 0 {
		t.Fatal("nothing is read during an Apply")
	}
	text := "x"
	r.clip.text = "x"
	_ = r.s.Apply(model.Item{Kind: model.KindText, Text: &text})
	select {
	case <-retried:
	case <-time.After(2 * time.Second):
		t.Fatal("the change that was skipped must be looked at again when Apply is done")
	}
}

func TestBaseNameOfWindowsPaths(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Docs\report.pdf`:  "report.pdf",
		`\\nas\share\a b.txt`: "a b.txt",
		"/home/u/x.txt":       "x.txt",
		"/home/u/we\\ird.txt": "we\\ird.txt", // a backslash in a Unix name stays
	} {
		if runtime.GOOS == "windows" && in[0] == '/' {
			continue
		}
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApplyThatWroteNothingHoldsNothingBack(t *testing.T) {
	r := newRig(t)
	r.clip.formats, r.clip.text = []string{"text/plain"}, "from elsewhere"
	_ = r.s.Apply(model.Item{Kind: model.KindImage, ImageFilePath: model.Ptr(filepath.Join(r.l.Images(), "gone.png"))})
	r.now = r.now.Add(10 * time.Millisecond) // well inside the window
	r.s.Capture(context.Background())
	if r.h.Len() != 1 {
		t.Fatal("a copy right after an Apply that wrote nothing must be kept")
	}
}

func TestCopyInsideWindowWithoutCounter(t *testing.T) {
	r := newRig(t)
	r.clip.formats, r.clip.text = []string{"text/plain"}, "ours"
	text := "ours"
	_ = r.s.Apply(model.Item{Kind: model.KindText, Text: &text})
	r.now = r.now.Add(100 * time.Millisecond)
	r.s.Capture(context.Background())
	if r.h.Len() != 0 {
		t.Fatal("our own write is still held back inside the window")
	}
	r.clip.text = "somebody else, 300 ms later"
	r.now = r.now.Add(200 * time.Millisecond)
	r.s.Capture(context.Background())
	if r.h.Len() != 1 {
		t.Fatal("another copy inside the window must be kept: the content tells it from ours")
	}
}

func TestSettingsChangeWhileCapturing(t *testing.T) {
	r := newRig(t)
	r.clip.formats, r.clip.text = []string{"text/plain"}, "x"
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			r.s.ApplySettings(model.Settings{CaptureText: i%2 == 0, CaptureImages: true, CaptureFiles: true, CaptureFileMaxMegabytes: 20, MaxMegabytes: 100})
			r.s.SetFileExtensions("txt, png")
		}
	}()
	for i := 0; i < 200; i++ {
		r.s.Capture(context.Background())
	}
	<-done
}

func TestSamePictureSeesASmallChange(t *testing.T) {
	enc := func(dot bool) []byte {
		img := image.NewRGBA(image.Rect(0, 0, 200, 200))
		for i := range img.Pix {
			img.Pix[i] = 255
		}
		if dot {
			img.Set(100, 100, color.RGBA{0, 0, 0, 255}) // one black pixel in a white screenshot
		}
		var b bytes.Buffer
		_ = png.Encode(&b, img)
		return b.Bytes()
	}
	if samePicture(enc(false), enc(true)) {
		t.Fatal("a one-pixel change is another picture")
	}
	if !samePicture(enc(true), enc(true)) {
		t.Fatal("the same picture")
	}
}

func TestOwnershipEndsWhenSomethingElseIsSeen(t *testing.T) {
	r := newRig(t)
	r.clip.formats, r.clip.text = []string{"text/plain"}, "A"
	a := "A"
	_ = r.s.Apply(model.Item{Kind: model.KindText, Text: &a})
	r.clip.text = "B"
	r.capture() // B is another program's
	if r.h.Len() != 1 {
		t.Fatal("B must be captured")
	}
	r.clip.text = "A"
	lim := r.h.Limits()
	lim.MergeDuplicates = false
	r.h.SetLimits(lim)
	r.capture()
	if r.h.Len() != 2 {
		t.Fatalf("a later copy of A is a new copy, got %d items", r.h.Len())
	}
}

func TestTransparentPicturesAreCompared(t *testing.T) {
	enc := func(c color.NRGBA) []byte {
		img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
		for i := 0; i < 16; i++ {
			img.SetNRGBA(i%4, i/4, c)
		}
		var b bytes.Buffer
		_ = png.Encode(&b, img)
		return b.Bytes()
	}
	if samePicture(enc(color.NRGBA{255, 0, 0, 128}), enc(color.NRGBA{0, 0, 255, 128})) {
		t.Fatal("two see-through pictures of different colours are different")
	}
}

func TestUnreadableFileListIsRetried(t *testing.T) {
	r := newRig(t)
	r.clip.formats = []string{"text/uri-list"}
	r.clip.filesErr = errors.New("clipboard busy")
	retried := make(chan struct{}, 8)
	r.s.Retry = func() { retried <- struct{}{} }
	r.s.Capture(context.Background())
	select {
	case <-retried:
	case <-time.After(2 * time.Second):
		t.Fatal("a file list that could not be read must be looked at again")
	}
}

func TestRetryBudgetStartsOverAfterAGoodRead(t *testing.T) {
	r := newRig(t)
	var n atomic.Int32
	r.s.Retry = func() { n.Add(1) }
	r.clip.formats = nil // a busy clipboard names no format
	for i := 0; i < 8; i++ {
		r.s.Capture(context.Background())
	}
	time.Sleep(500 * time.Millisecond)
	if got := n.Load(); got != 5 {
		t.Fatalf("%d retries, want the budget of 5", got)
	}
	r.clip.formats, r.clip.text = []string{"text/plain"}, "fine now"
	r.s.Capture(context.Background()) // a good read starts the count over
	r.clip.formats = nil
	r.s.Capture(context.Background())
	time.Sleep(500 * time.Millisecond)
	if got := n.Load(); got != 6 {
		t.Fatalf("%d retries after a good read, want 6", got)
	}
}

func TestFallbackCaptureCancelsRetry(t *testing.T) {
	r := newRig(t)
	var n atomic.Int32
	r.s.Retry = func() { n.Add(1) }
	r.clip.formats, r.clip.text, r.clip.image = []string{"text/plain", "image/png"}, "", tinyPNG(t)
	r.s.Capture(context.Background())
	time.Sleep(500 * time.Millisecond)
	if r.h.Len() != 1 || n.Load() != 0 {
		t.Fatalf("%d items, %d retries: a picture beside empty text is captured once", r.h.Len(), n.Load())
	}
}

func TestAdvertisedImageThatDoesNotComeIsRetried(t *testing.T) {
	r := newRig(t)
	var n atomic.Int32
	r.s.Retry = func() { n.Add(1) }
	r.clip.formats, r.clip.image = []string{"image/png"}, nil
	r.s.Capture(context.Background())
	time.Sleep(500 * time.Millisecond)
	if n.Load() != 1 {
		t.Fatalf("%d retries, want 1", n.Load())
	}
}

func TestRetryIsDroppedWhenTheClipboardMovedOn(t *testing.T) {
	r := newRig(t)
	var seq atomic.Int64
	seq.Store(5)
	r.s.Seq = seq.Load
	var n atomic.Int32
	r.s.Retry = func() { n.Add(1) }
	r.clip.formats = nil // unreadable now
	r.s.Capture(context.Background())
	seq.Store(6) // somebody copied again before the retry: the watcher takes that one
	time.Sleep(500 * time.Millisecond)
	if n.Load() != 0 {
		t.Fatalf("%d retries of a change that is no longer the newest", n.Load())
	}
}

func TestRetryIsDroppedWithoutCounterWhenContentMoved(t *testing.T) {
	r := newRig(t)
	var tok atomic.Value
	tok.Store("a")
	r.s.Token = func() string { return tok.Load().(string) }
	var n atomic.Int32
	r.s.Retry = func() { n.Add(1) }
	r.clip.formats = nil
	r.s.Capture(context.Background())
	tok.Store("b") // new content before the retry
	time.Sleep(500 * time.Millisecond)
	if n.Load() != 0 {
		t.Fatalf("%d retries of content that was replaced", n.Load())
	}
}

func TestOldCheckDoesNotEndNewerOwnership(t *testing.T) {
	r := newRig(t)
	r.clip.formats, r.clip.text = []string{"text/plain"}, "new"
	newer := "new"
	old := func() bool { // reads an external copy, and meanwhile a newer Apply finishes
		_ = r.s.Apply(model.Item{Kind: model.KindText, Text: &newer})
		return false
	}
	r.s.ownCheck.Store(&old)
	if r.s.checkOwn(&old) {
		t.Fatal("the old check said no")
	}
	if r.s.ownCheck.Load() == nil {
		t.Fatal("the ownership of the newer Apply was ended by the older check")
	}
}

func TestEachChangeGetsItsOwnRetryBudget(t *testing.T) {
	r := newRig(t)
	var seq atomic.Int64
	seq.Store(1)
	r.s.Seq = seq.Load
	var n atomic.Int32
	r.s.Retry = func() { n.Add(1) }
	r.clip.formats = nil
	for i := 0; i < 8; i++ {
		r.s.Capture(context.Background()) // one change, looked at again and again
	}
	time.Sleep(500 * time.Millisecond)
	if n.Load() != 5 {
		t.Fatalf("%d retries for the first change, want 5", n.Load())
	}
	seq.Store(2) // a new change, also busy at first
	r.s.Capture(context.Background())
	time.Sleep(500 * time.Millisecond)
	if n.Load() != 6 {
		t.Fatalf("%d retries in all, the second change must get its own", n.Load())
	}
}

func TestSupersededApplyDoesNotTouchOwnership(t *testing.T) {
	r := newRig(t)
	r.clip.formats, r.clip.text = []string{"text/plain"}, "new"
	old := r.s.applyID.Add(1) // an older Apply, paused
	newer := "new"
	_ = r.s.Apply(model.Item{Kind: model.KindText, Text: &newer}) // a newer one finishes
	if r.s.ownCheck.Load() == nil {
		t.Fatal("the newer Apply owns the clipboard")
	}
	r.s.setOwn(old, 0, 0, nil) // the older one wakes up and says "not ours"
	if r.s.ownCheck.Load() == nil {
		t.Fatal("a superseded Apply must not clear the newer ownership")
	}
}

func TestChangeWhileReadingIsLeftToTheWatcher(t *testing.T) {
	r := newRig(t)
	var seq atomic.Int64
	seq.Store(1)
	r.s.Seq = seq.Load
	r.clip.formats, r.clip.text = []string{"text/plain"}, "B"
	r.clip.onRead = func() { seq.Store(2) } // somebody copies while we read
	r.s.Capture(context.Background())
	if r.h.Len() != 0 {
		t.Fatal("content read across a change is the watcher's to take at its next look")
	}
}

func TestSameChangeIsNotTakenTwice(t *testing.T) {
	r := newRig(t)
	var seq atomic.Int64
	seq.Store(2)
	r.s.Seq = seq.Load
	r.clip.formats, r.clip.image = []string{"image/png"}, tinyPNG(t)
	r.s.Capture(context.Background()) // captured under the key of change 2
	r.s.Capture(context.Background()) // the watcher, which still holds change 1, comes for it again
	items := r.h.Items()
	if len(items) != 1 || items[0].CopyCount != 1 {
		t.Fatalf("%d items, copy count %d: one change, one capture", len(items), items[0].CopyCount)
	}
}

func TestDiscardedCaptureLeavesNoFiles(t *testing.T) {
	r := newRig(t)
	var seq atomic.Int64
	seq.Store(1)
	r.s.Seq = seq.Load
	r.clip.formats, r.clip.image = []string{"image/png"}, tinyPNG(t)
	r.clip.onRead = func() {}
	// The counter moves while the picture is read and saved.
	r.s.Clipboard = &moveOnImage{fakeClip: r.clip, move: func() { seq.Store(2) }}
	r.s.Capture(context.Background())
	if entries, _ := os.ReadDir(r.l.Images()); len(entries) != 0 {
		t.Fatalf("%d files left from a capture that was dropped", len(entries))
	}
}

type moveOnImage struct {
	*fakeClip
	move func()
}

func (m *moveOnImage) ReadImage() []byte { m.move(); return m.fakeClip.ReadImage() }

func TestRecurringContentIsNotARepeat(t *testing.T) {
	r := newRig(t)
	r.s.Token = func() string { return "same picture" } // no counter: a content token
	r.clip.formats, r.clip.image = []string{"image/png"}, tinyPNG(t)
	r.s.Capture(context.Background())
	r.s.Capture(context.Background()) // copied again later: the token is the same, the change is not
	if items := r.h.Items(); len(items) != 1 || items[0].CopyCount != 2 {
		t.Fatalf("%d items: the second copy of the same picture must count", len(items))
	}
}

func TestStorageFailureIsReportedAndRetried(t *testing.T) {
	r := newRig(t)
	retried := make(chan struct{}, 8)
	r.s.Retry = func() { retried <- struct{}{} }
	// Put a file where the images folder should be: it cannot be made.
	_ = os.MkdirAll(filepath.Dir(r.l.Images()), 0o755)
	_ = os.WriteFile(r.l.Images(), []byte("not a folder"), 0o644)
	r.clip.formats, r.clip.image = []string{"image/png"}, tinyPNG(t)
	err := r.s.Capture(context.Background())
	if err == nil || !errors.Is(err, errStorage) {
		t.Fatalf("err = %v, want a storage error", err)
	}
	select {
	case <-retried:
	case <-time.After(2 * time.Second):
		t.Fatal("a picture that could not be stored must be tried again")
	}
}

func TestCachedContentIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permissions")
	}
	r := newRig(t)
	r.clip.formats, r.clip.image = []string{"image/png"}, tinyPNG(t)
	r.capture()
	src := write(t, filepath.Join(t.TempDir(), "secret.txt"), []byte("password"))
	files, _ := cacheDirAndCopy(r, src)
	for _, p := range append(files, model.Str(r.h.Items()[0].ImageFilePath)) {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is %v: other users can read it", p, st.Mode().Perm())
		}
	}
}

func cacheDirAndCopy(r *rig, src string) ([]string, error) {
	dst := filepath.Join(r.l.Files(), "x", "0000_secret.txt")
	_ = os.MkdirAll(filepath.Dir(dst), 0o700)
	err := copyFile(src, dst)
	return []string{dst, filepath.Dir(dst)}, err
}

func TestBlockedOnlyAllowlistDoesNotBecomeUnrestricted(t *testing.T) {
	allowed := parseExtensions(".exe,.dll")
	if allowed == nil || len(allowed) != 0 {
		t.Fatalf("effective allowlist must be empty but restricted: %v", allowed)
	}
	if parseExtensions(" , ; ") == nil {
	} else {
		t.Fatal("blank configuration should remain unrestricted")
	}
}
