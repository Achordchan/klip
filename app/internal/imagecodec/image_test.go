package imagecodec

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"
)

func TestWebPLosslessAndExportFormats(t *testing.T) {
	im := image.NewNRGBA(image.Rect(0, 0, 16, 12))
	im.SetNRGBA(2, 3, color.NRGBA{R: 231, G: 48, B: 65, A: 128})
	data, err := Encode(im, "webp")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data[:16], []byte("WEBP")) {
		t.Fatal("not webp")
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if color.NRGBAModel.Convert(decoded.At(2, 3)) != im.NRGBAAt(2, 3) {
		t.Fatal("lossless pixel changed")
	}
	src := filepath.Join(t.TempDir(), "source.webp")
	os.WriteFile(src, data, 0600)
	for _, ext := range []string{"png", "jpg", "webp"} {
		dst := filepath.Join(t.TempDir(), "export."+ext)
		if err := Export(src, dst); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(dst)
		got, err := Decode(b)
		if err != nil || got.Bounds() != im.Bounds() {
			t.Fatalf("%s: %v", ext, err)
		}
	}
	if err := Export(src, src); err == nil {
		t.Fatal("overwrote history image")
	}
}

func TestExportTellsAnotherCaseFromTheSameFileByTheFileSystem(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "picture.png")
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	data, err := Encode(img, "png")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Export(src, src); err == nil {
		t.Fatal("the history image itself was overwritten")
	}
	other := filepath.Join(dir, "Picture.png")
	a, _ := os.Stat(src)
	b, err := os.Stat(other)
	sameFile := err == nil && os.SameFile(a, b)
	err = Export(src, other)
	if sameFile && err == nil {
		t.Fatal("on a volume that ignores case, Picture.png is the history image: it was overwritten")
	}
	if !sameFile && err != nil {
		t.Fatalf("on a volume that tells case apart, Picture.png is another file: %v", err)
	}
}

// WithChunk puts a chunk right after the IHDR of a PNG, as an encoder that
// writes colour information does.
func withChunk(t *testing.T, png []byte, kind string, body []byte) []byte {
	t.Helper()
	const ihdrEnd = 8 + 8 + 13 + 4
	var c bytes.Buffer
	binary.Write(&c, binary.BigEndian, uint32(len(body)))
	c.WriteString(kind)
	c.Write(body)
	sum := crc32.NewIEEE()
	sum.Write([]byte(kind))
	sum.Write(body)
	binary.Write(&c, binary.BigEndian, sum.Sum32())
	out := append([]byte(nil), png[:ihdrEnd]...)
	out = append(out, c.Bytes()...)
	return append(out, png[ihdrEnd:]...)
}

func TestPNGColorInfo(t *testing.T) {
	plain, err := Encode(image.NewNRGBA(image.Rect(0, 0, 2, 2)), "png")
	if err != nil {
		t.Fatal(err)
	}
	gamma := func(g uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, g); return b }
	for name, tc := range map[string]struct {
		data []byte
		want bool
	}{
		"plain":       {plain, false},
		"ICC profile": {withChunk(t, plain, "iCCP", []byte("Display P3\x00\x00xyz")), true},
		"primaries":   {withChunk(t, plain, "cHRM", make([]byte, 32)), true},
		"code points": {withChunk(t, plain, "cICP", []byte{12, 13, 0, 1}), true},
		"gamma 1.8":   {withChunk(t, plain, "gAMA", gamma(55556)), true},
		"sRGB gamma":  {withChunk(t, plain, "gAMA", gamma(45455)), false},
		"sRGB":        {withChunk(t, plain, "sRGB", []byte{0}), false},
		"not a PNG":   {[]byte("GIF89a"), false},
	} {
		if got := PNGColorInfo(tc.data); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
		if tc.want {
			if _, err := Decode(tc.data); err != nil {
				t.Fatalf("%s: the test picture does not decode: %v", name, err)
			}
		}
	}
}

func TestExportKeepsTheColourOfAColourManagedPNG(t *testing.T) {
	plain, err := Encode(image.NewNRGBA(image.Rect(0, 0, 2, 2)), "png")
	if err != nil {
		t.Fatal(err)
	}
	tagged := withChunk(t, plain, "iCCP", []byte("Display P3\x00\x00xyz"))
	dir := t.TempDir()
	src := filepath.Join(dir, "p3.png")
	if err := os.WriteFile(src, tagged, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "copy.png")
	if err := Export(src, out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, tagged) {
		t.Fatal("the PNG was encoded again and lost its profile")
	}
	for _, ext := range []string{".jpg", ".webp"} {
		if err := Export(src, filepath.Join(dir, "copy"+ext)); err == nil {
			t.Errorf("%s: a colour-managed picture was exported without its profile", ext)
		}
	}
	// A picture without colour information still converts.
	plainSrc := filepath.Join(dir, "plain.png")
	os.WriteFile(plainSrc, plain, 0o600)
	if err := Export(plainSrc, filepath.Join(dir, "plain.webp")); err != nil {
		t.Fatal(err)
	}
}
