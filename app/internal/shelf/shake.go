// Package shelf is the shelf of 1.0.10 on Windows: while a file is being
// dragged, a quick shake of the mouse to the left and right (with the trigger
// key held) brings up a small window that the file can be dropped on, and
// that can be dragged from again. This package has the gesture, which is
// plain logic and runs in tests; the mouse hook is in hook_windows.go and the
// window in package shelfwin.
package shelf

import "time"

// Detector is MouseShakeDetector: it counts changes of direction of the
// mouse along the X axis.
type Detector struct {
	// Window is how far back samples are kept (750 ms).
	Window time.Duration
	// MinDeltaX is the smallest step that counts as a move (7 pixels).
	MinDeltaX float64
	// RequiredDirectionChanges is how many reversals make a shake (2).
	RequiredDirectionChanges int

	samples  []sample
	changes  int
	lastDir  int
	lastTurn time.Time
}

type sample struct {
	t time.Time
	x float64
}

// NewDetector returns a Detector with the defaults of the C# class.
func NewDetector() *Detector {
	return &Detector{Window: 750 * time.Millisecond, MinDeltaX: 7, RequiredDirectionChanges: 2}
}

// Reset forgets everything seen.
func (d *Detector) Reset() {
	d.samples = d.samples[:0]
	d.changes, d.lastDir = 0, 0
	d.lastTurn = time.Time{}
}

// Update takes the X position of the mouse at time now and says whether the
// shake is complete. As in C#, the reversals are counted from the last Reset
// (only the samples are limited to Window), and two reversals closer than
// 40 ms are one.
func (d *Detector) Update(now time.Time, x float64) bool {
	d.samples = append(d.samples, sample{now, x})
	for len(d.samples) > 0 && now.Sub(d.samples[0].t) > d.Window {
		d.samples = d.samples[1:]
	}
	if len(d.samples) < 3 {
		return false
	}
	prev, cur := d.samples[len(d.samples)-2], d.samples[len(d.samples)-1]
	dx := cur.x - prev.x
	if abs(dx) < d.MinDeltaX {
		return false
	}
	dir := 1
	if dx <= 0 {
		dir = -1
	}
	if d.lastDir == 0 {
		d.lastDir = dir
		return false
	}
	if dir != d.lastDir {
		if !d.lastTurn.IsZero() && now.Sub(d.lastTurn) < 40*time.Millisecond {
			d.lastDir = dir
			return false
		}
		// (C# starts lastTurn at MinValue, so the first reversal is never
		// "too soon".)
		d.changes++
		d.lastTurn = now
		d.lastDir = dir
		if d.changes >= d.RequiredDirectionChanges {
			return true
		}
	}
	return false
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
