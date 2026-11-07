package app

import (
	"context"
	"errors"
)

// StopForMigration leaves the app stopped; callers must relaunch on either
// outcome. Acquiring the sync gate prevents a timed-out Stop from racing copying.
func (a *App) StopForMigration(ctx context.Context) error {
	a.Stop()
	done := make(chan struct{})
	go func() { a.done.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	var saveErr error
	if !a.Auto.Exclusive(ctx, func() { saveErr = a.Persister.Flush(ctx) }) {
		return errors.New("后台同步尚未结束，不能迁移")
	}
	return saveErr
}
