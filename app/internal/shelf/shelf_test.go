package shelf

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// The detector against an independent port of the C# class
// (testdata/gen_shake_vectors.py), on a few dozen traces.
func TestDetectorMatchesTheReferenceOfTheCSharpClass(t *testing.T) {
	data, err := os.ReadFile("testdata/shake_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		Trace [][2]float64 `json:"trace"`
		Fires []bool       `json:"fires"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) < 50 {
		t.Fatalf("only %d vectors", len(vectors))
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fired := 0
	for i, v := range vectors {
		d := NewDetector()
		for j, p := range v.Trace {
			got := d.Update(base.Add(time.Duration(p[0])*time.Millisecond), p[1])
			if got != v.Fires[j] {
				t.Fatalf("trace %d, sample %d (t=%v x=%v): Go %v, reference %v", i, j, p[0], p[1], got, v.Fires[j])
			}
			if got {
				fired++
			}
		}
	}
	if fired == 0 {
		t.Fatal("no trace fired: the comparison proves nothing")
	}
}

func TestDetectorBasics(t *testing.T) {
	base := time.Now()
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	d := NewDetector()
	// Right, left, right: two reversals.
	xs := []float64{100, 130, 160, 190, 160, 130, 100, 130, 160}
	var fired []int
	for i, x := range xs {
		if d.Update(at(i*16), x) {
			fired = append(fired, i)
		}
	}
	if len(fired) == 0 {
		t.Fatalf("a shake was not seen")
	}
	// Reset forgets.
	d.Reset()
	if d.Update(at(1000), 100) || d.Update(at(1016), 140) {
		t.Fatal("a fresh detector fired at once")
	}
	// Small steps never count.
	d.Reset()
	for i := 0; i < 100; i++ {
		x := 100.0
		if i%2 == 0 {
			x = 106
		}
		if d.Update(at(2000+i*16), x) {
			t.Fatal("steps of 6 pixels counted")
		}
	}
}

type env struct {
	now      time.Time
	settings Settings
	modDown  bool
	spawned  [][2]int
	g        *Gesture
}

func newEnv() *env {
	e := &env{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), settings: Settings{Enabled: true, Modifier: "Ctrl"}, modDown: true}
	e.g = &Gesture{
		Settings:     func() Settings { return e.settings },
		ModifierDown: func(m string) bool { return e.modDown },
		Spawn:        func(x, y int) { e.spawned = append(e.spawned, [2]int{x, y}) },
		Now:          func() time.Time { return e.now },
	}
	return e
}

func (e *env) at(ms int) { e.now = e.now.Add(time.Duration(ms) * time.Millisecond) }

// shake presses, moves a little, waits out the hold and shakes.
func (e *env) shake(startX int) {
	e.g.LeftDown()
	e.g.Move(startX, 300) // first move: only remembered
	e.at(20)
	e.g.Move(startX+20, 300) // past 8 pixels: a drag
	e.at(120)                // the hold is over
	x := startX + 20
	for i := 0; i < 12; i++ {
		switch (i / 3) % 2 {
		case 0:
			x -= 30
		default:
			x += 30
		}
		e.at(16)
		e.g.Move(x, 300)
	}
}

func TestShakeSpawnsOnceForOnePress(t *testing.T) {
	e := newEnv()
	e.shake(500)
	if len(e.spawned) != 1 {
		t.Fatalf("spawned %d shelves", len(e.spawned))
	}
	// More shaking in the same press does not make another.
	for i := 0; i < 20; i++ {
		e.at(16)
		e.g.Move(500+(i%2)*60, 300)
	}
	if len(e.spawned) != 1 {
		t.Fatalf("a second shelf in the same press: %d", len(e.spawned))
	}
	// A new press, a new shelf.
	e.g.LeftUp()
	e.at(500)
	e.shake(500)
	if len(e.spawned) != 2 {
		t.Fatalf("spawned %d shelves after the second press", len(e.spawned))
	}
}

func TestShakeNeedsEverything(t *testing.T) {
	cases := map[string]func(*env){
		"modifier up":    func(e *env) { e.modDown = false },
		"disabled":       func(e *env) { e.settings.Enabled = false },
		"old Disabled":   func(e *env) { e.settings = FromSettings(true, "Disabled") },
		"no left button": nil,
	}
	for name, setup := range cases {
		e := newEnv()
		if setup != nil {
			setup(e)
		}
		if name == "no left button" {
			x := 500
			for i := 0; i < 30; i++ {
				e.at(16)
				x += 40 * (1 - 2*((i/3)%2))
				e.g.Move(x, 300)
			}
		} else {
			e.shake(500)
		}
		if len(e.spawned) != 0 {
			t.Errorf("%s: a shelf appeared", name)
		}
	}

	// Without moving away from where the button went down.
	e := newEnv()
	e.g.LeftDown()
	e.g.Move(500, 300)
	e.at(200)
	for i := 0; i < 30; i++ {
		e.at(50)
		e.g.Move(500+(i%2)*7, 300) // steps of 7 pixels, never 8 away from where it went down
	}
	if len(e.spawned) != 0 {
		t.Error("a wiggle inside 8 pixels counted as a drag")
	}

	// Too soon after the press: a click and a swipe. The whole shake (two
	// reversals, the second 40 ms after the first) is over by 65 ms.
	e = newEnv()
	e.g.LeftDown()
	e.g.Move(500, 300)
	for _, m := range []struct{ ms, x int }{{2, 520}, {8, 560}, {4, 600}, {6, 560}, {45, 600}} {
		e.at(m.ms)
		e.g.Move(m.x, 300)
	}
	if len(e.spawned) != 0 {
		t.Errorf("a swipe within 110 ms made a shelf")
	}
	// The same shake after the hold does.
	e = newEnv()
	e.g.LeftDown()
	e.g.Move(500, 300)
	e.at(2)
	e.g.Move(520, 300)
	e.at(120)
	for _, m := range []struct{ ms, x int }{{8, 560}, {45, 600}, {45, 560}, {45, 600}, {45, 560}} {
		e.at(m.ms)
		e.g.Move(m.x, 300)
	}
	if len(e.spawned) != 1 {
		t.Errorf("the same shake after 110 ms made %d shelves", len(e.spawned))
	}
}

func TestSettingsAreReadAtEachMove(t *testing.T) {
	e := newEnv()
	e.settings.Enabled = false
	e.g.LeftDown()
	e.g.Move(500, 300)
	e.at(20)
	e.g.Move(520, 300)
	e.at(120)
	e.settings.Enabled = true // switched on in the middle of the press
	x := 520
	for i := 0; i < 12; i++ {
		x += 30 * (1 - 2*((i/3)%2))
		e.at(16)
		e.g.Move(x, 300)
	}
	if len(e.spawned) != 1 {
		t.Fatalf("a setting changed during the press must apply: %d", len(e.spawned))
	}
}

func TestParseModifier(t *testing.T) {
	for in, want := range map[string]string{"": "Ctrl", " ": "Ctrl", "ctrl": "Ctrl", "ALT": "Alt", "shift": "Shift", "Disabled": "Disabled", "disabled": "Disabled", "Win": "Ctrl"} {
		if got := ParseModifier(in); got != want {
			t.Errorf("ParseModifier(%q) = %q, want %q", in, got, want)
		}
	}
	if s := FromSettings(true, "Disabled"); s.Enabled || s.Modifier != "Disabled" {
		t.Errorf("%+v", s)
	}
	if s := FromSettings(true, " alt "); !s.Enabled || s.Modifier != "Alt" {
		t.Errorf("%+v", s)
	}
}

func TestShelfSourceMatchesCSharp(t *testing.T) {
	src, err := os.ReadFile("../../../klip/Services/ShelfService.cs")
	if err != nil {
		t.Skip("C# source not here")
	}
	for _, s := range []string{"8 * 8", "TimeSpan.FromMilliseconds(110)", "ShelfTriggerModifier.Ctrl", "VK_CONTROL = 0x11", "VK_MENU = 0x12", "VK_SHIFT = 0x10", "desired = new PixelPoint(cursor.X + 18, cursor.Y + 18)"} {
		if !strings.Contains(string(src), s) {
			t.Errorf("ShelfService.cs no longer contains %q: the port needs a look", s)
		}
	}
	det, err := os.ReadFile("../../../klip/Services/MouseShakeDetector.cs")
	if err != nil {
		t.Skip()
	}
	for _, s := range []string{"FromMilliseconds(750)", "MinDeltaX { get; set; } = 7", "RequiredDirectionChanges { get; set; } = 2", "FromMilliseconds(40)"} {
		if !strings.Contains(string(det), s) {
			t.Errorf("MouseShakeDetector.cs no longer contains %q", s)
		}
	}
}

func TestServiceDrivesTheGestureFromHookEvents(t *testing.T) {
	var ev Events
	stopped := 0
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	spawns := 0
	s := &Service{
		Settings: func() Settings { return Settings{Enabled: true, Modifier: "Ctrl"} },
		Spawn:    func() { spawns++ },
		Hook:     func(e Events) (func(), error) { ev = e; return func() { stopped++ }, nil },
		Modifier: func(string) bool { return true },
		Now:      func() time.Time { return now },
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("started twice")
	}
	ev.LeftDown(now)
	ev.Move(500, 300, now)
	now = now.Add(20 * time.Millisecond)
	ev.Move(520, 300, now)
	now = now.Add(120 * time.Millisecond)
	x := 520
	for i := 0; i < 12; i++ {
		x += 30 * (1 - 2*((i/3)%2))
		now = now.Add(16 * time.Millisecond)
		ev.Move(x, 300, now)
	}
	if spawns != 1 {
		t.Fatalf("spawns = %d", spawns)
	}
	ev.LeftUp(now)
	s.Stop()
	s.Stop()
	if stopped != 1 {
		t.Fatalf("hook stopped %d times", stopped)
	}
	// Where there is no hook, Start says so.
	bad := &Service{Hook: func(Events) (func(), error) { return nil, ErrUnsupported }}
	if err := bad.Start(); err != ErrUnsupported {
		t.Fatalf("%v", err)
	}
	if err := bad.Start(); err != ErrUnsupported {
		t.Fatalf("a failed Start must be repeatable: %v", err)
	}
}

func TestFollowStartsAndStopsTheHook(t *testing.T) {
	started, stopped := 0, 0
	s := &Service{Settings: func() Settings { return Settings{} },
		Hook: func(Events) (func(), error) { started++; return func() { stopped++ }, nil }}
	for _, c := range []struct {
		st          Settings
		wantStarted int
		wantStopped int
		wantRunning bool
	}{
		{Settings{Enabled: false, Modifier: "Ctrl"}, 0, 0, false},
		{Settings{Enabled: true, Modifier: "Ctrl"}, 1, 0, true},
		{Settings{Enabled: true, Modifier: "Alt"}, 1, 0, true}, // the key is read at each move
		{Settings{Enabled: true, Modifier: "Disabled"}, 1, 1, false},
		{Settings{Enabled: true, Modifier: "Shift"}, 2, 1, true},
		{Settings{Enabled: false, Modifier: "Shift"}, 2, 2, false},
	} {
		if err := s.Follow(c.st); err != nil {
			t.Fatal(err)
		}
		if started != c.wantStarted || stopped != c.wantStopped || s.Running() != c.wantRunning {
			t.Fatalf("%+v: started %d stopped %d running %v", c.st, started, stopped, s.Running())
		}
	}
}
