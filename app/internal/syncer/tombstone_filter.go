package syncer

import (
	"klip/internal/model"
	"strings"
)

func (s *Syncer) account() string {
	if s.cfg.Account != nil {
		return s.cfg.Account()
	}
	return ""
}

// holdsState says that the account the work is for has a repair that is not
// written down: its syncs are not written down as done (PersistSyncState).
// Another account's work is not held by it.
func (s *Syncer) holdsState() bool {
	p := s.unrepaired.Load()
	return p != nil && *p == s.account()
}

// repaired is for the account the work is for: its repair is written down,
// or its server has the history without the items.
func (s *Syncer) repaired() {
	if p := s.unrepaired.Load(); p != nil && *p == s.account() {
		s.unrepaired.CompareAndSwap(p, nil)
	}
}

// withoutDeleted leaves out of items the ones the user deleted (Config.Deleted).
func (s *Syncer) withoutDeleted(items []model.Item) []model.Item {
	gone := s.Deleted(items)
	if len(gone) == 0 {
		return items
	}
	if s.cfg.Dropped != nil {
		if err := s.cfg.Dropped(); err != nil {
			acct := s.account()
			s.unrepaired.Store(&acct)
		} else {
			s.repaired()
		}
	}
	skip := map[string]bool{}
	for _, it := range gone {
		skip[strings.ToLower(it.ID)] = true
	}
	out := make([]model.Item, 0, len(items)-len(gone))
	for _, it := range items {
		if !skip[strings.ToLower(it.ID)] {
			out = append(out, it)
		}
	}
	return out
}

// Deleted says which of items the user deleted (Config.Deleted; see
// Tombstone for which items a deletion stands for).
func (s *Syncer) Deleted(items []model.Item) []model.Item {
	if s.cfg.Deleted == nil {
		return nil
	}
	stones := s.cfg.Deleted()
	if len(stones) == 0 {
		return nil
	}
	byID, byKey := map[string][]Tombstone{}, map[string][]Tombstone{}
	for _, t := range stones {
		byID[strings.ToLower(t.ID)] = append(byID[strings.ToLower(t.ID)], t)
		if k := strings.ToLower(t.Key); k != "" {
			byKey[k] = append(byKey[k], t)
		}
	}
	covered := func(it model.Item, ts []Tombstone) bool {
		for _, t := range ts {
			if t.Covers(it) {
				return true
			}
		}
		return false
	}
	var out []model.Item
	for _, it := range items {
		if covered(it, byID[strings.ToLower(it.ID)]) || covered(it, byKey[MergeKey(it)]) {
			out = append(out, it)
		}
	}
	return out
}
