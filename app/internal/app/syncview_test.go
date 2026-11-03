package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"klip/internal/syncer"
)

func TestSyncViewIconAndTip(t *testing.T) {
	cases := []struct {
		v    SyncView
		icon string
		tip  string
	}{
		{SyncView{}, "idle", "未启用云同步"},
		{SyncView{Cloud: true}, "idle", "已启用云同步\n等待首次同步"},
		{SyncView{Cloud: true, Syncing: true}, "working", "同步中"},
		{SyncView{Cloud: true, Syncing: true, LastStatus: "后台同步：合并历史"}, "working", "后台同步：合并历史"},
		{SyncView{Cloud: true, Syncing: true, Progress: true, Text: "上传快照", Percent: 84, LastStatus: "同步中"}, "working", "同步中\n上传快照 84%"},
		{SyncView{Cloud: true, Result: SyncOK, LastStatus: "同步成功", LastAt: time.Date(2026, 5, 30, 18, 1, 2, 0, time.Local)}, "success", "同步成功\n最近同步：2026-05-30 18:01:02"},
		{SyncView{Cloud: true, Result: SyncFailed, LastAt: time.Date(2026, 5, 30, 18, 1, 2, 0, time.Local)}, "error", "同步\n最近同步：2026-05-30 18:01:02"},
		// a result is not shown for a signed-out client, nor while the bar is up
		{SyncView{Result: SyncOK}, "idle", "未启用云同步"},
		{SyncView{Cloud: true, Result: SyncOK, Progress: true}, "idle", ""},
	}
	for i, c := range cases {
		if got := c.v.Icon(); got != c.icon {
			t.Errorf("case %d: icon %q, want %q", i, got, c.icon)
		}
		if c.tip != "" {
			if got := c.v.Tip(); got != c.tip {
				t.Errorf("case %d: tip %q, want %q", i, got, c.tip)
			}
		}
	}
}

func newBare(t *testing.T) (*App, *[]string) {
	t.Helper()
	var toasts []string
	a := &App{cfg: Config{Logf: func(string, ...any) {}, Toast: func(s string) { toasts = append(toasts, s) }}}
	a.acctCtx, a.acctCancel = context.WithCancel(context.Background())
	t.Cleanup(a.acctCancel)
	return a, &toasts
}

func TestSyncProgressOfManualAndBackground(t *testing.T) {
	a, _ := newBare(t)
	a.syncBegin("manual", true)
	if v := a.copyView(); !v.Syncing || !v.Progress || v.Text != "准备中" {
		t.Fatalf("begin: %+v", v)
	}
	a.syncProgress(syncer.Progress{Code: "upload_snapshot", Percent: 84}, "manual", true)
	if v := a.copyView(); v.Text != "上传快照" || v.Percent != 84 {
		t.Fatalf("progress: %+v", v)
	}
	a.syncProgress(syncer.Progress{Code: "x", Message: "自定义", Percent: 400}, "manual", true)
	if v := a.copyView(); v.Text != "自定义" || v.Percent != 100 {
		t.Fatalf("unknown stage: %+v", v)
	}
	start := time.Now()
	a.syncDone("manual", true, nil)
	if time.Since(start) > 150*time.Millisecond {
		t.Fatalf("syncDone took %v: the pause is a timer, the runner is held until this returns", time.Since(start))
	}
	// The bar stays at 100% for a moment, then goes.
	if v := a.copyView(); !v.Progress || v.Text != "完成" || v.Percent != 100 {
		t.Fatalf("at the end: %+v", v)
	}
	time.Sleep(450 * time.Millisecond)
	v := a.copyView()
	if v.Syncing || v.Progress || v.Text != "" || v.Result != SyncOK || v.LastStatus != "同步成功" || v.LastAt.IsZero() {
		t.Fatalf("done: %+v", v)
	}
	if a.SyncStatus() == "" || !strings.HasPrefix(a.SyncStatus(), "已同步") {
		t.Fatalf("status = %q", a.SyncStatus())
	}

	// A background sync does not show the bar; its stage goes to the tip.
	a.syncBegin("auto", false)
	a.syncProgress(syncer.Progress{Code: "merge_snapshot"}, "auto", false)
	if v := a.copyView(); v.Progress || v.LastStatus != "后台同步：合并历史" || !v.Syncing {
		t.Fatalf("background: %+v", v)
	}
	a.syncDone("auto", false, nil)
	if v := a.copyView(); v.LastStatus != "后台同步成功" || v.Syncing {
		t.Fatalf("background done: %+v", v)
	}
}

func TestSyncFailureToasts(t *testing.T) {
	a, toasts := newBare(t)
	// the first background sync that fails says so; later ones do not
	a.syncBegin("startup", false)
	a.syncDone("startup", false, errors.New("boom"))
	if len(*toasts) != 1 || !strings.HasPrefix((*toasts)[0], "首次同步失败：") {
		t.Fatalf("toasts = %v", *toasts)
	}
	if v := a.copyView(); v.Result != SyncFailed || v.LastStatus != "后台同步失败" {
		t.Fatalf("view: %+v", v)
	}
	a.initialOK.Store(true)
	a.syncDone("auto", false, errors.New("boom"))
	if len(*toasts) != 1 {
		t.Fatalf("a later background failure toasted: %v", *toasts)
	}
	// one the user asked for always does, and the bar shows the failure first
	a.syncBegin("manual", true)
	start := time.Now()
	a.syncDone("manual", true, errors.New("boom"))
	if len(*toasts) != 2 || time.Since(start) > 150*time.Millisecond {
		t.Fatalf("manual failure: toasts=%v after %v", *toasts, time.Since(start))
	}
	if v := a.copyView(); !v.Progress || v.Text != "同步失败" {
		t.Fatalf("the bar shows the failure first: %+v", v)
	}
	time.Sleep(650 * time.Millisecond)
	if v := a.copyView(); v.Progress || v.Syncing || v.LastStatus != "同步失败" {
		t.Fatalf("view: %+v", v)
	}

	a.syncDone("manual", true, syncer.ErrNotLoggedIn)
	if got := (*toasts)[len(*toasts)-1]; got != "请先登录" || a.SyncStatus() != "请先登录" {
		t.Fatalf("not logged in: %v %q", *toasts, a.SyncStatus())
	}
}

func TestOldBarDoesNotOutliveItsSyncOrHideTheNext(t *testing.T) {
	a, _ := newBare(t)
	a.syncBegin("manual", true)
	a.syncDone("manual", true, nil) // the bar stays up for a moment
	a.syncBegin("auto", false)      // a background sync begins inside that moment
	if v := a.copyView(); v.Progress || v.Text != "" || v.Percent != 0 || !v.Syncing {
		t.Fatalf("the background sync inherited the bar: %+v", v)
	}
	time.Sleep(450 * time.Millisecond) // the timer of the first one goes off
	if v := a.copyView(); !v.Syncing {
		t.Fatalf("the old timer took down the view of the sync that is running: %+v", v)
	}
}
