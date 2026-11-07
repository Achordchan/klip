// Package capture turns what is on the system clipboard into history items,
// and puts a history item back on the clipboard: ClipboardCaptureService and
// ClipboardApplyService. The clipboard itself is behind an interface, so the
// rules are the same on every platform and testable without a window.
package capture

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "image/gif"
	_ "image/jpeg"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"

	"klip/internal/durable"
	"klip/internal/history"
	"klip/internal/imagecodec"
	"klip/internal/model"
	"klip/internal/paths"
	"klip/internal/snapshot"
)

// Clipboard is the system clipboard, as much of it as capture needs.
type Clipboard interface {
	// Formats names what is on the clipboard, in portable names such as
	// "text/plain", "text/uri-list", "image/png" (MyGo's).
	Formats() []string
	ReadText() string
	// ReadFiles returns the paths of copied files, nil when there are none.
	ReadFiles() ([]string, error)
	// ReadImage returns the image as PNG, nil when there is none.
	ReadImage() []byte
	WriteText(text string) error
	WriteFiles(paths []string) error
	// WriteImage puts the PNG on the clipboard; filePath, when not empty, is
	// the file it came from, offered as a file too.
	WriteImage(png []byte, filePath string) error
}

// AppIdentity is the program a copy came from.
type AppIdentity struct{ Name, Path string }

// Service captures the clipboard into the history.
type retryKey struct{}

type Service struct {
	storeWebP bool
	// ClipboardDir holds the PNG copies offered as files when a picture kept
	// in another format is put on the clipboard; empty is a folder in the
	// user's own cache directory (never the shared temporary directory, where
	// another user could put a link of their choosing in its place).
	ClipboardDir string
	Layout       paths.Layout
	History      *history.Service
	Clipboard    Clipboard
	// ForegroundApp says which program is in front, or nil. May be nil.
	ForegroundApp func() *AppIdentity

	CaptureText         bool
	CaptureImages       bool
	CaptureFiles        bool
	CaptureFileMaxBytes int64

	// Now and NewID are time.Now and a random GUID unless a test sets them.
	Now   func() time.Time
	NewID func() string

	// Seq is the clipboard change counter (negative when the platform has
	// none); may be nil. With it, the change made by Apply is recognised by
	// its number, however long the poll takes to notice it.
	Seq func() int64
	// Token stands in for Seq on a platform without a counter: a string that
	// changes with the clipboard content (platform.ContentFingerprint); may be
	// nil.
	Token func() string
	// CapturedToken acknowledges the successfully captured fallback fingerprint.
	CapturedToken func(string)
	// Retry is called when a change was not looked at because an Apply was
	// running; it should capture again (may be nil).
	Retry           func()
	RetryGeneration func(uint64, string)
	// OwnWindow is how long after Apply a change is taken for our own when
	// there is no counter: the poll interval plus the C# 250 ms. Zero is 250 ms.
	OwnWindow time.Duration

	allowed         map[string]bool
	lastOwnWrit     atomic.Int64
	ownSeq          atomic.Int64                // counter after our last write, plus 2; 0 is none
	applying        atomic.Int32                // Apply calls in progress
	ownCheck        atomic.Pointer[func() bool] // reads the clipboard back; true while it holds what we wrote
	ownMu           sync.Mutex                  // makes a change of the ownership state one step
	applyID         atomic.Int64                // number of the newest Apply
	ownGen          atomic.Int64                // moves with every change of it
	setMu           sync.RWMutex                // guards the capture settings below
	capMu           sync.Mutex                  // one Capture at a time (the watcher and Retry)
	lastDone        atomic.Value                // the change key of the last capture that was kept
	triesFor        atomic.Value                // the change the budget is for
	retryGeneration atomic.Uint64
	tries           atomic.Int32 // retries of an unreadable clipboard in a row
	took            atomic.Bool  // whether the last Capture took something (see FoundNothing)
	skipped         atomic.Bool  // a Capture gave way to an Apply
	writeGen        atomic.Int64 // moves at the start and the end of every Apply
}

// New returns a Service with the defaults of ClipboardCaptureService: all
// three kinds on, files up to 20 MB, any file extension (but see
// dangerousExtensions).
func New(layout paths.Layout, h *history.Service, c Clipboard) *Service {
	return &Service{
		Layout: layout, History: h, Clipboard: c,
		storeWebP: true, CaptureText: true, CaptureImages: true, CaptureFiles: true, CaptureFileMaxBytes: 20 << 20,
		Now: time.Now, NewID: newGUID,
		// 250 ms of C# plus one poll of the platform watcher (400 ms): a write
		// just after a poll is seen one interval later.
		OwnWindow: 650 * time.Millisecond,
	}
}

// ApplySettings sets the capture options from the settings, as App.ApplySettings
// does: the file size limit is the smaller of its own and the history's.
func (s *Service) ApplySettings(st model.Settings) {
	s.setMu.Lock()
	s.CaptureText, s.CaptureImages, s.CaptureFiles = st.CaptureText, st.CaptureImages, st.CaptureFiles
	s.allowed = parseExtensions(st.CaptureFileExtensions)
	s.storeWebP = st.StoreImagesAsWebP()
	s.CaptureFileMaxBytes = int64(min(st.CaptureFileMaxMegabytes, st.MaxMegabytes)) * 1024 * 1024
	s.setMu.Unlock()
}

// SetFileExtensions sets which file extensions are captured: a list separated
// by commas, semicolons or spaces. An empty list is any extension.
func (s *Service) SetFileExtensions(list string) {
	s.setMu.Lock()
	s.allowed = parseExtensions(list)
	s.setMu.Unlock()
}

// options is the capture settings as one capture sees them: a copy taken at
// its start, so a change of settings never shows up half way.
type options struct {
	text, images, files bool
	maxBytes            int64
	allowed             map[string]bool
}

func (s *Service) options() options {
	s.setMu.RLock()
	defer s.setMu.RUnlock()
	return options{s.CaptureText, s.CaptureImages, s.CaptureFiles, s.CaptureFileMaxBytes, s.allowed}
}

// MarkWriteByUs says that the app has just written to the clipboard, so that
// the change that follows is not captured again.
func (s *Service) MarkWriteByUs() {
	s.setOwn(0, s.Now().UnixNano(), 0, nil)
}

// setOwn sets the ownership state in one step (never held across a clipboard
// call) and moves its generation.
func (s *Service) setOwn(apply int64, at int64, seqPlus2 int64, check *func() bool) {
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	if apply != 0 && apply != s.applyID.Load() {
		return // a newer Apply has taken over: this one is not heard any more
	}
	s.ownGen.Add(1)
	s.lastOwnWrit.Store(at)
	s.ownSeq.Store(seqPlus2)
	s.ownCheck.Store(check)
}

// markWritten is called when the write is done: the counter then stands where
// our own change left it.
func (s *Service) markWritten(apply int64, verify func() bool) {
	now := s.Now().UnixNano()
	notOurs := func() { s.setOwn(apply, 0, 0, nil) } // somebody copied in between: capture it
	var before int64 = -1
	if s.Seq != nil {
		before = s.Seq()
	}
	if verify != nil && !verifyWithRetry(verify) {
		notOurs()
		return
	}
	var seqPlus2 int64
	if s.Seq != nil {
		n := s.Seq()
		if n != before {
			notOurs() // the counter moved while we checked: not only our write
			return
		}
		if n >= 0 {
			seqPlus2 = n + 2
		}
	}
	var check *func() bool
	if verify != nil {
		check = &verify // without a counter, content tells ours from theirs
	}
	s.setOwn(apply, now, seqPlus2, check)
}

// checkOwn asks the verifier. Once it says the clipboard holds something
// else, the ownership is over: a later copy of the same text is a new copy.
func (s *Service) checkOwn(check *func() bool) bool {
	if verifyWithRetry(*check) {
		return true
	}
	// Only the ownership this very verifier belongs to may be ended: an Apply
	// that recorded a newer one meanwhile keeps it.
	s.ownMu.Lock()
	defer s.ownMu.Unlock()
	if s.ownCheck.Load() == check {
		s.ownGen.Add(1)
		s.lastOwnWrit.Store(0)
		s.ownSeq.Store(0)
		s.ownCheck.Store(nil)
	}
	return false
}

// ownSeqTTL is how long the counter after our write is trusted.
const ownSeqTTL = 5 * time.Second

// isOwnChange says whether the clipboard is as our last write left it, or the
// write is too recent to tell.
func (s *Service) isOwnChange() bool {
	check := s.ownCheck.Load()
	if s.Seq != nil {
		if own := s.ownSeq.Load(); own != 0 {
			if n := s.Seq(); n >= 0 {
				// The counter knows: anywhere but where our write left it,
				// somebody else copied, window or not.
				if n != own-2 {
					return false
				}
				if s.Now().UnixNano()-s.lastOwnWrit.Load() < int64(ownSeqTTL) || check == nil {
					return true
				}
				// Seen late (the machine slept?): the content has the say.
				return s.checkOwn(check)
			}
		}
	}
	// No counter: ours if the clipboard still holds what we wrote, however
	// late it is looked at; before that is known, the window decides.
	if check != nil {
		return s.checkOwn(check)
	}
	w := s.OwnWindow
	if w <= 0 {
		w = 250 * time.Millisecond
	}
	return s.Now().UnixNano()-s.lastOwnWrit.Load() < int64(w)
}

// dangerousExtensions are never captured, whatever the list says.
var dangerousExtensions = map[string]bool{
	".exe": true, ".msi": true, ".msp": true, ".msix": true, ".appx": true,
	".bat": true, ".cmd": true, ".com": true, ".scr": true, ".pif": true,
	".ps1": true, ".psm1": true, ".psd1": true, ".vbs": true, ".vbe": true, ".js": true, ".jse": true, ".wsf": true, ".wsh": true, ".hta": true,
	".jar": true, ".reg": true, ".lnk": true, ".url": true, ".cpl": true, ".dll": true,
}

func parseExtensions(list string) map[string]bool {
	fields := strings.FieldsFunc(list, func(r rune) bool { return strings.ContainsRune(",; \t\r\n", r) })
	set := map[string]bool{}
	for _, f := range fields {
		ext := strings.TrimSpace(f)
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		ext = strings.ToLower(ext)
		if dangerousExtensions[ext] {
			continue
		}
		set[ext] = true
	}
	if len(fields) == 0 {
		return nil
	}
	return set
}

var textFormats = []string{"text/plain", "text", "unicodetext"}
var fileFormats = []string{"application/x-mygo-file-list", "text/uri-list", "filenames", "files"}
var imageFormats = []string{"image/png", "png", "bitmap", "deviceindependentbitmap", "dib", "image/bmp", "image/tiff", "image/jpeg"}

func has(set map[string]bool, names []string) bool {
	for _, n := range names {
		if set[n] {
			return true
		}
	}
	return false
}

// Capture reads the clipboard once and adds what it finds to the history.
// It does nothing for the change the app made itself with Apply: found by the
// change counter when there is one, else by the 250 ms (OwnWindow) after it.
// Text comes first, then files, then images; the first that yields something
// is the item.
func (s *Service) Capture(ctx context.Context) error { return s.capture(ctx, nil) }
func (s *Service) CaptureRetry(ctx context.Context, generation uint64, key string) error {
	return s.capture(context.WithValue(ctx, retryKey{}, key), &generation)
}
func (s *Service) capture(ctx context.Context, expected *uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	generation := s.retryGeneration.Load()
	s.capMu.Lock()
	defer s.capMu.Unlock()
	if expected != nil && *expected != s.retryGeneration.Load() {
		return nil
	}
	if generation != s.retryGeneration.Load() && s.lastDone.Load() == any(s.changeKey()) {
		return nil
	}
	gen := s.writeGen.Load()
	s.took.Store(false)
	opt := s.options()
	if s.applying.Load() > 0 {
		s.noteSkipped()
		s.took.Store(true) // the retry after the Apply is on its way
		return nil
	}
	if s.isOwnChange() {
		s.took.Store(true) // ours: nothing to look for
		return nil
	}
	// When the clipboard could not be read (it was busy), the watcher has
	// already moved on, so look again a little later: up to five times in a
	// row, the count starting over with the next clipboard that reads well.
	key := s.changeKey() // taken before any read: it is the change being looked at
	if original, ok := ctx.Value(retryKey{}).(string); ok && original != "" && key != "" && original != key {
		return nil
	}
	if strings.HasPrefix(key, "seq:") && s.lastDone.Load() == any(key) {
		s.took.Store(true)
		return nil // this change was taken already (the watcher saw it late)
	}
	wantRetry := false
	var storeErr error // the picture was fine but could not be stored
	defer func() {
		if wantRetry {
			s.retryLater(key)
		} else {
			s.tries.Store(0)
		}
	}()
	formats := s.Clipboard.Formats()
	if len(formats) == 0 {
		wantRetry = true
	}
	set := map[string]bool{}
	for _, f := range formats {
		set[strings.ToLower(f)] = true
	}
	var app *AppIdentity
	if s.ForegroundApp != nil {
		app = s.ForegroundApp()
	}
	add := func(it model.Item) {
		if ctx.Err() != nil {
			s.discard(it)
			return
		}
		if s.interfered(gen) {
			s.noteSkipped()
			// An Apply wrote while we read: this is our own content.
			s.discard(it)
			return
		}
		if current := s.changeKey(); key != "" && current != "" && current != key {
			// The clipboard changed while we read it: what was read may be the
			// newer content, which the watcher takes at its next look. Taking
			// it here too would capture it twice.
			s.discard(it)
			return
		}
		if key != "" {
			// The counter gives every change its own number, so a second look
			// at the same number is a repeat. A content token is not that: the
			// same picture copied again has the same token.
			s.lastDone.Store(key)
		}
		s.retryGeneration.Add(1)
		wantRetry = false // something was captured: nothing to look at again
		now := model.Time{Time: s.Now()}
		it.CapturedAt, it.FirstCapturedAt = now, &now
		if app != nil {
			it.SourceAppName, it.SourceAppPath = &app.Name, &app.Path
		}
		s.History.Add(it)
		s.took.Store(true)
		if strings.HasPrefix(key, "tok:") && s.CapturedToken != nil {
			s.CapturedToken(strings.TrimPrefix(key, "tok:"))
		}
	}

	if opt.text && has(set, textFormats) {
		text := s.Clipboard.ReadText()
		if text == "" {
			wantRetry = true // the format is there but the text did not come
		}
		if strings.TrimSpace(text) != "" {
			items := s.History.Items()
			if s.History.Limits().MergeDuplicates && len(items) > 0 && items[0].Kind == model.KindText && model.Str(items[0].Text) == text {
				return nil
			}
			add(model.Item{
				ID: s.NewID(), Kind: model.KindText, ApproxBytes: int64(model.UTF16Len(text)) * 2, Text: &text, CopyCount: 1,
				ContentHash: model.Ptr(hashOf("text", []byte(text))),
			})
			return nil
		}
	}

	if opt.files && has(set, fileFormats) {
		files, ferr := s.Clipboard.ReadFiles()
		if ferr != nil && len(nonBlank(files)) == 0 {
			wantRetry = true // not "no files" but "could not look"
		}
		files = nonBlank(files)
		if len(files) > 0 {
			if opt.images && len(files) == 1 && isImagePath(files[0]) && isFile(files[0]) {
				if data, err := readBoundedImage(files[0], opt.maxBytes); err == nil {
					p, h, size, err := s.saveImage(data)
					if err == nil {
						add(model.Item{ID: s.NewID(), Kind: model.KindImage, ApproxBytes: size, ImageFilePath: &p, CopyCount: 1, ContentHash: &h})
						return nil
					}
					if errors.Is(err, errStorage) {
						storeErr, wantRetry = err, true
					}
				}
			}
			files = s.filterFiles(files, opt)
			if len(files) > 0 {
				id := s.NewID()
				if err := s.cacheCopies(id, files, opt.maxBytes); err != nil {
					storeErr, wantRetry = fmt.Errorf("%w: %v", errStorage, err), true
					return storeErr
				}
				add(model.Item{
					ID: id, Kind: model.KindFileList, ApproxBytes: s.fileListBytes(id, files), FilePaths: files, CopyCount: 1,
					ContentHash: model.Ptr(hashOf("files", []byte(strings.Join(files, "\n")))),
				})
				return nil
			}
		}
	}

	if opt.images {
		// Whatever was not captured above gets one try as a picture, whatever
		// formats are named: screenshot tools use odd names, and blank text
		// or a text flavour beside the picture must not hide it (the C#
		// fallback loop does the same).
		data := s.Clipboard.ReadImage()
		if len(data) == 0 && has(set, imageFormats) {
			wantRetry = true // an image is advertised but did not come
		}
		if len(data) > 0 {
			p, h, size, err := s.saveImage(data)
			if err == nil {
				add(model.Item{ID: s.NewID(), Kind: model.KindImage, ApproxBytes: size, ImageFilePath: &p, CopyCount: 1, ContentHash: &h})
				return nil
			}
			if errors.Is(err, errStorage) {
				storeErr, wantRetry = err, true
			}
		}
	}
	if storeErr != nil {
		return storeErr
	}
	return ctx.Err()
}

func nonBlank(in []string) []string {
	var out []string
	for _, p := range in {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// filterFiles drops directories, dangerous types, types not in the list and
// files over the size limit (a size of 0 or unknown passes).
func (s *Service) filterFiles(in []string, opt options) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if strings.TrimSpace(p) == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			continue
		}
		ext := strings.ToLower(extension(p))
		if ext != "" && !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		if ext != "" && dangerousExtensions[ext] {
			continue
		}
		if opt.allowed != nil {
			if ext == "" || !opt.allowed[ext] {
				continue
			}
		}
		if opt.maxBytes > 0 {
			if size := fileSize(p); size > 0 && size > opt.maxBytes {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// cacheCopies copies the files into files/<id>/NNNN_name, so that the item
// still has them when the originals move or go.
func (s *Service) cacheCopies(id string, files []string, limits ...int64) (err error) {
	var limit int64
	if len(limits) > 0 {
		limit = limits[0]
	}
	dir := filepath.Join(s.Layout.Files(), strings.ReplaceAll(strings.ToLower(id), "-", ""))
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	for i, src := range files {
		name := baseName(src)
		if strings.TrimSpace(src) == "" || !isFile(src) || strings.TrimSpace(name) == "" {
			return fmt.Errorf("source file unavailable: %s", src)
		}
		if err = copyFileBounded(src, filepath.Join(dir, captureCacheName(i, name)), limit); err != nil {
			return err
		}
	}
	return nil
}

// fileListBytes is the size of the cached copies, else of the originals, else
// twice the length of the paths (so that it is never 0).
func (s *Service) fileListBytes(id string, files []string) int64 {
	dir := filepath.Join(s.Layout.Files(), strings.ReplaceAll(strings.ToLower(id), "-", ""))
	var n int64
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if !e.IsDir() {
				n += fileSize(filepath.Join(dir, e.Name()))
			}
		}
	}
	if n <= 0 {
		for _, p := range files {
			n += fileSize(p)
		}
	}
	if n <= 0 {
		for _, p := range files {
			n += int64(model.UTF16Len(p)) * 2
		}
	}
	return n
}

// errStorage marks a failure to store a picture that was good: the disk is
// full or the folder cannot be written, as against data that is no picture.
var errStorage = errors.New("capture: cannot store the picture")

// saveImage stores lossless WebP by default, or PNG when disabled, when the
// picture has more than 8 bits per channel (WebP would lose them), when it
// carries colour information WebP would drop (an ICC profile, a gamma: see
// imagecodec.PNGColorInfo) or when it is wider or taller than WebP allows.
//
// A picture has two possible fingerprints: the hash of its PNG bytes (what
// C# and earlier versions store, and what a PNG kept as it came gives back
// on the clipboard) and the hash of its canonical PNG (what a WebP picture
// gives back, see imagecodec.CanonicalPNG). An item already in the history
// under either keeps its fingerprint, so neither a round trip through the
// clipboard nor a change of the setting makes a duplicate.
func (s *Service) saveImage(data []byte) (path, hash string, size int64, err error) {
	if err := validateImageSize(data); err != nil {
		return "", "", 0, err
	}
	var img image.Image
	colour := imagecodec.PNGColorInfo(data) // of a PNG as it came, before any conversion
	if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		var derr error
		img, _, derr = image.Decode(bytes.NewReader(data))
		if derr != nil {
			return "", "", 0, derr
		}
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			return "", "", 0, err
		}
		data = b.Bytes()
	} else {
		var derr error
		if img, derr = png.Decode(bytes.NewReader(data)); derr != nil {
			// the header alone is not enough: a cut-off file must not become a
			// picture that cannot be shown
			return "", "", 0, derr
		}
	}
	legacy := hashOf("img", data)
	deep := imagecodec.HighDepth(img)
	s.setMu.RLock()
	webp := s.storeWebP && !deep && !colour && imagecodec.FitsWebP(img.Bounds())
	s.setMu.RUnlock()
	hash = legacy
	if !deep {
		canonical, err := imagecodec.CanonicalPNG(img)
		if err != nil {
			return "", "", 0, err
		}
		if webp {
			hash = hashOf("img", canonical)
		}
		hash = s.knownImageHash(hash, legacy, hashOf("img", canonical))
	}
	extension := "png"
	if webp {
		data, err = imagecodec.Encode(imagecodec.Normalize(img), "webp")
		if err != nil {
			return "", "", 0, err
		}
		extension = "webp"
	}
	if err := os.MkdirAll(s.Layout.Images(), 0o700); err != nil {
		return "", "", 0, fmt.Errorf("%w: %v", errStorage, err)
	}
	path = filepath.Join(s.Layout.Images(), fmt.Sprintf("%d_%s.%s", s.Now().UnixMilli(), strings.ReplaceAll(newGUID(), "-", ""), extension))
	// To a side file first: a write that stops half way (disk full) must not
	// leave a broken picture behind.
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		os.Remove(tmp)
		return "", "", 0, fmt.Errorf("%w: %v", errStorage, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return "", "", 0, fmt.Errorf("%w: %v", errStorage, err)
	}
	return path, hash, int64(len(data)), nil
}

// knownImageHash is the fingerprint of an image item of the history that has
// one of the candidates, or fallback when none has.
//
// It is taken over only for the capture to be merged into that item, whose
// picture file then stays. Without merging the capture is an item of its own
// and keeps fallback, the fingerprint its own file gives back on the
// clipboard: a WebP item given the PNG-bytes hash of an older item could not
// be found again from its canonical PNG once merging is turned on.
func (s *Service) knownImageHash(fallback string, candidates ...string) string {
	if s.History == nil || !s.History.Limits().MergeDuplicates {
		return fallback
	}
	for _, it := range s.History.Items() {
		if it.Kind != model.KindImage || !isFile(model.Str(it.ImageFilePath)) {
			continue // a picture that is gone is replaced by the new file: fallback fits that
		}
		for _, c := range candidates {
			if strings.EqualFold(model.Str(it.ContentHash), c) {
				return model.Str(it.ContentHash)
			}
		}
	}
	return fallback
}

// clipboardPNG writes the PNG of a picture kept in another format, so that a
// paste as a file gets a PNG as before. It returns "" when it cannot: the
// picture then goes without a file.
//
// Once the picture is on the clipboard, the copies of the ones before it are
// needed by nobody and go (pruneClipboardPNGs): the folder holds the copy the
// clipboard offers, and at most one more after a write that failed.
func (s *Service) clipboardPNG(stored string, data []byte) string {
	dir, err := s.clipboardDir()
	if err != nil {
		return ""
	}
	base := filepath.Base(stored)
	path := filepath.Join(dir, strings.TrimSuffix(base, filepath.Ext(base))+".png")
	if got, err := os.ReadFile(path); err == nil && bytes.Equal(got, data) {
		s.offered(path)
		return path
	}
	f, err := os.CreateTemp(dir, ".klip-png-*")
	if err != nil {
		return ""
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil || os.Rename(tmp, path) != nil {
		os.Remove(tmp)
		return ""
	}
	s.offered(path)
	return path
}

// How long a PNG copy is kept after it was last offered, and how many are
// kept at most. A paste may go on reading a copy after the clipboard moved on
// (a file manager asking whether to replace a file): a copy goes when it is
// old, not when the next picture is put on the clipboard.
const (
	clipboardCopyRetention = 10 * time.Minute
	clipboardCopyMax       = 32
)

// offered marks a copy as just offered: its time is what pruneClipboardPNGs
// goes by.
func (s *Service) offered(path string) {
	now := s.Now()
	_ = os.Chtimes(path, now, now)
}

// clipboardDir is the folder of the PNG copies, made if need be. It must be a
// folder of this user's that only they can enter: a link, or a folder of
// somebody else, is refused, as writing into it, and pruning it, would act on
// files that are not Klip's.
func (s *Service) clipboardDir() (string, error) {
	dir := s.ClipboardDir
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "Klip", "clipboard-png")
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("capture: the clipboard folder is not a plain folder")
	}
	if !ownedByMe(info) {
		return "", errors.New("capture: the clipboard folder belongs to another user")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// pruneClipboardPNGs removes the copies, other than keep ("" for none), last
// offered more than clipboardCopyRetention ago, and side files left by a
// write that stopped as long ago; of the rest it keeps the clipboardCopyMax
// newest. It is called once a picture is on the clipboard: until then, the
// clipboard may still offer the copy before.
func (s *Service) pruneClipboardPNGs(keep string) {
	dir, err := s.clipboardDir()
	if err != nil {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type copyFile struct {
		path string
		at   time.Time
	}
	var young []copyFile
	now := s.Now()
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if !e.Type().IsRegular() || (keep != "" && samePath(p, keep)) {
			continue // a folder or a link is not a copy Klip made
		}
		if !strings.EqualFold(filepath.Ext(e.Name()), ".png") && !strings.HasPrefix(e.Name(), ".klip-png-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > clipboardCopyRetention {
			os.Remove(p)
			continue
		}
		young = append(young, copyFile{p, info.ModTime()})
	}
	if len(young) <= clipboardCopyMax {
		return
	}
	sort.Slice(young, func(i, j int) bool { return young[i].at.After(young[j].at) })
	for _, f := range young[clipboardCopyMax:] {
		os.Remove(f.path)
	}
}

// hashOf is "<prefix>:<SHA-256 in upper case hex>", what C# stores in
// ContentHash.
func hashOf(prefix string, payload []byte) string {
	sum := sha256.Sum256(payload)
	return prefix + ":" + strings.ToUpper(hex.EncodeToString(sum[:]))
}

func isImagePath(p string) bool {
	switch strings.ToLower(extension(p)) {
	case ".png", ".jpg", ".jpeg", ".bmp", ".gif", ".webp":
		return true
	}
	return false
}

// baseName is Path.GetFileName: on Windows it cuts at / and \, elsewhere at
// / only.
// windowsStyle says whether p looks like a Windows path: a drive letter and a
// backslash (C:\...) or a share (\\host\...).
func windowsStyle(p string) bool {
	if strings.HasPrefix(p, `\\`) {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && p[2] == '\\' && (p[0]|0x20) >= 'a' && (p[0]|0x20) <= 'z'
}

func baseName(p string) string {
	cut := strings.LastIndex(p, "/")
	// A path that came from Windows (a sync, or the C# client) keeps its
	// backslashes wherever it is read; on Linux and macOS they are otherwise
	// part of a file name.
	if runtime.GOOS == "windows" || windowsStyle(p) {
		if i := strings.LastIndex(p, `\`); i > cut {
			cut = i
		}
	}
	return p[cut+1:]
}

// extension is Path.GetExtension: the last dot of the file name and what
// follows, "" when there is none or it ends the name.
func extension(p string) string {
	name := baseName(p)
	i := strings.LastIndex(name, ".")
	if i < 0 || i == len(name)-1 {
		return ""
	}
	return name[i:]
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return 0
	}
	return st.Size()
}

// isFile is true for a regular file: a device (/dev/zero) or a pipe would
// never end, or never answer, when it is read or copied.
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func copyFile(src, dst string) error { return copyFileBounded(src, dst, 0) }
func copyFileBounded(src, dst string, limit int64) (err error) {
	if !isFile(src) {
		return errors.New("capture: not a regular file: " + src)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Copy to a side file and rename it when whole, so that a copy that
	// fails half way never stands in for the original.
	temp, err := os.CreateTemp(filepath.Dir(dst), "capture-part-*")
	if err != nil {
		return err
	}
	tmp := temp.Name()
	temp.Close()
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // private: the copy may hold what the original kept to itself
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	var reader io.Reader = in
	if limit > 0 {
		reader = io.LimitReader(in, limit+1)
	}
	var written int64
	written, err = io.Copy(out, reader)
	if err == nil && limit > 0 && written > limit {
		err = errors.New("capture file exceeds size limit")
	}
	if err != nil {
		out.Close()
		return err
	}
	if err = out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	return durable.Rename(tmp, dst)
}

func newGUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// FoundNothing says whether the last Capture took nothing from the clipboard.
// A program that declares what it will put on the clipboard and writes it a
// moment later moves the change counter for the first step only: a look in
// between sees nothing, or what was there before, and nothing tells the
// watcher to look again. So a look that took nothing is repeated a few times
// (platform.Watcher.Retry); one that took something, or that left the
// clipboard alone because the app had just written it itself, is not.
func (s *Service) FoundNothing() bool { return !s.took.Load() }

// ---- apply

// Apply puts an item back on the clipboard (ClipboardApplyService). Content
// that is gone is not an error: nothing is written.
//
// Apply is not called from two goroutines at once (the window calls it, one
// click at a time); overlapping calls are not ordered against each other.
// Locking them would not be safe either: the clipboard calls hop to the main
// thread, and a caller on it waiting for a lock would deadlock with the holder.
//
// A Capture that is reading while this writes would take the written content
// for a copy, so Capture drops what it read when an Apply ran in the
// meantime (see interfered). It does not wait for Apply: both call the
// clipboard, which hops to the main thread, and a lock held across that could
// deadlock with a caller that is on it.
func (s *Service) Apply(it model.Item) error {
	id := s.applyID.Add(1)
	s.applying.Add(1)
	s.writeGen.Add(1)
	defer func() {
		s.writeGen.Add(1)
		if s.applying.Add(-1) == 0 {
			s.retrySkipped()
		}
	}()
	s.MarkWriteByUs()
	wrote, w, err := s.write(it)
	if wrote && err == nil {
		s.markWritten(id, s.verifier(it, w)) // only a write that happened can be recognised as ours
	} else {
		s.setOwn(id, 0, 0, nil) // nothing was written: nothing to hold back
	}
	return err
}

// verifier reads the clipboard back to see that it still holds what Apply
// wrote: the text itself, the files that were written (by name), or a picture
// with the same pixels.
func (s *Service) verifier(it model.Item, w written) func() bool {
	files := w.files
	switch it.Kind {
	case model.KindText:
		want := model.Str(it.Text)
		return func() bool { return s.Clipboard.ReadText() == want }
	case model.KindFileList:
		initial := make([]string, len(files))
		for i, p := range files {
			if st, err := os.Stat(p); err == nil {
				initial[i] = fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
			}
		}
		return func() bool {
			got, _ := s.Clipboard.ReadFiles()
			got = nonBlank(got)
			if len(got) != len(files) {
				return false
			}
			for i, g := range got {
				if st, err := os.Stat(g); err != nil || fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano()) != initial[i] {
					return false
				}
				if !samePath(g, files[i]) {
					return false
				}
			}
			return true
		}
	case model.KindImage:
		return func() bool {
			return samePicture(w.image, s.Clipboard.ReadImage())
		}
	}
	return nil
}

// Retry immediate readback briefly: native clipboard locks are transient.
func verifyWithRetry(verify func() bool) bool {
	for i := 0; i < 3; i++ {
		if verify() {
			return true
		}
		if i < 2 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return false
}

// samePath says whether two paths are the same file: by the file system when
// both exist, else by their text (Windows ignores case).
func samePath(a, b string) bool {
	// The file system knows best (case-sensitive volumes, links, hard links).
	if sa, err := os.Stat(a); err == nil {
		if sb, err := os.Stat(b); err == nil {
			return os.SameFile(sa, sb)
		}
		return false
	}
	norm := func(p string) string { return filepath.Clean(p) }
	a, b = norm(a), norm(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// samePicture says whether two encoded pictures show the same pixels, give or
// take the rounding a trip through the clipboard does: no channel (alpha
// included) of any pixel may differ by more than a few levels, so a small
// change in one corner is a different picture. A clipboard that drops the
// alpha of a see-through picture makes it a different one, which errs on the
// side of keeping a copy.
func samePicture(a, b []byte) bool {
	if validateImageSize(a) != nil || validateImageSize(b) != nil {
		return false
	}
	ia, _, errA := image.Decode(bytes.NewReader(a))
	ib, _, errB := image.Decode(bytes.NewReader(b))
	if errA != nil || errB != nil || ia.Bounds().Dx() != ib.Bounds().Dx() || ia.Bounds().Dy() != ib.Bounds().Dy() {
		return false
	}
	const tolerance = 3 * 257 // three levels of 255, in 16 bits
	ba, bb := ia.Bounds(), ib.Bounds()
	for y := 0; y < ba.Dy(); y++ {
		for x := 0; x < ba.Dx(); x++ {
			r1, g1, b1, a1 := ia.At(ba.Min.X+x, ba.Min.Y+y).RGBA()
			r2, g2, b2, a2 := ib.At(bb.Min.X+x, bb.Min.Y+y).RGBA()
			if absDiff(r1, r2) > tolerance || absDiff(g1, g2) > tolerance || absDiff(b1, b2) > tolerance || absDiff(a1, a2) > tolerance {
				return false
			}
		}
	}
	return true
}

func absDiff(a, b uint32) uint64 {
	if a > b {
		return uint64(a - b)
	}
	return uint64(b - a)
}

// discard throws away an item that was made but not kept: the files made for
// it belong to no item, so they go too.
func (s *Service) discard(it model.Item) {
	if p := model.Str(it.ImageFilePath); p != "" {
		_ = os.Remove(p)
	}
	if it.Kind == model.KindFileList {
		_ = os.RemoveAll(filepath.Join(s.Layout.Files(), strings.ReplaceAll(strings.ToLower(it.ID), "-", "")))
	}
}

// retryLater asks for another look at the clipboard in 300 ms (the budget of
// five is kept in tries).
func (s *Service) retryLater(key string) {
	if s.Retry == nil && s.RetryGeneration == nil {
		return
	}
	// The budget is for one change: a new one starts with five again.
	if prev, _ := s.triesFor.Load().(string); prev != key {
		s.triesFor.Store(key)
		s.tries.Store(0)
	}
	if s.tries.Add(1) > 5 {
		return
	}
	generation := s.retryGeneration.Load()
	time.AfterFunc(300*time.Millisecond, func() {
		if generation != s.retryGeneration.Load() {
			return
		}
		// A newer change is the watcher's to take; looking at it here too
		// would capture it twice.
		if current := s.changeKey(); key != "" && current != "" && current != key {
			return
		}
		if s.RetryGeneration != nil {
			s.RetryGeneration(generation, key)
		} else {
			s.Retry()
		}
	})
}

// changeKey names the clipboard change that is current: the counter, or
// without one the content token ("" when there is neither).
func (s *Service) changeKey() string {
	if s.Seq != nil {
		if n := s.Seq(); n >= 0 {
			return "seq:" + strconv.FormatInt(n, 10)
		}
	}
	if s.Token != nil {
		token := s.Token()
		if token == "" {
			return ""
		}
		return "tok:" + token
	}
	return ""
}

// noteSkipped registers a capture that gave way to an Apply. The Apply may
// have finished already, so the flag is looked at again: one side or the other
// always finds it set and starts the retry.
func (s *Service) noteSkipped() {
	s.skipped.Store(true)
	if s.applying.Load() == 0 {
		s.retrySkipped()
	}
}

// retrySkipped starts the retry for a change that was not looked at.
func (s *Service) retrySkipped() {
	if s.skipped.Swap(false) {
		if s.RetryGeneration != nil {
			g, k := s.retryGeneration.Load(), s.changeKey()
			go s.RetryGeneration(g, k)
		} else if s.Retry != nil {
			go s.Retry()
		}
	}
}

// interfered says whether an Apply ran, or is running, since gen was read.
func (s *Service) interfered(gen int64) bool {
	return s.applying.Load() > 0 || s.writeGen.Load() != gen
}

// written is what a write put on the clipboard besides text: the files by
// their paths, the picture by its bytes (the history may drop the file at any
// time, so the bytes are kept for the check).
type written struct {
	files []string
	image []byte
}

// write puts the item on the clipboard and says whether it wrote anything.
func (s *Service) write(it model.Item) (bool, written, error) {
	switch it.Kind {
	case model.KindText:
		return true, written{}, s.Clipboard.WriteText(model.Str(it.Text))

	case model.KindFileList:
		var resolved []string
		compact := 0
		legacyScore, rawScore := 0, 0
		for i, p := range it.FilePaths {
			if strings.TrimSpace(p) == "" {
				continue
			}
			name := baseName(p)
			if isFile(filepath.Join(s.Layout.Files(), it.IDN(), captureCacheName(compact, name))) {
				legacyScore++
			}
			if isFile(filepath.Join(s.Layout.Files(), it.IDN(), captureCacheName(i, name))) {
				rawScore++
			}
			compact++
		}
		legacyComplete := legacyScore > rawScore
		ambiguous := legacyScore == rawScore && len(nonBlank(it.FilePaths)) != len(it.FilePaths)

		compact = 0
		for i, p := range it.FilePaths {
			if strings.TrimSpace(p) == "" {
				continue
			}
			name := baseName(p)
			if strings.TrimSpace(name) == "" {
				continue
			}
			cached := resolveCachePath(s.Layout.Files(), it.IDN(), i, name)
			legacy := resolveCachePath(s.Layout.Files(), it.IDN(), compact, name)
			compact++
			if legacyComplete {
				cached = legacy
			}
			if ambiguous && cached != legacy {
				cached = ""
			}
			switch {
			case isFile(cached):
				resolved = append(resolved, cached)
			case isFile(p):
				resolved = append(resolved, p)
			}
		}
		if len(resolved) == 0 {
			return false, written{}, nil
		}
		return true, written{files: resolved}, s.Clipboard.WriteFiles(resolved)

	case model.KindImage:
		p := model.Str(it.ImageFilePath)
		if strings.TrimSpace(p) == "" || !isFile(p) {
			return false, written{}, nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return false, written{}, err
		}
		file := p
		if !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
			im, err := imagecodec.Decode(data)
			if err != nil {
				return false, written{}, err
			}
			// The canonical PNG: copied back, it has the fingerprint the item
			// was given (saveImage).
			data, err = imagecodec.CanonicalPNG(im)
			if err != nil {
				return false, written{}, err
			}
			// The file offered with the picture must be a PNG too: the stored
			// one is not, and a folder or a chat would get a WebP.
			file = s.clipboardPNG(p, data)
		}
		if err := s.Clipboard.WriteImage(data, file); err != nil {
			return true, written{image: data}, err
		}
		if file == p {
			file = "" // a stored PNG is offered: no copy is in use
		}
		s.pruneClipboardPNGs(file)
		return true, written{image: data}, nil
	}
	return false, written{}, errors.New("capture: unknown item kind")
}

// validateImageSize bounds decoded memory before any full decoder runs.
func validateImageSize(data []byte) error {
	c, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if c.Width <= 0 || c.Height <= 0 || int64(c.Width)*int64(c.Height) > 40_000_000 {
		return errors.New("image dimensions exceed capture limit")
	}
	return nil
}

func readBoundedImage(path string, limit int64) ([]byte, error) {
	if limit <= 0 || limit > 64*1024*1024 {
		limit = 64 * 1024 * 1024
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > limit {
		return nil, errors.New("encoded image exceeds capture limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("encoded image exceeds capture limit")
	}
	return data, err
}

func captureCacheName(index int, name string) string { return history.CaptureCacheName(index, name) }

func resolveCachePath(root, id string, index int, name string) string {
	legacy := filepath.Join(root, id, fmt.Sprintf("%04d_%s", index, name))
	if isFile(legacy) {
		return legacy
	}
	imported := filepath.Join(root, id, snapshot.ImportedFileName(index, name))
	if isFile(imported) {
		return imported
	}
	return filepath.Join(root, id, captureCacheName(index, name))
}
