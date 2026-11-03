package shelf

import (
	"testing"
	"time"
)

func TestQueuedGestureUsesEventTime(t *testing.T) {
	var events Events
	spawns := 0
	s := &Service{
		Settings: func() Settings { return Settings{Enabled: true, Modifier: "Ctrl"} },
		Modifier: func(string) bool { return true },
		Spawn:    func() { spawns++ },
		Now:      func() time.Time { return time.Unix(0, 0) },
		Hook:     func(e Events) (func(), error) { events = e; return func() {}, nil },
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	var clock mouseClock
	tick := ^uint32(0) - 100 // The queue spans the Windows tick counter rollover.
	events.LeftDown(clock.at(tick))
	events.Move(500, 300, clock.at(tick))
	tick += 20
	events.Move(520, 300, clock.at(tick))
	tick += 120
	x := 520
	for i := 0; i < 12; i++ {
		x += 30 * (1 - 2*((i/3)%2))
		tick += 16
		events.Move(x, 300, clock.at(tick))
	}
	if spawns != 1 {
		t.Fatalf("queued shake spawned %d shelves", spawns)
	}
}
