// Package panel is Klip's main window: the search box, the list of history
// items, the "常用剪贴" folder of favourites and the bar below. The view is
// plain MyGo UI over a Model, so it runs in a window and, in tests, without
// one. Its looks and behaviour follow MainWindow of the .NET client.
package panel

import (
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/webp"

	"klip/internal/model"
)

// Match is MainWindowViewModel.Matches: the query is found, ignoring case, in
// the text, the note, the image's path or any file path.
func Match(it model.Item, query string) bool {
	q := strings.ToLower(query)
	has := func(s string) bool { return s != "" && strings.Contains(strings.ToLower(s), q) }
	if has(model.Str(it.Text)) || has(model.Str(it.Note)) || has(model.Str(it.ImageFilePath)) {
		return true
	}
	for _, p := range it.FilePaths {
		if has(p) {
			return true
		}
	}
	return false
}

// Filter is the items that match the query (trimmed; blank matches all), in
// the order of the history. Pinned ones are not moved to the top there.
func Filter(items []model.Item, query string) []model.Item {
	q := strings.TrimSpace(query)
	out := make([]model.Item, 0, len(items))
	for _, it := range items {
		if q == "" || Match(it, q) {
			out = append(out, it)
		}
	}
	return out
}

// Pinned keeps the favourites of a list.
func Pinned(items []model.Item) []model.Item {
	var out []model.Item
	for _, it := range items {
		if it.Pinned {
			out = append(out, it)
		}
	}
	return out
}

// SingleLine is TextToSingleLineConverter: each run of line breaks becomes
// one space, and the ends are trimmed.
func SingleLine(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	var b strings.Builder
	brk := false
	for _, r := range s {
		if r == '\r' || r == '\n' {
			if !brk {
				b.WriteByte(' ')
			}
			brk = true
			continue
		}
		brk = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

// baseName is the last element of a path of either kind.
func baseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return filepath.Base(p)
}

// FileSummary is FilePathsToSummaryStringConverter: the name of the first
// file, and how many more there are.
func FileSummary(paths []string) string {
	var arr []string
	for _, p := range paths {
		if strings.TrimSpace(p) != "" {
			arr = append(arr, p)
		}
	}
	if len(arr) == 0 {
		return ""
	}
	name := baseName(arr[0])
	if strings.TrimSpace(name) == "" || name == "." || name == "/" {
		name = arr[0]
	}
	if len(arr) == 1 {
		return name
	}
	return fmt.Sprintf("%s (+%d)", name, len(arr)-1)
}

// ExtensionTag is FilePathsToExtensionTagConverter: the extension of the
// first file in capitals, or FILE.
func ExtensionTag(paths []string) string {
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		ext := strings.TrimPrefix(strings.TrimSpace(filepath.Ext(strings.ReplaceAll(p, "\\", "/"))), ".")
		if ext == "" {
			return "FILE"
		}
		return strings.ToUpper(ext)
	}
	return ""
}

// Tag is the small label at the right of a row: the extension of a file, IMG
// for an image, nothing for text.
func Tag(it model.Item) string {
	switch it.Kind {
	case model.KindFileList:
		return ExtensionTag(it.FilePaths)
	case model.KindImage:
		return "IMG"
	}
	return ""
}

// RowText is the one line of a text or file row.
func RowText(it model.Item) string {
	if it.Kind == model.KindFileList {
		return FileSummary(it.FilePaths)
	}
	return SingleLine(headRunes(model.Str(it.Text), rowTextRunes))
}

// rowTextRunes is how much of a text a row looks at: the line is cut by the
// width of the window long before that, and a copied log of many megabytes
// must cost no more than a short note, in every frame.
const rowTextRunes = 2000

// headRunes is the first n characters of s, found without reading the rest.
func headRunes(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

// ImageInfo is ImagePathToImageInfoConverter: "IMG · 640×480 · 12 KB", or
// nothing when the file is gone.
func ImageInfo(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return ""
	}
	kb := max(1, st.Size()/1024)
	f, err := os.Open(path)
	if err != nil {
		return "IMG"
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return "IMG"
	}
	return fmt.Sprintf("IMG · %d×%d · %d KB", cfg.Width, cfg.Height, kb)
}

// infoCache keeps what ImageInfo found, so a row does not read its file in
// every frame. The file is read in the background, as the thumbnail is: it
// may be on a disk that answers slowly, and the window must not wait for it.
type infoCache struct {
	mu       sync.Mutex
	m        map[string]string
	pending  map[string]bool
	failedAt map[string]time.Time
}

// get returns what is known of the picture at path, or "" while it is being
// read (redraw is called when it has been).
func (c *infoCache) get(path string, redraw func()) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.m[path]; ok {
		if s != "" || time.Since(c.failedAt[path]) < 5*time.Second {
			return s
		}
		delete(c.m, path)
	}
	if path == "" || c.pending[path] {
		return ""
	}
	if c.pending == nil {
		c.pending = map[string]bool{}
	}
	c.pending[path] = true
	go func() {
		s := ImageInfo(path)
		c.mu.Lock()
		delete(c.pending, path)
		if c.m == nil || len(c.m) > 512 {
			c.m = map[string]string{}
		}
		c.m[path] = s
		if s == "" {
			if c.failedAt == nil {
				c.failedAt = map[string]time.Time{}
			}
			c.failedAt[path] = time.Now()
		}
		c.mu.Unlock()
		if redraw != nil {
			redraw()
		}
	}()
	return ""
}

// DragText is what the ghost of a dragged row says.
func DragText(it model.Item) string {
	switch it.Kind {
	case model.KindFileList:
		if n := len(it.FilePaths); n > 0 {
			return fmt.Sprintf("%d 个文件", n)
		}
		return "文件"
	case model.KindImage:
		return "图片"
	}
	t := strings.NewReplacer("\r\n", " ", "\n", " ").Replace(model.Str(it.Text))
	if r := []rune(t); len(r) > 42 {
		t = string(r[:42]) + "…"
	}
	if strings.TrimSpace(t) == "" {
		return "文本"
	}
	return t
}

// Size writes a byte count for people.
func Size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

var _ = time.Now
