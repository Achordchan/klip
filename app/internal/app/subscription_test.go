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
	a.SubscriptionChanged(false)
	// Prevent a background request in this unit test while checking the state.
	a.Auto.CanSync = func() bool { return false }
	a.SubscriptionChanged(true)
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
