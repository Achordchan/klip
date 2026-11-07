package imagecodec

import (
	"bytes"
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
