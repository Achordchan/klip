package syncer

import (
	"klip/internal/model"
	"strings"
)

// recountMergedSides uses the transitive ID/hash component, not just direct
// matches with the representative retained by MergeItems.
func recountMergedSides(merged, remote, local []model.Item) {
	all := append(append([]model.Item(nil), remote...), local...)
	parent := make([]int, len(all))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	ids, keys := map[string]int{}, map[string]int{}
	for i, it := range all {
		for _, entry := range []struct {
			key   string
			table map[string]int
		}{{strings.ToLower(it.ID), ids}, {strings.ToLower(mergeKey(it)), keys}} {
			if entry.key == "" {
				continue
			}
			if j, ok := entry.table[entry.key]; ok {
				parent[find(i)] = find(j)
			} else {
				entry.table[entry.key] = i
			}
		}
	}
	totals := map[int][2]int64{}
	for i, it := range all {
		root := find(i)
		v := totals[root]
		side := 0
		if i >= len(remote) {
			side = 1
		}
		v[side] = min(int64(2147483647), v[side]+int64(max(1, it.CopyCount)))
		totals[root] = v
	}
	for i, it := range merged {
		if j, ok := ids[strings.ToLower(it.ID)]; ok {
			v := totals[find(j)]
			merged[i].CopyCount = int32(max(v[0], v[1]))
		}
	}
}

func findConnectedIdentity(candidates []model.Item, target model.Item, sides ...[]model.Item) (model.Item, bool) {
	if it, ok := findIdentity(candidates, target); ok {
		return it, true
	}
	group := []model.Item{target}
	all := []model.Item{}
	for _, side := range sides {
		all = append(all, side...)
	}
	seen := make([]bool, len(all))
	for changed := true; changed; {
		changed = false
		for i, it := range all {
			if seen[i] {
				continue
			}
			for _, member := range group {
				if sameMergeIdentity(member, it) {
					seen[i] = true
					group = append(group, it)
					changed = true
					break
				}
			}
		}
	}
	for _, it := range candidates {
		for _, member := range group {
			if sameMergeIdentity(member, it) {
				return it, true
			}
		}
	}
	return model.Item{}, false
}

func identityComponents(sides ...[]model.Item) map[string]int {
	all := []model.Item{}
	for _, items := range sides {
		all = append(all, items...)
	}
	parent := make([]int, len(all))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	ids, keys := map[string]int{}, map[string]int{}
	for i, it := range all {
		for _, entry := range []struct {
			k string
			m map[string]int
		}{{strings.ToLower(it.ID), ids}, {strings.ToLower(mergeKey(it)), keys}} {
			if entry.k == "" {
				continue
			}
			if j, ok := entry.m[entry.k]; ok {
				parent[find(i)] = find(j)
			} else {
				entry.m[entry.k] = i
			}
		}
	}
	result := map[string]int{}
	for id, i := range ids {
		result[id] = find(i)
	}
	return result
}
