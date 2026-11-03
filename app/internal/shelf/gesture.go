package shelf

import (
	"strings"
	"time"
)

// Settings say whether the shelf is on and which key triggers it.
type Settings struct {
	Enabled  bool
	Modifier string // Ctrl, Alt, Shift or Disabled
}

// Gesture is the part of ShelfService that turns mouse events into "show a
// shelf here": the left button must be held, the trigger key down, the mouse
// moved at least 8 pixels from where it went down, the button held for 110 ms,
// and then a shake. One shelf for each press.
type Gesture struct {
	// Settings is asked at each move, so that a change applies at once.
	Settings func() Settings
	// ModifierDown says whether the key ("Ctrl", "Alt", "Shift") is down.
	ModifierDown func(modifier string) bool
	// Spawn shows a shelf for a shake that ended at this screen point.
	Spawn func(x, y int)
	// Now is time.Now unless a test sets it.
	Now func() time.Time

	det *Detector

	leftDown     bool
	spawned      bool
	downAt       time.Time
	downPoint    *[2]int
	thresholdMet bool
}

func (g *Gesture) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Gesture) detector() *Detector {
	if g.det == nil {
		g.det = NewDetector()
	}
	return g.det
}

// LeftDown is the left button going down anywhere.
func (g *Gesture) LeftDown() { g.leftDownAt(g.now()) }

func (g *Gesture) leftDownAt(at time.Time) {
	g.leftDown, g.spawned = true, false
	g.downAt = at
	g.downPoint, g.thresholdMet = nil, false
	g.detector().Reset()
}

// LeftUp is the left button coming up.
func (g *Gesture) LeftUp() {
	g.leftDown, g.spawned = false, false
	g.downPoint, g.thresholdMet = nil, false
	g.detector().Reset()
}

// Move is the mouse at screen point (x, y).
func (g *Gesture) Move(x, y int) { g.moveAt(x, y, g.now()) }

func (g *Gesture) moveAt(x, y int, at time.Time) {
	if !g.leftDown || g.spawned {
		return
	}
	st := g.Settings()
	mod := ParseModifier(st.Modifier)
	if !st.Enabled || mod == "Disabled" {
		return
	}
	if g.ModifierDown != nil && !g.ModifierDown(mod) {
		return
	}
	// Not before the user has dragged a little.
	if g.downPoint == nil {
		g.downPoint = &[2]int{x, y}
		return
	}
	if !g.thresholdMet {
		dx, dy := x-g.downPoint[0], y-g.downPoint[1]
		if dx*dx+dy*dy < 8*8 {
			return
		}
		g.thresholdMet = true
		g.detector().Reset() // a real drag: a fresh shake window
	}
	// Not a click and a swipe: a short hold first.
	if at.Sub(g.downAt) < 110*time.Millisecond {
		return
	}
	if g.detector().Update(at, float64(x)) {
		g.spawned = true
		if g.Spawn != nil {
			g.Spawn(x, y)
		}
	}
}

// ParseModifier is ParseModifier of ShelfService: Ctrl, Alt, Shift or
// Disabled, any case; anything else, and blank, is Ctrl.
func ParseModifier(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "alt":
		return "Alt"
	case "shift":
		return "Shift"
	case "disabled":
		return "Disabled"
	}
	return "Ctrl"
}

// FromSettings builds Settings from the stored values: a modifier "Disabled"
// from an old configuration switches the shelf off.
func FromSettings(enabled bool, modifier string) Settings {
	m := ParseModifier(modifier)
	if m == "Disabled" {
		enabled = false
	}
	return Settings{Enabled: enabled, Modifier: m}
}
