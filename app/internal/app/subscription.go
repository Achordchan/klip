package app

import (
	"encoding/json"
	"klip/internal/model"
)

const subscriptionPauseKey = "LocalSyncSubscriptionPausedFor"

func (a *App) subscriptionPaused() bool {
	if a.Settings == nil {
		return false
	}
	var owner string
	st := a.Settings.Get()
	_ = json.Unmarshal(st.Extra[subscriptionPauseKey], &owner)
	return owner != "" && owner == a.owner()
}

// SubscriptionChanged is called only for a confirmed server subscription result.
// Unknown/network failures must not overwrite an established entitlement state.
func (a *App) SubscriptionChanged(active bool) {
	owner := a.owner()
	if owner == "" {
		return
	}
	was := a.subscriptionPaused()
	if was == !active {
		return
	}
	if err := a.Settings.Update(func(st *model.Settings) {
		extra := make(map[string]json.RawMessage, len(st.Extra)+1)
		for k, v := range st.Extra {
			extra[k] = v
		}
		if active {
			delete(extra, subscriptionPauseKey)
		} else {
			extra[subscriptionPauseKey], _ = json.Marshal(owner)
		}
		st.Extra = extra
	}); err != nil {
		a.cfg.Logf("subscription pause persistence: %v", err)
	}
	if !active {
		a.mu.Lock()
		a.status = "云同步已暂停（未订阅或已到期）"
		if a.stopWatch != nil {
			a.stopWatch()
		}
		a.mu.Unlock()
		a.loginWanted.Store(false)
		a.update(func(v *SyncView) {
			v.Result = SyncNone
			v.Syncing = false
			v.Progress = false
			v.Text = ""
			v.LastStatus = "云同步已暂停（未订阅或已到期）"
		})
	} else if was {
		a.mu.Lock()
		ctx, started := a.ctx, a.started
		a.mu.Unlock()
		if started {
			a.watchRemote(ctx)
			a.retryStartup(ctx)
		}
		a.Auto.Trigger(a.acct(), "subscription", false)
	}
}
