package gui

import "time"

// A sender counts a file's bytes when it hands them to the connection, not
// when they arrive. The connection takes them a burst at a time, with a
// pause while it empties, so the count stands still and then jumps, and is
// ahead of what has arrived. This file makes what the window shows of a
// send move steadily: it follows the count, spread over about twice the
// pause between bursts. A count that comes without pauses is shown as it
// is, and so is one that has stopped.

// easeMost is the longest time what is shown may be spread over.
const easeMost = 3 * time.Second

// eased is the count of one send and how it comes.
type eased struct {
	// sent is the count, and heard when it last came.
	sent  int64
	heard time.Time
	// pause is how far apart the bursts have come lately: the longest
	// time between two counts, which fades slowly when they come closer.
	pause time.Duration
	// at is when what is shown last moved.
	at time.Time
}

// count takes the sender's count at a moment.
func (e *eased) count(now time.Time, sent int64) {
	if !e.heard.IsZero() {
		gap := now.Sub(e.heard)
		e.pause = max(gap, e.pause-gap/10)
	}
	e.sent, e.heard = sent, now
}

// show is what to show at a moment, given what is shown now: nearer the
// count, never past it and never back.
func (e *eased) show(now time.Time, shown int64) int64 {
	dt := now.Sub(e.at)
	e.at = now
	behind := e.sent - shown
	over := min(2*e.pause, easeMost)
	// A count that has stood still for longer than its bursts explain is
	// not waiting for the next one: there is nothing to spread.
	if behind <= 0 || dt >= over || now.Sub(e.heard) >= 3*over {
		return max(shown, e.sent)
	}
	// Rounded up, so that it always arrives.
	step := (behind*int64(dt) + int64(over) - 1) / int64(over)
	return shown + step
}
