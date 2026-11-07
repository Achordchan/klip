package app

import (
	"encoding/json"
	"errors"

	"klip/internal/account"
	"klip/internal/model"
	"klip/internal/syncer"
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

// BeginSubscriptionCheck is taken before a question to the server about the
// subscription of the account signed in now, and given back with the answer
// (SubscriptionChanged).
func (a *App) BeginSubscriptionCheck() account.SubscriptionCheck {
	return a.subscriptionCheck(a.owner())
}

func (a *App) subscriptionCheck(owner string) account.SubscriptionCheck {
	return account.SubscriptionCheck{Owner: owner, Seq: a.subSeq.Add(1)}
}

// SubscriptionChanged is called only for a confirmed server subscription
// result, for the question c. Unknown/network failures must not overwrite an
// established entitlement state.
//
// The answer is for c's account alone: it is checked against the signed-in
// account inside the settings update, under its lock, so an answer about an
// account that was left meanwhile neither pauses nor resumes the next one.
// And it is applied only if no newer question has been answered: a sync's
// "expired" that arrives after the preferences window confirmed a renewal
// is out of date and changes nothing. An answer that changes nothing still
// counts as the newest.
func (a *App) SubscriptionChanged(c account.SubscriptionCheck, active bool) {
	owner := c.Owner
	if owner == "" || a.Settings == nil {
		return
	}
	a.subMu.Lock()
	defer a.subMu.Unlock()
	if c.Seq <= a.subApplied {
		return // a newer answer was applied
	}
	if ownerOf(a.Settings.Get()) != owner {
		return // not this account's
	}
	if pausedFor(a.Settings.Get(), owner) == !active {
		a.subApplied = c.Seq
		return // nothing to change
	}
	changed, was, mine := false, false, false
	if err := a.Settings.Update(func(st *model.Settings) {
		if ownerOf(*st) != owner {
			return // the account changed since the check above
		}
		mine = true
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
	if mine {
		a.subApplied = c.Seq
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

// afterPreflight deals with the answer to the question a sync asks before it
// runs. It returns the error the run ends with, or nil for the run to go on.
// An active subscription is told (it is newer than any answer still on its
// way), and the run then asks under a question of its own: an expiry the
// server reports while it runs is newer than the preflight, which has just
// been applied, and must not be turned away as out of date.
func (a *App) afterPreflight(owner string, check account.SubscriptionCheck, s account.Subscription) error {
	switch s.Status {
	case "未订阅":
		return syncer.ErrSubscriptionExpired
	case "查询失败":
		return errors.New("无法确认订阅状态")
	case "未登录":
		return syncer.ErrNotLoggedIn
	}
	if s.Subscribed {
		a.SubscriptionChanged(check, true)
		run := a.subscriptionCheck(owner)
		a.runSubCheck.Store(&run)
	}
	return nil
}

// endPausedRun ends the display of a run that is over while sync is paused:
// the pause may have come from another question, which found the run's
// "syncing" in place, or before the run began.
func (a *App) endPausedRun() {
	a.mu.Lock()
	a.status = "云同步已暂停（未订阅或已到期）"
	a.mu.Unlock()
	a.update(func(v *SyncView) {
		v.Syncing = false
		v.Progress, v.Text, v.Percent = false, "", 0
		v.LastStatus = "云同步已暂停（未订阅或已到期）"
	})
}
