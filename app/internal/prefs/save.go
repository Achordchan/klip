package prefs

import (
	"fmt"
	"klip/internal/model"
	"strconv"
)

// Save applies only fields edited by this window, preserving concurrent sync
// and account changes. Validation precedes all persistent and OS mutations.
func (c *Controller) Save(base, draft View) error {
	number := func(label, value string) (int, error) {
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 || n > 1<<30 {
			return 0, fmt.Errorf("%s请输入有效正整数", label)
		}
		return n, nil
	}
	items, err := number("最大条目数", draft.MaxItems)
	if err != nil {
		return err
	}
	mb, err := number("最大容量", draft.MaxMegabytes)
	if err != nil {
		return err
	}
	fileMB, err := number("单文件上限", draft.CaptureFileMaxMegabytes)
	if err != nil {
		return err
	}
	if fileMB > mb {
		return fmt.Errorf("单文件上限不能超过最大容量")
	}
	old := c.Settings.Get()
	oldAuto := c.Autostart.IsEnabled()
	if draft.StartWithWindows != base.StartWithWindows {
		if err = c.Autostart.SetEnabled(draft.StartWithWindows); err != nil {
			return err
		}
	}
	err = c.Settings.Update(func(s *model.Settings) {
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
	})
	if err != nil {
		installed := c.Settings.Get()
		_ = c.Settings.Update(func(s *model.Settings) {
			s.RestorePortable(installed, old)
			if draft.StoreImagesAsWebP != base.StoreImagesAsWebP {
				s.SetStoreImagesAsWebP(old.StoreImagesAsWebP())
			}
		})
		if draft.StartWithWindows != base.StartWithWindows {
			_ = c.Autostart.SetEnabled(oldAuto)
		}
		return err
	}
	return nil
}
