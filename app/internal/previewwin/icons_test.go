package previewwin

import (
	"github.com/egoist/mygo/ui"
	"klip/internal/model"
	"testing"
	"time"
)

func TestIconLookupDoesNotBlockCaller(t *testing.T) {
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	mg := &Manager{model: &Model{Item: model.Item{SourceAppPath: model.Ptr("slow")}}}
	mg.IconOf = func(string) *ui.Bitmap { close(entered); <-release; return nil }
	mg.onMain = func(f func()) { f(); close(done) }
	mg.mu.Lock()
	mg.loadIcon("slow")
	mg.mu.Unlock()
	defer close(release)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("lookup not started")
	}
	mg.mu.Lock()
	mg.loadIcon("slow")
	mg.mu.Unlock() // no duplicate worker
}
