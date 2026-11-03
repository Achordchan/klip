package history

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"klip/internal/model"
	"klip/internal/paths"
)

// Limits are the rules the history keeps itself to. A nil limit is no limit.
type Limits struct {
	MaxItems                *int
	MaxBytes                *int64
	MergeDuplicates         bool
	ExcludePinnedFromLimits bool
}

// DefaultLimits are those of a new ClipboardHistoryService: 200 items,
// 300 MB, duplicates merged, pinned items outside the limits.
func DefaultLimits() Limits {
	n, b := 200, int64(300)*1024*1024
	return Limits{MaxItems: &n, MaxBytes: &b, MergeDuplicates: true, ExcludePinnedFromLimits: true}
}

// Service is the list of clipboard items, newest first, and the rules that
// keep it in order: a copy of something already in the list moves that item
// to the top, the oldest items go when the list is too long or too big, and
// the files of an item that leaves the list go with it. It is
// ClipboardHistoryService. It is safe for concurrent use.
type Service struct {
	layout paths.Layout

	mu           sync.Mutex
	items        []model.Item
	mutation     string
	deferCleanup bool
	pendingTrash trash
	limits       Limits
	handlers     []func()

	// Now is time.Now unless a test sets it.
	Now func() time.Time
}

// NewService returns an empty history that keeps its files under layout.
func NewService(layout paths.Layout) *Service {
	return &Service{layout: layout, limits: DefaultLimits(), Now: time.Now}
}

// OnChanged registers a function that is called, outside the lock, after
// each change of the list.
func (s *Service) OnChanged(f func()) {
	s.mu.Lock()
	s.handlers = append(s.handlers, f)
	s.mu.Unlock()
}

// Items returns a copy of the list, newest first.
func (s *Service) Items() []model.Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Item(nil), s.items...)
}

// Len is the number of items.
func (s *Service) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// Limits returns the current rules.
func (s *Service) Limits() Limits {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limits
}

// SetLimits changes the rules. The list is not trimmed until the next change.
func (s *Service) SetLimits(l Limits) {
	s.mu.Lock()
	s.limits = l
	s.mu.Unlock()
}

// ApplyLimits sets the rules from the settings (SnapshotSqliteService's
// ApplyHistoryLimits).
func (s *Service) ApplyLimits(st model.Settings) {
	n, b := int(st.MaxItems), int64(st.MaxMegabytes)*1024*1024
	s.SetLimits(Limits{MaxItems: &n, MaxBytes: &b, MergeDuplicates: st.MergeDuplicates, ExcludePinnedFromLimits: st.ExcludePinnedFromLimits})
}

// trash is what a change leaves behind: files that no item refers to.
type trash struct {
	images   []string
	fileDirs []string // item ids, without hyphens
}

func (t *trash) add(it model.Item) {
	if p := model.Str(it.ImageFilePath); strings.TrimSpace(p) != "" {
		t.images = append(t.images, p)
	}
	if it.Kind == model.KindFileList {
		t.fileDirs = append(t.fileDirs, it.IDN())
	}
}

// empty the trash: delete the image files and the cached file directories.
// It is called with the lock held, so that a change cannot bring back an item
// whose files are being deleted.
// A file that cannot be deleted stays; nothing here is worth failing for.
func (s *Service) empty(ctx context.Context, t trash) {
	if s.deferCleanup {
		s.pendingTrash.images = append(s.pendingTrash.images, t.images...)
		s.pendingTrash.fileDirs = append(s.pendingTrash.fileDirs, t.fileDirs...)
		return
	}
	s.emptyNow(ctx, t)
}
func (s *Service) emptyNow(ctx context.Context, t trash) {
	// What an item that stays still refers to is not trash, whatever was
	// removed beside it (two items may share a picture after a sync).
	keepImg := map[string]bool{}
	keepDir := map[string]bool{}
	for _, it := range s.items {
		if p := model.Str(it.ImageFilePath); strings.TrimSpace(p) != "" {
			keepImg[strings.ToLower(p)] = true
		}
		for _, p := range it.FilePaths {
			keepImg[strings.ToLower(p)] = true
		}
		if it.Kind == model.KindFileList {
			keepDir[it.IDN()] = true
		}
	}
	for _, p := range t.images {
		if ctx.Err() != nil {
			return
		}
		if keepImg[strings.ToLower(p)] || !s.managedPath(p) {
			continue
		}
		implicit := false
		for id := range keepDir {
			if withinDir(filepath.Join(s.layout.Files(), id), p) {
				implicit = true
				break
			}
		}
		if !implicit {
			os.Remove(p)
		}
	}
	for _, id := range t.fileDirs {
		if ctx.Err() != nil {
			return
		}
		if keepDir[id] {
			continue
		}
		dir := filepath.Join(s.layout.Files(), id)
		if !s.managedPath(dir) {
			continue
		}
		referenced := false
		for _, it := range s.items {
			if it.ImageFilePath != nil && withinDir(dir, *it.ImageFilePath) {
				referenced = true
			}
			for _, p := range it.FilePaths {
				if withinDir(dir, p) {
					referenced = true
				}
			}
		}
		if !referenced {
			os.RemoveAll(dir)
		}
	}
}

func (s *Service) fire() {
	s.mu.Lock()
	hs := append([]func(){}, s.handlers...)
	s.mu.Unlock()
	for _, h := range hs {
		h()
	}
}

func indexOf(items []model.Item, id string) int {
	for i, it := range items {
		if strings.EqualFold(it.ID, id) {
			return i
		}
	}
	return -1
}

func blank(p *string) bool { return strings.TrimSpace(model.Str(p)) == "" }

// Add puts a new capture at the top. When duplicates are merged and an item
// of the same kind and content hash is already there, that item is updated
// and moved to the top instead: the new time, size and text, one more copy,
// and its pin and note kept. The files of the new capture, which the old item
// does not need, are deleted.
func (s *Service) Add(item model.Item) {
	if item.Kind == model.KindFileList && item.LocalCaptureMutation == "" {
		item.LocalCaptureMutation = captureMutationID()
	}
	var t trash
	s.mu.Lock()
	if s.limits.MergeDuplicates && !blank(item.ContentHash) {
		idx := -1
		for i, x := range s.items {
			if x.Kind == item.Kind && !blank(x.ContentHash) && strings.EqualFold(model.Str(x.ContentHash), model.Str(item.ContentHash)) {
				idx = i
				break
			}
		}
		if idx >= 0 && item.Kind == model.KindFileList {
			item.LocalCaptureMutation = captureMutationID()
		}
		if idx >= 0 && item.Kind == model.KindFileList && !s.replaceCapturedFiles(s.items[idx], item) {
			idx = -1
		}
		if idx >= 0 {
			existing := s.items[idx]
			if existing.Kind == model.KindImage && !blank(item.ImageFilePath) && !blank(existing.ImageFilePath) &&
				!strings.EqualFold(model.Str(item.ImageFilePath), model.Str(existing.ImageFilePath)) && isFile(model.Str(existing.ImageFilePath)) {
				t.images = append(t.images, model.Str(item.ImageFilePath))
			}
			if existing.Kind == model.KindFileList {
				t.fileDirs = append(t.fileDirs, item.IDN())
			}

			first := existing.CapturedAt
			if existing.FirstCapturedAt != nil {
				first = *existing.FirstCapturedAt
			}
			updated := existing
			if item.Kind == model.KindFileList {
				updated.LocalCaptureMutation = item.LocalCaptureMutation
			}
			updated.CapturedAt = item.CapturedAt
			updated.ApproxBytes = item.ApproxBytes
			if item.Text != nil {
				updated.Text = item.Text
			}
			if existing.ImageFilePath == nil || !isFile(model.Str(existing.ImageFilePath)) {
				updated.ImageFilePath = item.ImageFilePath
			}
			if item.FilePaths != nil {
				updated.FilePaths = item.FilePaths
			}
			updated.CopyCount = int32(min(int64(2147483647), int64(max(1, existing.CopyCount))+1))
			updated.FirstCapturedAt = &first
			if existing.ContentHash == nil {
				updated.ContentHash = item.ContentHash
			}

			s.items = append(s.items[:idx], s.items[idx+1:]...)
			s.items = append([]model.Item{updated}, s.items...)
			s.enforceLimits(&t)
			s.empty(context.Background(), t)
			s.mu.Unlock()
			s.fire()
			return
		}
	}
	s.items = append([]model.Item{item}, s.items...)
	s.enforceLimits(&t)
	s.empty(context.Background(), t)
	s.mu.Unlock()
	s.fire()
}

// UpdateNote sets or clears the note of an item. A blank note is none.
func (s *Service) UpdateNote(id string, note string) {
	s.mu.Lock()
	idx := indexOf(s.items, id)
	if idx < 0 {
		s.mu.Unlock()
		return
	}
	if strings.TrimSpace(note) == "" {
		s.items[idx].Note = nil
	} else {
		s.items[idx].Note = &note
	}
	s.mu.Unlock()
	s.fire()
}

// Touch is the use of an item: it moves to the top with the time now and one
// more copy.
func (s *Service) Touch(id string) {
	var t trash
	s.mu.Lock()
	idx := indexOf(s.items, id)
	if idx < 0 {
		s.mu.Unlock()
		return
	}
	it := s.items[idx]
	first := it.CapturedAt
	if it.FirstCapturedAt != nil {
		first = *it.FirstCapturedAt
	}
	it.CapturedAt = model.Time{Time: s.Now()}
	it.CopyCount = int32(min(int64(2147483647), int64(max(1, it.CopyCount))+1))
	it.FirstCapturedAt = &first
	s.items = append(s.items[:idx], s.items[idx+1:]...)
	s.items = append([]model.Item{it}, s.items...)
	s.enforceLimits(&t)
	s.empty(context.Background(), t)
	s.mu.Unlock()
	s.fire()
}

// Remove deletes an item and its files.
func (s *Service) Remove(id string) {
	var t trash
	s.mu.Lock()
	if !s.removeLocked(id, &t) {
		s.mu.Unlock()
		return
	}
	s.empty(context.Background(), t)
	s.mu.Unlock()
	s.fire()
}

func (s *Service) removeLocked(id string, t *trash) bool {
	idx := indexOf(s.items, id)
	if idx < 0 {
		return false
	}
	t.add(s.items[idx])
	s.items = append(s.items[:idx], s.items[idx+1:]...)
	return true
}

// ReplaceAll replaces the whole list (after a sync) and deletes the files of
// the items that are no longer in it: the images whose path is not in the new
// list and the cached file directories of ids that are not. The new list is
// then trimmed to the limits.
func (s *Service) ReplaceAll(ctx context.Context, items []model.Item) error {
	if err := ctx.Err(); err != nil {
		return err // cancelled before anything was changed
	}
	newImages := map[string]bool{}
	newDirs := map[string]bool{}
	for _, it := range items {
		if p := model.Str(it.ImageFilePath); strings.TrimSpace(p) != "" {
			newImages[strings.ToLower(p)] = true
		}
		if it.Kind == model.KindFileList {
			newDirs[it.IDN()] = true
		}
	}

	var t trash
	s.mu.Lock()
	seenImg := map[string]bool{}
	seenDir := map[string]bool{}
	for _, it := range s.items {
		if p := model.Str(it.ImageFilePath); strings.TrimSpace(p) != "" && !newImages[strings.ToLower(p)] && !seenImg[strings.ToLower(p)] {
			seenImg[strings.ToLower(p)] = true
			t.images = append(t.images, p)
		}
		if it.Kind == model.KindFileList && !newDirs[it.IDN()] && !seenDir[it.IDN()] {
			seenDir[it.IDN()] = true
			t.fileDirs = append(t.fileDirs, it.IDN())
		}
	}
	s.items = append([]model.Item(nil), items...)
	s.enforceLimits(&t)
	// Past this point the replacement is done, so it is reported as done and
	// the cleanup is finished even if the caller has given up meanwhile.
	s.empty(context.Background(), t)
	s.mu.Unlock()
	s.fire()
	return nil
}

// ClearAll empties the list and deletes all the files.
func (s *Service) ClearAll() {
	s.mu.Lock()
	if len(s.items) == 0 {
		s.mu.Unlock()
		return
	}
	var t trash
	for _, it := range s.items {
		t.add(it)
	}
	s.items = nil
	s.empty(context.Background(), t)
	s.mu.Unlock()
	s.fire()
}

// TogglePinned pins or unpins an item.
func (s *Service) TogglePinned(id string) {
	s.mu.Lock()
	idx := indexOf(s.items, id)
	if idx < 0 {
		s.mu.Unlock()
		return
	}
	s.items[idx].Pinned = !s.items[idx].Pinned
	if !s.items[idx].Pinned {
		var trash trash
		s.enforceLimits(&trash)
		s.empty(context.Background(), trash)
	}
	s.mu.Unlock()
	s.fire()
}

// MoveBefore moves an item to just before another one, or to the end when
// beforeID is nil or not in the list.
func (s *Service) MoveBefore(movedID string, beforeID *string) {
	if beforeID != nil && *beforeID == movedID {
		return // dropped on itself: it stays where it is
	}
	s.mu.Lock()
	from := indexOf(s.items, movedID)
	if from < 0 {
		s.mu.Unlock()
		return
	}
	it := s.items[from]
	list := append(append([]model.Item(nil), s.items[:from]...), s.items[from+1:]...)
	to := len(list)
	if beforeID != nil {
		if i := indexOf(list, *beforeID); i >= 0 {
			to = i
		}
	}
	list = append(list[:to], append([]model.Item{it}, list[to:]...)...)
	s.items = list
	s.mu.Unlock()
	s.fire()
}

// enforceLimits removes the oldest items that are not pinned while the list
// is too long or too big (EnforceLimits). With ExcludePinnedFromLimits the
// pinned items do not count; without it they count but are still never
// removed, so the list can stay over a limit.
func (s *Service) enforceLimits(t *trash) {
	count := 0
	var size int64
	for _, it := range s.items {
		if s.limits.ExcludePinnedFromLimits && it.Pinned {
			continue
		}
		count++
		size += it.ApproxBytes
	}
	remove := make(map[int]bool)
	for i := len(s.items) - 1; i >= 0; i-- {
		over := (s.limits.MaxItems != nil && count > *s.limits.MaxItems) || (s.limits.MaxBytes != nil && size > *s.limits.MaxBytes)
		if !over {
			break
		}
		if s.items[i].Pinned {
			continue
		}
		remove[i] = true
		count--
		size -= s.items[i].ApproxBytes
		t.add(s.items[i])
	}
	if len(remove) == 0 {
		return
	}
	kept := s.items[:0]
	for i, it := range s.items {
		if !remove[i] {
			kept = append(kept, it)
		}
	}
	s.items = kept
}
