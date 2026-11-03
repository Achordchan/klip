package platform

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatcherMoved(t *testing.T) {
	w := &Watcher{}
	steps := []struct {
		seq  int64
		want bool
		why  string
	}{
		{5, false, "first tick only records"},
		{5, false, "unchanged"},
		{6, true, "moved"},
		{6, false, "unchanged again"},
		{9, true, "moved by more than one is still one change"},
	}
	for _, s := range steps {
		if got := w.moved(s.seq); got != s.want {
			t.Fatalf("moved(%d) = %v, want %v (%s)", s.seq, got, s.want, s.why)
		}
	}
}

func TestWatcherWithoutCounter(t *testing.T) {
	w := &Watcher{}
	if w.moved(-1) {
		t.Fatal("the first tick must not look, as with a counter")
	}
	if !w.moved(-1) || !w.moved(-1) {
		t.Fatal("without a counter every later tick must look")
	}
}

func TestWatcherRun(t *testing.T) {
	var seq atomic.Int64
	seq.Store(10)
	var mu sync.Mutex
	calls := 0
	w := &Watcher{Interval: 5 * time.Millisecond, Seq: seq.Load, OnChange: func() { mu.Lock(); calls++; mu.Unlock() }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	time.Sleep(60 * time.Millisecond)
	mu.Lock()
	if calls != 0 {
		t.Fatalf("calls before any change = %d", calls)
	}
	mu.Unlock()

	seq.Store(11)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		c := calls
		mu.Unlock()
		if c == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("calls = %d, want 1", c)
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond) // many more ticks, same number
	mu.Lock()
	if calls != 1 {
		t.Fatalf("one change was reported %d times", calls)
	}
	mu.Unlock()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestWatcherContentFallback(t *testing.T) {
	w := &Watcher{}
	steps := []struct {
		fp   string
		want bool
	}{{"a", false}, {"a", false}, {"b", true}, {"b", false}, {"a", true}}
	for _, s := range steps {
		if got := w.contentMoved(s.fp); got != s.want {
			t.Fatalf("contentMoved(%q) = %v, want %v", s.fp, got, s.want)
		}
	}
}
