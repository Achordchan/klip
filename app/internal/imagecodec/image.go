package imagecodec

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/HugoSmits86/nativewebp"
	_ "golang.org/x/image/webp"
	"klip/internal/durable"
)

const MaxBytes = 64 << 20

func Decode(data []byte) (image.Image, error) {
	if len(data) > MaxBytes {
		return nil, errors.New("图片文件超过64MB")
	}
	c, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if c.Width <= 0 || c.Height <= 0 || int64(c.Width)*int64(c.Height) > 40_000_000 {
		return nil, errors.New("图片像素超过处理上限")
	}
	im, _, err := image.Decode(bytes.NewReader(data))
	return im, err
}
func Encode(im image.Image, format string) ([]byte, error) {
	var b bytes.Buffer
	var err error
	switch strings.ToLower(format) {
	case ".webp", "webp":
		err = nativewebp.Encode(&b, im, &nativewebp.Options{CompressionLevel: nativewebp.DefaultCompression})
	case ".png", "png":
		err = png.Encode(&b, im)
	case ".jpg", ".jpeg", "jpg", "jpeg":
		flat := image.NewRGBA(im.Bounds())
		draw.Draw(flat, flat.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
		draw.Draw(flat, flat.Bounds(), im, im.Bounds().Min, draw.Over)
		err = jpeg.Encode(&b, flat, &jpeg.Options{Quality: 95})
	default:
		return nil, errors.New("请选择 PNG、JPEG 或 WebP 格式")
	}
	return b.Bytes(), err
}
func Read(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if len(b) > MaxBytes {
		return nil, errors.New("图片文件超过64MB")
	}
	return b, err
}
func Export(source, target string) error {
	src, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	dst, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	// The same path, as spelled; whether another spelling names the same file
	// (case on a volume that ignores it, a link) is the file system's to say.
	if filepath.Clean(src) == filepath.Clean(dst) {
		return errors.New("请另选文件名，不能覆盖历史图片")
	}
	a, ae := os.Stat(src)
	b, be := os.Stat(dst)
	if ae == nil && be == nil && os.SameFile(a, b) {
		return errors.New("不能覆盖历史图片")
	}
	data, err := Read(src)
	if err != nil {
		return err
	}
	im, err := Decode(data)
	if err != nil {
		return err
	}
	data, err = Encode(im, filepath.Ext(dst))
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".klip-export-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return durable.Rename(f.Name(), dst)
}

// HighDepth reports more than 8 bits per channel, which lossless WebP cannot
// keep.
func HighDepth(im image.Image) bool {
	switch im.(type) {
	case *image.RGBA64, *image.NRGBA64, *image.Gray16:
		return true
	}
	switch im.ColorModel() {
	case color.RGBA64Model, color.NRGBA64Model, color.Gray16Model:
		return true
	}
	return false
}

// Normalize returns the 8-bit non-premultiplied pixels of im, starting at 0,0.
// Lossless WebP stores exactly these, so a picture normalized, stored as WebP
// and decoded again gives the same pixels back.
func Normalize(im image.Image) *image.NRGBA {
	if n, ok := im.(*image.NRGBA); ok && n.Rect.Min == (image.Point{}) {
		return n
	}
	b := im.Bounds()
	n := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(n, n.Rect, im, b.Min, draw.Src)
	return n
}

// CanonicalPNG is the PNG of the normalized pixels: the bytes Klip puts on the
// clipboard for a picture it does not keep as PNG, and so the identity of such
// a picture when it is copied back.
func CanonicalPNG(im image.Image) ([]byte, error) {
	return Encode(Normalize(im), "png")
}

// WebPMaxDimension is the largest width or height a WebP image can have.
const WebPMaxDimension = 16384

// FitsWebP says whether a picture of these bounds can be stored as WebP (a
// long scrolling screenshot may be taller).
func FitsWebP(b image.Rectangle) bool {
	return b.Dx() > 0 && b.Dy() > 0 && b.Dx() <= WebPMaxDimension && b.Dy() <= WebPMaxDimension
}

// PNGColorInfo says whether a PNG says how its values are to be shown other
// than as sRGB: an ICC profile (iCCP; a Display P3 screenshot), primaries
// (cHRM), coding-independent code points (cICP) or a gamma (gAMA) other than
// sRGB's. WebP as Klip writes it keeps none of these, so such a picture would
// be shown in other colours: it is kept as PNG.
func PNGColorInfo(data []byte) bool {
	const sig = "\x89PNG\r\n\x1a\n"
	if len(data) < len(sig) || string(data[:len(sig)]) != sig {
		return false
	}
	for p := len(sig); p+8 <= len(data); {
		n := int(uint32(data[p])<<24 | uint32(data[p+1])<<16 | uint32(data[p+2])<<8 | uint32(data[p+3]))
		kind := string(data[p+4 : p+8])
		body := p + 8
		if n < 0 || body+n+4 > len(data) {
			return false
		}
		switch kind {
		case "iCCP", "cHRM", "cICP":
			return true
		case "gAMA":
			if n == 4 {
				g := uint32(data[body])<<24 | uint32(data[body+1])<<16 | uint32(data[body+2])<<8 | uint32(data[body+3])
				if g != 45455 { // 1/2.2, what sRGB has
					return true
				}
			}
		case "IDAT", "IEND":
			return false // the colour chunks come before the image data
		}
		p = body + n + 4
	}
	return false
}
