package app

import (
	"encoding/json"

	"klip/internal/account"
	"klip/internal/model"
)

const subscriptionPauseKey = "LocalSyncSubscriptionPausedFor"

func (a *App) subscriptionPaused() bool {
	if a.Settings == nil {
		return false
	}
	st := a.Settings.Get()
	return pausedFor(st, ownerOf(st))
}

// pausedFor says whether sync is paused, in st, for the account owner.
func pausedFor(st model.Settings, owner string) bool {
	var paused string
	_ = json.Unmarshal(st.Extra[subscriptionPauseKey], &paused)
	return paused != "" && paused == owner
}

// Owner is the account signed in now ("" for none): a caller that asks the
// server about the subscription takes it first and gives it back with the
// answer (SubscriptionChanged).
func (a *App) Owner() string { return a.owner() }

// SubscriptionChanged is called only for a confirmed server subscription
// result, for the account owner the question was asked for. Unknown/network
// failures must not overwrite an established entitlement state.
//
// The answer is for that account alone: it is checked against the signed-in
// account inside the settings update, under its lock, so an answer about an
// account that was left meanwhile neither pauses nor resumes the next one.
func (a *App) SubscriptionChanged(owner string, active bool) {
	if owner == "" || a.Settings == nil {
		return
	}
	if st := a.Settings.Get(); ownerOf(st) != owner || pausedFor(st, owner) == !active {
		return // not this account's, or nothing to change
	}
	changed, was := false, false
	if err := a.Settings.Update(func(st *model.Settings) {
		if ownerOf(*st) != owner {
			return // the account changed since the check above
		}
		was = pausedFor(*st, owner)
		if was == !active {
			return
		}
		changed = true
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
	if !changed || a.owner() != owner {
		return // the rest is for the account the answer was about, still signed in
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

type unconfirmedAction int

const (
	skipSync  unconfirmedAction = iota // signed out: nothing is owed to this account
	pauseSync                          // the server says there is no subscription
	retrySync                          // the answer is unknown: try again later
)

// uploadAfterUnconfirmed decides what an upload after a local change does when
// the subscription was not confirmed. Only a confirmed "未订阅" pauses sync; an
// unknown answer must not drop the owed upload.
func uploadAfterUnconfirmed(s account.Subscription) unconfirmedAction {
	switch s.Status {
	case "未订阅":
		return pauseSync
	case "未登录":
		return skipSync
	}
	return retrySync
}
