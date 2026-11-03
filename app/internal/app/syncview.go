package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"klip/internal/model"
	"klip/internal/syncer"
)

// SyncResult is how the last sync ended (MainWindowViewModel.SyncResultKind).
type SyncResult int

const (
	SyncNone SyncResult = iota
	SyncOK
	SyncFailed
)

// SyncView is what the main window shows of the sync: the cloud icon, the
// progress bar of a sync the user asked for, and the text of the icon's tip.
type SyncView struct {
	// Cloud says that the history is synchronized with an account.
	Cloud bool
	// Syncing is true while a sync runs, of either kind.
	Syncing bool
	// Progress is shown for a sync the user asked for; Text and Percent say
	// where it is.
	Progress bool
	Text     string
	Percent  int
	Result   SyncResult
	LastAt   time.Time
	// LastStatus is the words of the last sync or of the stage a background
	// one is at.
	LastStatus string
}

// Icon is the cloud that is drawn: idle, working, success or error.
func (v SyncView) Icon() string {
	working := v.Cloud && v.Syncing
	switch {
	case working:
		return "working"
	case v.Cloud && !v.Progress && v.Result == SyncOK:
		return "success"
	case v.Cloud && !v.Progress && v.Result == SyncFailed:
		return "error"
	}
	return "idle"
}

// ProgressText is the words beside the bar: the stage and the percentage.
func (v SyncView) ProgressText() string {
	if strings.TrimSpace(v.Text) == "" {
		return ""
	}
	return v.Text + " " + itoa(v.Percent) + "%"
}

// Tip is the tool tip of the cloud icon.
func (v SyncView) Tip() string {
	if v.Syncing {
		working := strings.TrimSpace(v.LastStatus)
		if working == "" {
			working = "同步中"
		}
		if p := v.ProgressText(); p != "" {
			return working + "\n" + p
		}
		return working
	}
	if !v.Cloud {
		return "未启用云同步"
	}
	if v.LastAt.IsZero() {
		return "已启用云同步\n等待首次同步"
	}
	status := strings.TrimSpace(v.LastStatus)
	if status == "" {
		status = "同步"
	}
	return status + "\n最近同步：" + v.LastAt.Format("2006-01-02 15:04:05")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// Sync returns the state of the sync for the window.
func (a *App) Sync() SyncView {
	v := a.copyView()
	v.Cloud = a.Syncer.CanAutoSync()
	return v
}

func (a *App) copyView() SyncView {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.view
}

func (a *App) update(f func(v *SyncView)) {
	a.mu.Lock()
	f(&a.view)
	a.mu.Unlock()
	a.changed()
}

func (a *App) toast(msg string) {
	if a.cfg.Toast != nil && msg != "" {
		a.cfg.Toast(msg)
	}
}

// stageNames are the words for the steps of a sync.
var stageNames = map[string]string{
	"prepare_local":     "准备本地",
	"backup_snapshot":   "备份本地快照",
	"export_snapshot":   "导出本地快照",
	"upload_blobs":      "上传二进制对象",
	"read_snapshot":     "读取云端快照",
	"merge_snapshot":    "合并历史",
	"import_snapshot":   "导入本地快照",
	"upload_snapshot":   "上传快照",
	"download_snapshot": "下载快照",
	"persist_local":     "写入本地",
	"check_remote":      "检查远端",
}

func (a *App) syncBegin(reason string, manual bool) {
	a.mu.Lock()
	a.viewSeq++ // a pause of the one before ends nothing of this
	a.mu.Unlock()
	a.update(func(v *SyncView) {
		v.Syncing = true
		// Whatever the one before left on the bar (it stays up a moment after a
		// manual sync) is not this sync's.
		v.Progress, v.Text, v.Percent = false, "", 0
		if manual {
			v.Progress = true
			v.Text, v.Percent = "准备中", 5
		}
	})
	a.mu.Lock()
	a.status = "同步中…"
	a.mu.Unlock()
}

func (a *App) syncProgress(p syncer.Progress, reason string, manual bool) {
	stage := stageNames[p.Code]
	if stage == "" {
		stage = strings.TrimSpace(p.Message)
	}
	if stage == "" {
		stage = "同步中"
	}
	pct := max(0, min(p.Percent, 100))
	a.update(func(v *SyncView) {
		if manual {
			v.Text, v.Percent = stage, pct
		} else {
			v.LastStatus = "后台同步：" + stage
		}
	})
}

// finishView shows how a sync ended (syncDone has dealt with the accounting
// and what is owed). The pauses are those of the C# client: the bar stays at
// 100% for a moment, and longer when the sync failed. The pause is a timer, not
// a sleep: this runs before the runner is released, and the next sync (the one
// that a sign-in is owed) must not be turned away for it.
func (a *App) finishView(reason string, manual bool, err error) {
	a.mu.Lock()
	seq := a.viewSeq
	a.mu.Unlock()
	// closeBar ends the display of the sync, unless another has begun since.
	closeBar := func(after time.Duration) {
		end := func() {
			// The check and the change are one step: a sync that began in
			// between must not find its bar taken down.
			a.mu.Lock()
			same := a.viewSeq == seq
			if same {
				a.view.Syncing = false
				if a.view.Progress {
					a.view.Progress, a.view.Text, a.view.Percent = false, "", 0
				}
			}
			a.mu.Unlock()
			if same {
				a.changed()
			}
		}
		if after <= 0 {
			end()
			return
		}
		time.AfterFunc(after, end)
	}
	if manual && errors.Is(err, syncer.ErrNotLoggedIn) {
		a.mu.Lock()
		a.status = "请先登录"
		a.view.Syncing, a.view.Progress, a.view.Text, a.view.Percent = false, false, "", 0
		a.mu.Unlock()
		a.changed()
		a.toast("请先登录")
		return
	}
	if err == nil {
		if manual {
			a.update(func(v *SyncView) { v.Text, v.Percent = "完成", 100 })
		}
		a.update(func(v *SyncView) {
			v.Result, v.LastAt = SyncOK, time.Now()
			v.LastStatus = map[bool]string{true: "同步成功", false: "后台同步成功"}[manual]
		})
		a.mu.Lock()
		a.status = "已同步 " + time.Now().Format("15:04")
		a.mu.Unlock()
		if manual {
			closeBar(300 * time.Millisecond)
		} else {
			closeBar(0)
		}
		return
	}
	a.cfg.Logf("sync (%s): %v", reason, err)
	if manual {
		a.update(func(v *SyncView) { v.Text, v.Percent = "同步失败", 100 })
	}
	a.update(func(v *SyncView) {
		v.Result, v.LastAt = SyncFailed, time.Now()
		v.LastStatus = map[bool]string{true: "同步失败", false: "后台同步失败"}[manual]
	})
	explain := syncer.Explain(err, syncer.InMainWindow)
	a.mu.Lock()
	a.status = "同步失败"
	if manual {
		a.status = explain
	}
	a.mu.Unlock()
	switch {
	case manual:
		a.toast(explain)
	case !a.initialOK.Load():
		a.toast("首次同步失败：" + explain)
	}
	if manual {
		closeBar(450 * time.Millisecond)
	} else {
		closeBar(0)
	}
}

// remove deletes items from the history (pick says which of the history as it
// is when they are removed; it runs with the history locked, and must not use
// it) and, when there is an account, uploads the result at once
// (UploadCloudAfterLocalMutationAsync). The removal and its upload are one
// piece of work that waits for the sync that may be running and keeps the
// others out, so that no sync can put the deleted items back or share its
// fingerprints with the upload; it belongs to the account that is signed in,
// and Stop waits for it.
//
// The deletion is written down (pendingDeletes) for the account signed in as
// soon as the user asks, before waiting for its turn, and what more the
// removal takes when it is made is written down before it goes. It is marked
// as the server's only once the upload went through: a wait that is cut short
// (a quit, a change of account), a failed or cancelled upload, a crash leave
// it owed, and every later sync of that account leaves those items out of
// what it brings in and uploads the result.
//
// The items are chosen, written down and removed in one step with the history
// locked: a copy made meanwhile, which may keep the id of the item it copies
// again, is either there when they are chosen (and is what is written down
// and removed) or comes after (and stays).
func (a *App) remove(pick func(items []model.Item) []model.Item) {
	// What the user asked for, as it is now: what is removed when the account
	// changes before this one's turn comes (the history may then hold the
	// next account's items, which the user did not ask about).
	asked := pick(a.History.Items())
	if !a.Syncer.CanAutoSync() {
		a.mutateQuietly(func() { a.History.RemoveSelected(pick) })
		return
	}
	ctx, gen, owner := a.acctWork()
	if err := a.pending.Add(owner, a.History.Items(), asked...); err != nil {
		// Still owed for as long as the program runs.
		a.cfg.Logf("pending deletions: %v", err)
	}
	a.done.Add(1)
	go func() {
		defer a.done.Done()
		ran := a.Auto.Exclusive(ctx, func() {
			a.runGen.Store(gen) // the account this upload is for
			a.runOwner.Store(&owner)
			a.runActive.Store(true)
			defer a.runActive.Store(false)
			var items []model.Item
			a.mutateQuietly(func() {
				items = a.History.RemoveSelected(func(all []model.Item) []model.Item {
					got := pick(all)
					if err := a.pending.Add(owner, all, notCovered(got, asked)...); err != nil {
						a.cfg.Logf("pending deletions: %v", err)
					}
					return got
				})
			})
			if len(items) == 0 {
				return // already gone; what is owed goes with the next sync
			}
			if err := a.pending.Ready(); err != nil {
				a.cfg.Logf("pending deletions: %v", err)
				a.retryLater()
				return
			}
			// The upload, once through, tells the deletions it took (Uploaded).
			if _, err := a.Syncer.UploadAfterLocalMutation(ctx); err != nil && ctx.Err() == nil {
				a.cfg.Logf("upload after change: %v", err)
				a.retryLater()
			}
		})
		if !ran {
			// The account changed before its turn came: the change is the user's
			// all the same, and the cloud copy is not this account's to touch.
			// What was asked for goes, not a copy of it made since.
			a.mutateQuietly(func() { a.History.RemoveSelected(versionsOf(asked)) })
		}
		// The sync a sign-in asked for while the gate was shut was turned away,
		// and this work is no sync whose end asks again (syncDone): ask now, for
		// the account signed in now, also when this work was the old account's
		// and was cut short by the change.
		if a.loginWanted.Load() {
			a.Auto.Trigger(a.acct(), "login", false)
		}
		if !ran || ctx.Err() != nil {
			return
		}
		// A change of the history made while the gate was shut was turned away
		// too.
		a.Auto.OnHistoryChanged(a.acct())
	}()
}

// notCovered is the items of items that what was written down for asked does
// not stand for: those that are not among asked, and those that a sync has
// since replaced with a later version of the same id (a copy made later on
// another device), which the deletion written down at first does not cover.
func notCovered(items, asked []model.Item) []model.Item {
	was := map[string]model.Item{}
	for _, it := range asked {
		was[strings.ToLower(it.ID)] = it
	}
	var out []model.Item
	for _, it := range items {
		if old, ok := was[strings.ToLower(it.ID)]; !ok || it.CapturedAt.After(old.CapturedAt.Time) {
			out = append(out, it)
		}
	}
	return out
}

// versionsOf picks of a history the items of asked, as they were: not one
// that a copy has made newer since.
func versionsOf(asked []model.Item) func([]model.Item) []model.Item {
	return func(all []model.Item) []model.Item {
		return slices.DeleteFunc(all, func(it model.Item) bool { return len(notCovered([]model.Item{it}, asked)) > 0 })
	}
}

func idsOf(items []model.Item) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

// retryLater asks for a sync in a while, which also makes the deletions that
// are owed. A program that was not started has nobody to ask; its next start
// syncs.
func (a *App) retryLater() {
	a.mu.Lock()
	started := a.started
	a.mu.Unlock()
	if !started {
		return
	}
	time.AfterFunc(30*time.Second, func() {
		if c := a.acct(); c.Err() == nil {
			a.Auto.Trigger(c, "auto", false)
		}
	})
}

// settleDeletions is for the end of a sync that went through, for the account
// owner. What it brought in has left out the items the user deleted; the
// result is uploaded when the server still has one of them (a merge or a
// download left one out, written down by Dropped, also across a restart),
// when one is still here (the program stopped between writing the deletion
// down and making it), or when the server has not had a deletion yet. The
// upload tells which deletions it took (Uploaded). What other devices added
// stays.
func (a *App) settleDeletions(ctx context.Context, owner string) error {
	if len(a.pending.Tombstones(owner)) == 0 {
		return a.pending.Prune(owner)
	}
	// The items are chosen and removed in one step: a copy made in between
	// (which may keep the id of the item it copies again) is newer than the
	// deletion, and stays.
	var here []model.Item
	a.mutateQuietly(func() {
		here = a.History.RemoveSelected(func(items []model.Item) []model.Item {
			if ctx.Err() != nil || a.owner() != owner {
				return nil
			}
			return a.Syncer.Deleted(items)
		})
	})
	if len(here) == 0 && !a.pending.NeedsRepair(owner) && !a.pending.Owed(owner) {
		return a.pending.Prune(owner) // drops what has been enforced long enough
	}
	if _, err := a.Syncer.UploadAfterLocalMutation(ctx); err != nil {
		if ctx.Err() == nil {
			a.retryLater()
		}
		return err
	}
	return nil
}

func (a *App) mutateQuietly(mutate func()) {
	before := a.Syncer.Fingerprint()
	a.Auto.Suppress(true)
	mutate()
	a.Auto.Suppress(false)
	if a.Syncer.Fingerprint() != before {
		a.Auto.OnHistoryChanged(a.acct())
	}
}

// Delete removes an item; with an account the cloud copy follows.
func (a *App) Delete(id string) {
	a.remove(func(all []model.Item) []model.Item {
		for _, it := range all {
			if strings.EqualFold(it.ID, id) {
				return []model.Item{it}
			}
		}
		return nil
	})
}

// ClearAll empties the history; with an account the cloud copy follows. What
// is cleared is what is there when the clearing is made, after a sync that
// was running has ended; an item copied after that stays.
func (a *App) ClearAll() { a.remove(func(all []model.Item) []model.Item { return all }) }

// Reorder moves an item before another one (or to the end).
func (a *App) Reorder(id string, before *string) { a.History.MoveBefore(id, before) }
