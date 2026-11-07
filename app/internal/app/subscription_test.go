package app

import (
	"klip/internal/account"
	"klip/internal/model"
	"klip/internal/syncer"
	"testing"
)

func TestExpiredSubscriptionPausesWithoutFailureAndSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	a := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1")
	_ = a.Settings.Update(func(s *model.Settings) {
		s.AuthRefreshToken = model.Ptr("token")
		s.AuthUserEmail = model.Ptr("expired@example.com")
	})
	owner := "expired@example.com"
	check := a.subscriptionCheck(owner)
	a.runSubCheck.Store(&check) // the question of the run that found the subscription expired
	a.syncDone("startup", false, syncer.ErrSubscriptionExpired)
	if !a.subscriptionPaused() || a.Auto.CanSync() {
		t.Fatal("expired subscription still schedules sync")
	}
	v := a.Sync()
	if v.Icon() != "idle" || v.Tip() != "云同步已暂停（未订阅或已到期）" {
		t.Fatalf("expiration shown as failure: %+v", v)
	}
	b := newApp(t, root, &fakeClip{}, "http://127.0.0.1:1")
	if !b.subscriptionPaused() || b.Auto.CanSync() {
		t.Fatal("restart forgot pause")
	}
	_ = b.Settings.Update(func(s *model.Settings) { s.AuthUserEmail = model.Ptr("other@example.com") })
	if b.subscriptionPaused() {
		t.Fatal("pause leaked to another account")
	}
}

func TestConfirmedRenewalReenablesSync(t *testing.T) {
	a := newApp(t, t.TempDir(), &fakeClip{}, "http://127.0.0.1:1")
	_ = a.Settings.Update(func(s *model.Settings) {
		s.AuthRefreshToken = model.Ptr("token")
		s.AuthUserEmail = model.Ptr("renewed@example.com")
	})
	a.SubscriptionChanged(a.BeginSubscriptionCheck(), false)
	// Prevent a background request in this unit test while checking the state.
	a.Auto.CanSync = func() bool { return false }
	a.SubscriptionChanged(a.BeginSubscriptionCheck(), true)
	if a.subscriptionPaused() {
		t.Fatal("confirmed renewal did not clear pause")
	}
}

func TestUnconfirmedSubscriptionRetriesOwedUpload(t *testing.T) {
	for _, tc := range []struct {
		sub  account.Subscription
		want unconfirmedAction
	}{
		{account.None("未订阅"), pauseSync},
		{account.None("未登录"), skipSync},
		{account.None("查询失败"), retrySync},
		{account.Loading(), retrySync},
	} {
		if got := uploadAfterUnconfirmed(tc.sub); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.sub.Status, got, tc.want)
		}
	}
}

func TestSubscriptionAnswerOfALeftAccountChangesNothing(t *testing.T) {
	a := newApp(t, t.TempDir(), &fakeClip{}, "http://127.0.0.1:1")
	a.Auto.CanSync = func() bool { return false }
	_ = a.Settings.Update(func(s *model.Settings) {
		s.AuthRefreshToken = model.Ptr("token")
		s.AuthUserEmail = model.Ptr("b@example.com")
	})
	// An answer about account A comes after B signed in.
	a.SubscriptionChanged(a.subscriptionCheck("a@example.com"), false)
	if a.subscriptionPaused() {
		t.Fatal("A's expired subscription paused B")
	}
	a.SubscriptionChanged(a.subscriptionCheck("b@example.com"), false)
	if !a.subscriptionPaused() {
		t.Fatal("B's own answer did not pause B")
	}
	a.SubscriptionChanged(a.subscriptionCheck("a@example.com"), true)
	if !a.subscriptionPaused() {
		t.Fatal("A's renewal resumed B")
	}
}

func TestOlderSubscriptionAnswerDoesNotUndoANewerOne(t *testing.T) {
	a := newApp(t, t.TempDir(), &fakeClip{}, "http://127.0.0.1:1")
	a.Auto.CanSync = func() bool { return false }
	_ = a.Settings.Update(func(s *model.Settings) {
		s.AuthRefreshToken = model.Ptr("token")
		s.AuthUserEmail = model.Ptr("c@example.com")
	})
	// A sync asks first; the preferences window asks after it.
	syncCheck := a.BeginSubscriptionCheck()
	prefsCheck := a.BeginSubscriptionCheck()
	// The window's "renewed" comes first (nothing to change: not paused),
	// then the sync's older "expired".
	a.SubscriptionChanged(prefsCheck, true)
	a.SubscriptionChanged(syncCheck, false)
	if a.subscriptionPaused() {
		t.Fatal("an older expired answer paused sync after a newer renewal")
	}
	// In order, the newer answer wins too.
	first, second := a.BeginSubscriptionCheck(), a.BeginSubscriptionCheck()
	a.SubscriptionChanged(first, false)
	if !a.subscriptionPaused() {
		t.Fatal("expired did not pause")
	}
	a.SubscriptionChanged(second, true)
	if a.subscriptionPaused() {
		t.Fatal("the newer renewal did not resume")
	}
}

func signedIn(t *testing.T, email string) *App {
	t.Helper()
	a := newApp(t, t.TempDir(), &fakeClip{}, "http://127.0.0.1:1")
	a.Auto.CanSync = func() bool { return false }
	_ = a.Settings.Update(func(s *model.Settings) {
		s.AuthRefreshToken = model.Ptr("token")
		s.AuthUserEmail = model.Ptr(email)
	})
	a.runGen.Store(a.acctGen.Load()) // the run is for the account signed in
	return a
}

func TestExpiryDuringTheRunPausesAfterAnActivePreflight(t *testing.T) {
	a := signedIn(t, "d@example.com")
	check := a.subscriptionCheck("d@example.com")
	a.runSubCheck.Store(&check)
	if err := a.afterPreflight("d@example.com", check, account.Subscription{Subscribed: true, Status: "已订阅"}); err != nil {
		t.Fatal(err)
	}
	// The server says the subscription expired while the sync ran.
	a.syncDone("auto", false, syncer.ErrSubscriptionExpired)
	if !a.subscriptionPaused() {
		t.Fatal("an expiry reported by the run was turned away as older than its own preflight")
	}
}

func TestStaleExpiryStillEndsTheRun(t *testing.T) {
	a := signedIn(t, "e@example.com")
	older := a.subscriptionCheck("e@example.com")
	a.runSubCheck.Store(&older)
	a.syncBegin("auto", false)
	// The preferences window confirmed the subscription meanwhile.
	a.SubscriptionChanged(a.BeginSubscriptionCheck(), true)
	a.syncDone("auto", false, syncer.ErrSubscriptionExpired)
	if a.subscriptionPaused() {
		t.Fatal("the out-of-date expiry paused sync")
	}
	if v := a.Sync(); v.Syncing || v.Progress {
		t.Fatalf("the run is still shown as running: %+v", v)
	}
}

func TestRunEndingWhilePausedEndsItsDisplay(t *testing.T) {
	a := signedIn(t, "f@example.com")
	a.syncBegin("manual", true)
	a.SubscriptionChanged(a.BeginSubscriptionCheck(), false) // paused by another question
	a.syncBegin("manual", true)                              // a run already under way shows
	a.syncDone("manual", true, nil)
	if v := a.Sync(); v.Syncing || v.Progress {
		t.Fatalf("the run is still shown as running: %+v", v)
	}
}
