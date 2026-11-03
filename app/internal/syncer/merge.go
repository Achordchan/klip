package syncer

import (
	"math"
	"sort"
	"strings"

	"klip/internal/model"
)

const emptyGUID = "00000000-0000-0000-0000-000000000000"

// MergeItems is SyncService.MergeItems: the items of both sides, with the
// ones that are the same item (same content hash and kind, else same id)
// made one, newest first.
func MergeItems(remote, local []model.Item) []model.Item {
	return mergeItems(remote, local, mergeKey)
}

// MergeItemsKeepingDuplicates is MergeItems for a history whose duplicates
// are not merged (MergeDuplicates off), which the C# client does not tell
// apart: an item on both sides (the same id) is made one, but entries of the
// same content stay what they are, each with its own note and pin, as they
// do here between syncs.
func MergeItemsKeepingDuplicates(remote, local []model.Item) []model.Item {
	return mergeItems(remote, local, idKey)
}

func mergeItems(remote, local []model.Item, mergeKey func(model.Item) string) []model.Item {
	merged := map[string]model.Item{}
	var order []string // keys in insertion order: the dictionary's
	var passthrough []model.Item

	// byID reconciles items that are the same item by id but get different
	// keys because only one side has a content hash (an older client wrote
	// it). The snapshot export keeps one row per id, so the merged history
	// must too.
	byID := map[string]string{}
	aliases := map[string]string{}
	// noteLocal says whether the note a group carries came from a local item:
	// group membership alone does not say it (a local item can join a group
	// whose note is remote).
	noteLocal := map[string]bool{}
	resolve := func(key string) string {
		for i := 0; i < 8; i++ {
			next, ok := aliases[key]
			if !ok || next == key {
				break
			}
			key = next
		}
		return key
	}
	// fold joins the group other into into: they are the same item. The
	// folded group keeps its own note provenance.
	fold := func(into, other string) {
		into, other = resolve(into), resolve(other)
		if other == "" || other == into {
			return
		}
		value, ok := merged[other]
		if !ok {
			return
		}
		oldNote, oldLocal := merged[into].Note, noteLocal[into]
		merged[into] = mergeDuplicate(merged[into], value, noteLocal[other])
		_, noteLocal[into] = mergeNoteProvenance(oldNote, oldLocal, value.Note, noteLocal[other])
		delete(merged, other)
		delete(noteLocal, other)
		for i, k := range order {
			if k == other {
				order = append(order[:i], order[i+1:]...)
				break
			}
		}
		for alias, k := range byID {
			if k == other {
				byID[alias] = into
			}
		}
		for alias, k := range aliases {
			if k == other {
				aliases[alias] = into
			}
		}
		aliases[other] = into
	}
	add := func(it model.Item, isLocal bool) {
		key := mergeKey(it)
		if strings.TrimSpace(key) == "" {
			passthrough = append(passthrough, it)
			return
		}
		key = resolve(strings.ToLower(key)) // the C# dictionary ignores case
		id := strings.ToLower(strings.TrimSpace(it.ID))
		if id != "" && id != emptyGUID {
			if existingKey, ok := byID[id]; ok {
				existingKey = resolve(existingKey)
				if existingKey != key && merged[existingKey].Kind == it.Kind {
					oldNote, oldLocal := merged[existingKey].Note, noteLocal[existingKey]
					merged[existingKey] = mergeDuplicate(merged[existingKey], it, isLocal)
					_, noteLocal[existingKey] = mergeNoteProvenance(oldNote, oldLocal, it.Note, isLocal)
					fold(existingKey, key)
					if key != existingKey {
						aliases[key] = existingKey
					}
					byID[id] = existingKey
					return
				}
			}
		}
		if existing, ok := merged[key]; ok {
			oldNote, oldLocal := existing.Note, noteLocal[key]
			merged[key] = mergeDuplicate(existing, it, isLocal)
			_, noteLocal[key] = mergeNoteProvenance(oldNote, oldLocal, it.Note, isLocal)
			if mergedID := strings.ToLower(strings.TrimSpace(merged[key].ID)); mergedID != "" && mergedID != emptyGUID {
				if other, ok := byID[mergedID]; ok {
					fold(key, other)
				}
				byID[mergedID] = key
			}
			if id != "" && id != emptyGUID {
				if other, ok := byID[id]; ok {
					fold(key, other)
				}
				byID[id] = key
			}
			return
		}
		merged[key] = it
		noteLocal[key] = isLocal && strings.TrimSpace(model.Str(it.Note)) != ""
		order = append(order, key)
		if id != "" && id != emptyGUID {
			byID[id] = key
		}
	}
	for _, it := range remote {
		add(it, false)
	}
	for _, it := range local {
		add(it, true)
	}

	out := make([]model.Item, 0, len(merged)+len(passthrough))
	for _, k := range order {
		out = append(out, merged[k])
	}
	out = append(out, passthrough...)
	// Newest first, then by id: the order is stable and the GUID order is that
	// of its text (a, b, c as fixed-width hex, then the bytes).
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.CapturedAt.Equal(b.CapturedAt.Time) {
			return a.CapturedAt.After(b.CapturedAt.Time)
		}
		return strings.ToLower(a.ID) < strings.ToLower(b.ID)
	})
	return out
}

// MergeKey is what MergeItems takes two items to be the same item by, as it
// compares it ("" for an item it never merges).
func MergeKey(it model.Item) string { return strings.ToLower(mergeKey(it)) }

func mergeKey(it model.Item) string {
	if h := strings.TrimSpace(model.Str(it.ContentHash)); h != "" {
		return "hash:" + itoa(int(it.Kind)) + ":" + h
	}
	return idKey(it)
}

func idKey(it model.Item) string {
	if strings.ToLower(it.ID) == emptyGUID || it.ID == "" {
		return ""
	}
	return "id:" + strings.ToLower(it.ID)
}

func itoa(n int) string { return string(rune('0' + n)) } // kinds are 0..2

// addCounts adds two copy counts (a negative one counts as none), and stays
// at the largest count an item holds instead of wrapping around.
func addCounts(a, b int32) int32 {
	return int32(min(int64(max(0, a))+int64(max(0, b)), math.MaxInt32))
}

func mergeDuplicate(existing, incoming model.Item, incomingIsLocal bool) model.Item {
	latest := existing
	if !incoming.CapturedAt.Before(existing.CapturedAt.Time) {
		latest = incoming
	}
	firstOf := func(it model.Item) model.Time {
		if it.FirstCapturedAt != nil {
			return *it.FirstCapturedAt
		}
		return it.CapturedAt
	}
	first := firstOf(existing)
	if f := firstOf(incoming); f.Before(first.Time) {
		first = f
	}

	hash := latest.ContentHash
	if strings.TrimSpace(model.Str(hash)) == "" {
		if strings.TrimSpace(model.Str(existing.ContentHash)) == "" {
			hash = incoming.ContentHash
		} else {
			hash = existing.ContentHash
		}
	}

	out := latest
	out.ApproxBytes = max(existing.ApproxBytes, incoming.ApproxBytes)
	out.Pinned = existing.Pinned || incoming.Pinned
	out.CopyCount = addCounts(existing.CopyCount, incoming.CopyCount)
	out.FirstCapturedAt = &first
	out.Note = mergedNote(existing.Note, incoming.Note, incomingIsLocal)
	out.ContentHash = hash
	return out
}

// mergedNote keeps the local note when there is one, else the one that was
// there first.
func mergedNote(existing, incoming *string, incomingIsLocal bool) *string {
	blank := func(p *string) bool { return strings.TrimSpace(model.Str(p)) == "" }
	switch {
	case incomingIsLocal && !blank(incoming):
		return incoming
	case !blank(existing):
		return existing
	case blank(incoming):
		return nil
	}
	return incoming
}

// mergeNoteProvenance is mergedNote and says whether the resulting note came
// from a local item.
func mergeNoteProvenance(existing *string, existingLocal bool, incoming *string, incomingLocal bool) (*string, bool) {
	out := mergedNote(existing, incoming, incomingLocal)
	switch {
	case out == nil:
		return nil, false
	case out == incoming:
		return out, incomingLocal
	default:
		return out, existingLocal
	}
}

// EnsureSettingsCanHoldItems raises the history limits of s to what the
// merged items need, so that the merge does not lose items to the limits.
func EnsureSettingsCanHoldItems(s *model.Settings, items []model.Item) {
	if int(s.MaxItems) < len(items) {
		s.MaxItems = int32(len(items))
	}
	var total int64
	for _, it := range items {
		if it.ApproxBytes <= 0 {
			continue
		}
		if int64(^uint64(0)>>1)-total < it.ApproxBytes {
			total = int64(^uint64(0) >> 1)
			break
		}
		total += it.ApproxBytes
	}
	needed := int64(1)
	if total > 0 {
		const mb = int64(1024 * 1024)
		// Quotient and remainder: total+mb-1 would overflow at MaxInt64.
		needed = total / mb
		if total%mb != 0 {
			needed++
		}
		needed = max(1, needed)
		needed = min(needed, int64(^uint32(0)>>1))
	}
	if int64(s.MaxMegabytes) < needed {
		s.MaxMegabytes = int32(needed)
	}
}
