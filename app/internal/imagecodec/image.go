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
	if strings.EqualFold(src, dst) {
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
