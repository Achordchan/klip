package main

import (
	"context"
	"fmt"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"klip/internal/paths"
	"klip/internal/storage"
	"klip/internal/update"
)

func (p *program) changeStorage() {
	if !p.storageMoving.CompareAndSwap(false, true) {
		return
	}
	started := false
	defer func() {
		if !started {
			p.storageMoving.Store(false)
		}
	}()
	phase := p.app.Updates.State().Phase
	if phase == update.Downloading || phase == update.Applying {
		mygo.Dialog.Message(mygo.MessageOptions{Parent: p.prefsWin, Title: "暂时无法迁移", Message: "请等待更新任务结束后再迁移。"})
		return
	}
	selected, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: p.prefsWin, Title: "选择新的空存储文件夹", DefaultPath: p.layout.Root, Directory: true})
	if err != nil || len(selected) == 0 {
		return
	}
	target, err := storage.Validate(p.layout.Root, selected[0])
	if err != nil {
		mygo.Dialog.Message(mygo.MessageOptions{Parent: p.prefsWin, Type: mygo.MessageError, Title: "无法迁移", Message: err.Error()})
		return
	}
	answer, err := mygo.Dialog.Message(mygo.MessageOptions{Parent: p.prefsWin, Type: mygo.MessageQuestion, Title: "迁移存储位置", Message: "将迁移全部历史记录、图片、文件及设置，然后重启 Klip。", Detail: "新位置：" + target + "\n旧目录保留为备份，不会自动删除。", Buttons: []string{"取消", "迁移并重启"}, DefaultButton: 0, CancelButton: 0})
	if err != nil || answer.Button != 1 {
		return
	}
	selection, err := paths.SelectionPath()
	if err != nil {
		return
	}
	started = true
	// A dedicated non-closable window keeps the process responsive while all
	// mutable application windows and background workers are stopped.
	p.hide()
	p.prefsWin.Hide()
	p.stopShelf()
	status := "正在停止采集和同步…"
	progress := mygo.NewWindow(mygo.WindowOptions{Title: "迁移存储", Width: 420, Height: 140, UseContentSize: true, DisableResize: true, AlwaysOnTop: true, Content: ui.View(func(c *ui.Context) {
		ui.Column(c).Fill().Padding(20).Gap(12).Children(func() { ui.Text(c, "正在迁移存储位置").FontSize(16); ui.Text(c, status).FontSize(13).Wrap() })
	})})
	progress.OnClose(func(e *mygo.CloseEvent) {
		if !p.quitting.Load() {
			e.PreventDefault()
		}
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p.prefs.Wait()
		err := p.app.StopForMigration(ctx)
		if err == nil {
			err = storage.Migrate(p.layout.Root, target, selection, func(done, total int) {
				progress.Update(func() { status = fmt.Sprintf("正在复制并校验文件：%d / %d", done, total) })
			})
		}
		mygo.RunOnMain(func() {
			progress.Hide()
			if err != nil {
				mygo.Dialog.Message(mygo.MessageOptions{Type: mygo.MessageError, Title: "迁移未完成", Message: err.Error(), Detail: "旧目录和数据已保留，Klip 将重新启动。"})
			}
			mygo.App.Relaunch()
		})
	}()
}
