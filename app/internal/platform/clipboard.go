package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/transfer"
)

// Clipboard is MyGo's clipboard as capture.Clipboard. MyGo hops every call to
// its main thread, so it is safe from the watcher goroutine, but only once the
// application is running (App.WhenReady).
type Clipboard struct{}

// Formats lists the portable format names on the clipboard.
func (Clipboard) Formats() []string {
	var out []string
	for _, f := range mygo.Clipboard.Formats() {
		out = append(out, string(f))
	}
	return out
}

func (Clipboard) ReadText() string { return mygo.Clipboard.ReadText() }

// ReadFiles returns the local paths of copied files; "nothing there" is not an
// error.
func (Clipboard) ReadFiles() ([]string, error) {
	files, err := mygo.Clipboard.ReadFiles()
	if err != nil && errors.Is(err, transfer.ErrFormat) {
		return nil, nil
	}
	return files, err
}

func (Clipboard) ReadImage() []byte { return mygo.Clipboard.ReadImage() }

func (Clipboard) WriteText(text string) error {
	return mygo.Clipboard.Write(transfer.TextData(text))
}

func (Clipboard) WriteFiles(paths []string) error {
	return mygo.Clipboard.WriteFiles(paths...)
}

// WriteImage puts the PNG on the clipboard, and, when filePath names an
// existing file, offers that file too, so a paste into a folder or a chat
// window gets the file and a paste into an editor gets the picture (the
// C# client does the same).
func (Clipboard) WriteImage(png []byte, filePath string) error {
	if filePath != "" && filepath.IsAbs(filePath) {
		if _, err := os.Stat(filePath); err == nil {
			p := filepath.ToSlash(filePath)
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			u := (&url.URL{Scheme: "file", Path: p}).String()
			item := transfer.NewItem(
				transfer.Bytes(transfer.PNG, png),
				transfer.Bytes(transfer.FileList, []byte(u)),
				transfer.Bytes(transfer.URIList, []byte(u)),
			)
			if err := mygo.Clipboard.Write(transfer.New(item)); err == nil {
				return nil
			}
		}
	}
	return mygo.Clipboard.WriteImage(png)
}

// ContentFingerprint is a hash of what is on the clipboard (its formats, the
// text, the file list and the picture), for the platforms that have no change
// counter: the Watcher asks for a change only when it differs.
func ContentFingerprint() string {
	c := Clipboard{}
	formats := c.Formats()
	if len(formats) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(strings.Join(formats, "\x00")))
	h.Write([]byte{1})
	text := c.ReadText()
	h.Write([]byte(text))
	h.Write([]byte{1})
	files, err := c.ReadFiles()
	if err == nil {
		h.Write([]byte(strings.Join(files, "\x00")))
		for _, p := range files {
			if st, err := os.Stat(p); err == nil {
				fmt.Fprintf(h, "\x00%d:%d", st.Size(), st.ModTime().UnixNano())
			}
		}
	}
	h.Write([]byte{1})
	// The picture always counts: which kinds are captured is a setting that
	// may change, and screenshot tools name their format in odd ways.
	picture := c.ReadImage()
	if len(files) == 0 && len(picture) == 0 {
		if text == "" {
			for _, f := range formats {
				if strings.EqualFold(f, "text/plain") || strings.EqualFold(f, "text") || strings.EqualFold(f, "unicodetext") {
					return ""
				}
			}
		}
	}

	for _, format := range formats {
		f := strings.ToLower(format)
		if (strings.Contains(f, "image") || strings.Contains(f, "bitmap") || strings.Contains(f, "dib")) && len(picture) == 0 && strings.TrimSpace(text) == "" && len(files) == 0 {
			return ""
		}
		if (strings.Contains(f, "uri-list") || strings.Contains(f, "file")) && err != nil {
			return ""
		}
	}
	h.Write(picture)
	return hex.EncodeToString(h.Sum(nil))
}
