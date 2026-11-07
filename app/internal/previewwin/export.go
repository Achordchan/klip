package previewwin

import (
	"github.com/egoist/mygo"
	"klip/internal/imagecodec"
	"path/filepath"
	"strings"
)

func (mg *Manager) saveImageAs(source string) {
	go func() {
		target, err := mygo.Dialog.Save(mygo.SaveDialogOptions{Title: "图片另存为", DefaultPath: strings.TrimSuffix(filepath.Base(source), filepath.Ext(source)) + ".png", Filters: []mygo.FileFilter{{Name: "PNG 图片", Extensions: []string{"png"}}, {Name: "JPEG 图片", Extensions: []string{"jpg", "jpeg"}}, {Name: "WebP 图片", Extensions: []string{"webp"}}}})
		if err == nil && target == "" {
			return
		}
		if err == nil {
			if filepath.Ext(target) == "" {
				target += ".png"
			}
			err = imagecodec.Export(source, target)
		}
		if err != nil {
			mygo.Dialog.Message(mygo.MessageOptions{Type: mygo.MessageError, Title: "另存为失败", Message: err.Error()})
		}
	}()
}
