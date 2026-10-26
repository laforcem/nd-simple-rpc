package engine

import "time"

// limiter is a per-user rolling-window call limiter: a ring holding the
// times of the last max calls.
type limiter struct {
	ring []time.Time
	next int
}

// allow reports whether one more call at now keeps at most max calls in any
// window. A call exactly window after the oldest one still counts as inside.
func (l *limiter) allow(now time.Time, max int, window time.Duration) bool {
	if len(l.ring) < max {
		return true
	}
	return now.Sub(l.ring[l.next]) > window
}

func (l *limiter) record(now time.Time, max int) {
	if len(l.ring) < max {
		l.ring = append(l.ring, now)
		return
	}
	l.ring[l.next] = now
	l.next = (l.next + 1) % max
}

// idle reports whether no recorded call is still inside the window.
func (l *limiter) idle(now time.Time, window time.Duration) bool {
	for _, t := range l.ring {
		if now.Sub(t) <= window {
			return false
		}
	}
	return true
}
