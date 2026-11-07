package prefs

import (
	"fmt"
	"klip/internal/model"
	"strconv"
)

// Save applies only fields edited by this window, preserving concurrent sync
// and account changes. The edits are merged into the settings and the result
// is validated under the settings lock, so a limit changed meanwhile by a
// sync is checked too. The same check runs first on the settings as they are
// now, so that the usual failure comes before any persistent or OS change.
func (c *Controller) Save(base, draft View) error {
	number := func(label, value string) (int, error) {
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 || n > 1<<30 {
			return 0, fmt.Errorf("%s请输入有效正整数", label)
		}
		return n, nil
	}
	// Only a number this window changed is read: one it did not touch stays
	// as stored, even outside what the window accepts (a very large maximum
	// set elsewhere), as apply leaves it.
	var items, mb, fileMB int
	var err error
	if draft.MaxItems != base.MaxItems {
		if items, err = number("最大条目数", draft.MaxItems); err != nil {
			return err
		}
	}
	if draft.MaxMegabytes != base.MaxMegabytes {
		if mb, err = number("最大容量", draft.MaxMegabytes); err != nil {
			return err
		}
	}
	if draft.CaptureFileMaxMegabytes != base.CaptureFileMaxMegabytes {
		if fileMB, err = number("单文件上限", draft.CaptureFileMaxMegabytes); err != nil {
			return err
		}
	}
	apply := func(s *model.Settings) {
		if draft.StartWithWindows != base.StartWithWindows {
			s.StartWithWindows = draft.StartWithWindows
		}
		if draft.Theme != base.Theme {
			s.Theme = draft.Theme.Value
		}
		if draft.MergeDuplicates != base.MergeDuplicates {
			s.MergeDuplicates = draft.MergeDuplicates
		}
		if draft.ExcludePinnedFromLimits != base.ExcludePinnedFromLimits {
			s.ExcludePinnedFromLimits = draft.ExcludePinnedFromLimits
		}
		if draft.MaxItems != base.MaxItems {
			s.MaxItems = int32(items)
		}
		if draft.MaxMegabytes != base.MaxMegabytes {
			s.MaxMegabytes = int32(mb)
		}
		if draft.CaptureFileMaxMegabytes != base.CaptureFileMaxMegabytes {
			s.CaptureFileMaxMegabytes = int32(fileMB)
		}
		if draft.CaptureText != base.CaptureText {
			s.CaptureText = draft.CaptureText
		}
		if draft.StoreImagesAsWebP != base.StoreImagesAsWebP {
			s.SetStoreImagesAsWebP(draft.StoreImagesAsWebP)
		}
		if draft.CaptureImages != base.CaptureImages {
			s.CaptureImages = draft.CaptureImages
		}
		if draft.CaptureFiles != base.CaptureFiles {
			s.CaptureFiles = draft.CaptureFiles
		}
		if draft.CaptureFileExtensions != base.CaptureFileExtensions {
			s.CaptureFileExtensions = draft.CaptureFileExtensions
		}
		if draft.ShelfEnabled != base.ShelfEnabled {
			s.ShelfEnabled = draft.ShelfEnabled
		}
		if draft.ShelfModifier != base.ShelfModifier {
			s.ShelfTriggerModifier = draft.ShelfModifier.Value
		}
	}
	// The limits are checked against each other only when this save changes
	// one of them: a pair made inconsistent elsewhere does not block saving
	// an unrelated setting.
	limitsEdited := draft.MaxMegabytes != base.MaxMegabytes || draft.CaptureFileMaxMegabytes != base.CaptureFileMaxMegabytes
	validate := func(merged model.Settings) error {
		if limitsEdited && merged.CaptureFileMaxMegabytes > merged.MaxMegabytes {
			return fmt.Errorf("单文件上限不能超过最大容量")
		}
		return nil
	}
	now := c.Settings.Get()
	apply(&now)
	if err := validate(now); err != nil {
		return err
	}

	oldAuto := c.Autostart.IsEnabled()
	if draft.StartWithWindows != base.StartWithWindows {
		if err = c.Autostart.SetEnabled(draft.StartWithWindows); err != nil {
			return err
		}
	}
	// before and after are taken inside the update, under the settings lock:
	// a rollback then puts back only what this save changed, and leaves a
	// change made by somebody else since (a sync, the account) alone.
	var before, after model.Settings
	var invalid error
	err = c.Settings.Update(func(s *model.Settings) {
		before = *s
		merged := *s
		apply(&merged)
		if invalid = validate(merged); invalid != nil {
			after = *s // nothing is changed
			return
		}
		*s = merged
		after = *s
	})
	if invalid != nil {
		if draft.StartWithWindows != base.StartWithWindows {
			_ = c.Autostart.SetEnabled(oldAuto)
		}
		return invalid
	}
	if err != nil {
		_ = c.Settings.Update(func(s *model.Settings) {
			// A field this save did not change has the same value in before
			// and after: RestorePortable leaves it as it is now.
			s.RestorePortable(after, before)
			if draft.StoreImagesAsWebP != base.StoreImagesAsWebP && s.StoreImagesAsWebP() == after.StoreImagesAsWebP() {
				s.SetStoreImagesAsWebP(before.StoreImagesAsWebP())
			}
		})
		if draft.StartWithWindows != base.StartWithWindows {
			_ = c.Autostart.SetEnabled(oldAuto)
		}
		return err
	}
	return nil
}
