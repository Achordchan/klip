package shelf

import (
	"errors"
	"time"
)

// ErrUnsupported is returned where there is no global mouse hook: the shelf
// is a Windows feature.
var ErrUnsupported = errors.New("shelf: global mouse events are only available on Windows")

// Events are the mouse events the shelf listens to, from anywhere on the
// screen. They are called one at a time, on a goroutine of their own.
type Events struct {
	LeftDown func(time.Time)
	LeftUp   func(time.Time)
	Move     func(x, y int, at time.Time)
}
