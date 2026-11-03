//go:build windows

package shelfwin

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/egoist/mygo/ui"
)

var (
	shell32            = syscall.NewLazyDLL("shell32.dll")
	user32             = syscall.NewLazyDLL("user32.dll")
	gdi32              = syscall.NewLazyDLL("gdi32.dll")
	procSHGetFileInfoW = shell32.NewProc("SHGetFileInfoW")
	procDestroyIcon    = user32.NewProc("DestroyIcon")
	procGetIconInfo    = user32.NewProc("GetIconInfo")
	procGetDC          = user32.NewProc("GetDC")
	procReleaseDC      = user32.NewProc("ReleaseDC")
	procGetObjectW     = gdi32.NewProc("GetObjectW")
	procGetDIBits      = gdi32.NewProc("GetDIBits")
	procDeleteObject   = gdi32.NewProc("DeleteObject")
)

const (
	shgfiIcon              = 0x100
	shgfiLargeIcon         = 0x0
	shgfiUseFileAttributes = 0x10
	fileAttributeDirectory = 0x10
	fileAttributeNormal    = 0x80
	dibRGBColors           = 0
)

type shFileInfo struct {
	HIcon       uintptr
	IIcon       int32
	Attributes  uint32
	DisplayName [260]uint16
	TypeName    [80]uint16
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

type bitmap struct {
	Type       int32
	Width      int32
	Height     int32
	WidthBytes int32
	Planes     uint16
	BitsPixel  uint16
	Bits       uintptr
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

var (
	iconMu    sync.Mutex
	iconCache = map[string]*ui.Bitmap{}
)

// SystemIcon is FileIconService.GetLargeIcon: the icon the shell shows for
// the file (by its type, so it needs no file to read), cached. nil when there
// is none.
func SystemIcon(path string) *ui.Bitmap {
	key := iconKey(path)
	iconMu.Lock()
	if bm, ok := iconCache[key]; ok {
		iconMu.Unlock()
		return bm
	}
	iconMu.Unlock()

	var bm *ui.Bitmap
	if _, err := os.Stat(path); err == nil {
		if img := shellIcon(path); img != nil {
			var buf bytes.Buffer
			if png.Encode(&buf, img) == nil {
				bm, _ = ui.DecodeBitmap(buf.Bytes())
			}
		}
	}
	iconMu.Lock()
	if len(iconCache) >= iconCacheMax {
		iconCache = map[string]*ui.Bitmap{} // a bound on the memory; icons are cheap to ask for again
	}
	iconCache[key] = bm
	iconMu.Unlock()
	return bm
}

// iconCacheMax is how many icons are kept.
const iconCacheMax = 256

// iconKey says which icons are the same: the shell draws one icon for all the
// files of a type, so many files cost one entry. Programs, shortcuts and icons
// carry their own and are kept by path.
func iconKey(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".exe", ".lnk", ".ico", ".url", ".scr", ".cur", ".ani":
		return strings.ToLower(path)
	case "":
		if st, err := os.Stat(path); err == nil && st.IsDir() {
			return "<dir>"
		}
	}
	return "ext:" + ext
}

func shellIcon(path string) image.Image {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ole := syscall.NewLazyDLL("ole32.dll")
	hr, _, _ := ole.NewProc("CoInitializeEx").Call(0, 2)
	if int32(hr) >= 0 {
		defer ole.NewProc("CoUninitialize").Call()
	} else if uint32(hr) != 0x80010106 {
		return nil
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil
	}
	attrs := uintptr(fileAttributeNormal)
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		attrs = fileAttributeDirectory
	}
	flags := uintptr(shgfiIcon | shgfiLargeIcon | shgfiUseFileAttributes)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".exe", ".lnk", ".ico", ".url", ".scr", ".cur", ".ani":
		flags &^= shgfiUseFileAttributes
	}
	var info shFileInfo
	r, _, _ := procSHGetFileInfoW.Call(uintptr(unsafe.Pointer(p)), attrs, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info),
		flags)
	if r == 0 || info.HIcon == 0 {
		return nil
	}
	defer procDestroyIcon.Call(info.HIcon)

	var ii iconInfo
	if r, _, _ := procGetIconInfo.Call(info.HIcon, uintptr(unsafe.Pointer(&ii))); r == 0 {
		return nil
	}
	defer func() {
		if ii.HbmColor != 0 {
			procDeleteObject.Call(ii.HbmColor)
		}
		if ii.HbmMask != 0 {
			procDeleteObject.Call(ii.HbmMask)
		}
	}()
	if ii.HbmColor == 0 {
		return nil
	}
	var bm bitmap
	if r, _, _ := procGetObjectW.Call(ii.HbmColor, unsafe.Sizeof(bm), uintptr(unsafe.Pointer(&bm))); r == 0 {
		return nil
	}
	w, h := int(bm.Width), int(bm.Height)
	if w <= 0 || h <= 0 || w > 1024 || h > 1024 {
		return nil
	}
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return nil
	}
	defer procReleaseDC.Call(0, hdc)

	hdr := bitmapInfoHeader{Size: uint32(unsafe.Sizeof(bitmapInfoHeader{})), Width: int32(w), Height: -int32(h), Planes: 1, BitCount: 32}
	pix := make([]byte, w*h*4)
	if r, _, _ := procGetDIBits.Call(hdc, ii.HbmColor, 0, uintptr(h), uintptr(unsafe.Pointer(&pix[0])), uintptr(unsafe.Pointer(&hdr)), dibRGBColors); r == 0 {
		return nil
	}
	// BGRA to RGBA. An icon without an alpha channel has all of it zero.
	opaque := true
	for i := 0; i < len(pix); i += 4 {
		if pix[i+3] != 0 {
			opaque = false
			break
		}
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(pix); i += 4 {
		a := pix[i+3]
		if opaque {
			a = 255
		}
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = pix[i+2], pix[i+1], pix[i], a
	}
	return img
}
