package history

import (
	"encoding/json"
	"klip/internal/model"
	"klip/internal/paths"
	"os"
	"testing"
)

func TestEmptyHistoryKeepsAtomicMutationEvidence(t *testing.T) {
	l := paths.Layout{Root: t.TempDir()}
	marker := "0123456789abcdef0123456789abcdef"
	if err := SaveWithMutation(l, nil, marker); err != nil {
		t.Fatal(err)
	}
	if SavedMutation(l) != marker {
		t.Fatal("empty history lost transaction evidence")
	}
	data, _ := os.ReadFile(l.History())
	var legacy []model.Item
	if err := json.Unmarshal(data, &legacy); err != nil || len(legacy) != 0 {
		t.Fatalf("legacy array incompatible: %v", err)
	}
	if err := Save(l, nil); err != nil {
		t.Fatal(err)
	}
	if SavedMutation(l) != "" {
		t.Fatal("legacy write retained stale evidence")
	}
}
