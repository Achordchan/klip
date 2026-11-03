package shelf

import "time"

// mouseClock unwraps Windows' 32-bit millisecond event counter. Only event
// intervals matter to the gesture; queue latency must not change them.
type mouseClock struct {
	tick    uint32
	elapsed time.Duration
}

func (c *mouseClock) at(tick uint32) time.Time {
	c.elapsed += time.Duration(tick-c.tick) * time.Millisecond
	c.tick = tick
	return time.Unix(0, 0).Add(c.elapsed)
}
