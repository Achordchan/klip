package history

import (
	"context"
	"klip/internal/model"
	"os"
	"path/filepath"
	"strings"
)

// Mutate publishes a sync transaction under the same lock as user edits.
// Payload cleanup is deferred until the sync caller has persisted history.
func (s *Service) Mutate(ctx context.Context, f func([]model.Item) ([]model.Item, error)) error {
	return s.MutateWithMarker(ctx, "", f)
}

// MutateWithMarker commits evidence under the same lock as the history change.
func (s *Service) MutateWithMarker(ctx context.Context, marker string, f func([]model.Item) ([]model.Item, error)) error {
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	next, err := f(append([]model.Item(nil), s.items...))
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.items = append([]model.Item(nil), next...)
	if marker != "" {
		s.mutation = marker
	}
	s.mu.Unlock()
	s.fire()
	return nil
}

// ApplyLimitsAndSnapshot reports only removals made by this limits operation;
// concurrent user deletions must never be mistaken for rollback candidates.
func (s *Service) ApplyLimitsAndSnapshot(st model.Settings) ([]model.Item, []string) {
	s.mu.Lock()
	n, b := int(st.MaxItems), int64(st.MaxMegabytes)*1024*1024
	s.limits = Limits{MaxItems: &n, MaxBytes: &b, MergeDuplicates: st.MergeDuplicates, ExcludePinnedFromLimits: st.ExcludePinnedFromLimits}
	before := append([]model.Item(nil), s.items...)
	var discarded trash
	s.enforceLimits(&discarded)
	kept := make(map[string]bool, len(s.items))
	for _, it := range s.items {
		kept[strings.ToLower(it.ID)] = true
	}
	var removed []string
	for _, it := range before {
		if !kept[strings.ToLower(it.ID)] {
			removed = append(removed, it.ID)
		}
	}
	items := append([]model.Item(nil), s.items...)
	s.mu.Unlock()
	if len(removed) > 0 {
		s.fire()
	}
	return items, removed
}

// RemoveFiles rechecks live references while holding the history lock. Only
// regular cache files beneath the application cache roots may be removed.
func (s *Service) RemoveFiles(files []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := map[string]bool{}
	for _, it := range s.items {
		if it.ImageFilePath != nil {
			keep[strings.ToLower(filepath.Clean(*it.ImageFilePath))] = true
		}
		for _, p := range it.FilePaths {
			keep[strings.ToLower(filepath.Clean(p))] = true
		}
		if it.Kind == model.KindFileList {
			dir := filepath.Join(s.layout.Files(), it.IDN())
			for _, candidate := range files {
				if withinDir(dir, candidate) {
					keep[strings.ToLower(filepath.Clean(candidate))] = true
				}
			}
		}

	}
	var first error
	for _, p := range files {
		p = filepath.Clean(p)
		if keep[strings.ToLower(p)] {
			continue
		}
		if !s.managedPath(p) {
			continue
		}
		st, err := os.Lstat(p)
		if err == nil && st.Mode().IsRegular() {
			if err = os.Remove(p); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// managedPath rejects cache roots themselves and every linked path component.
func (s *Service) managedPath(p string) bool {
	for _, root := range []string{s.layout.Images(), s.layout.Files()} {
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		path := root
		safe := true
		for _, part := range append([]string{""}, strings.Split(rel, string(filepath.Separator))...) {
			path = filepath.Join(path, part)
			st, err := os.Lstat(path)
			if err != nil || st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
				safe = false
				break
			}
		}
		if safe {
			return true
		}
	}
	return false
}
