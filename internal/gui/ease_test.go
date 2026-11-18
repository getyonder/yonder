package gui

import (
	"testing"
	"time"
)

// A count that comes a burst a second is shown moving at every look, in
// steps far smaller than a burst, never past the count and never back, and
// it arrives once the count stops.
func TestBurstsAreShownSteadily(t *testing.T) {
	const burst = 1 << 20
	var e eased
	now := time.Unix(0, 0)
	var sent, shown int64
	for look := 0; look < 200; look++ {
		now = now.Add(redrawEvery)
		if look%10 == 0 {
			// A burst: the count grows in pieces, all at once.
			for piece := 0; piece < 32; piece++ {
				sent += burst / 32
				e.count(now, sent)
			}
		}
		next := e.show(now, shown)
		step := next - shown
		shown = next
		if shown > sent {
			t.Fatalf("look %d: shown %d is past the count %d", look, shown, sent)
		}
		if step < 0 {
			t.Fatalf("look %d: what is shown went back by %d", look, -step)
		}
		// The first bursts teach it how far apart they come.
		if look < 30 {
			continue
		}
		if step == 0 {
			t.Errorf("look %d: what is shown stood still, %d behind the count", look, sent-shown)
		}
		if step > burst/4 {
			t.Errorf("look %d: what is shown jumped by %d, a burst is %d", look, step, burst)
		}
	}
	for look := 0; look < 100 && shown < sent; look++ {
		now = now.Add(redrawEvery)
		shown = e.show(now, shown)
	}
	if shown != sent {
		t.Errorf("10 s after the count stopped at %d, %d is shown", sent, shown)
	}
}

// A count that comes without pauses is shown as it is.
func TestASteadyCountIsShownAsItIs(t *testing.T) {
	var e eased
	now := time.Unix(0, 0)
	var sent, shown int64
	for look := 0; look < 50; look++ {
		for piece := 0; piece < 4; piece++ {
			now = now.Add(redrawEvery / 4)
			sent += 32 << 10
			e.count(now, sent)
		}
		if shown = e.show(now, shown); shown != sent {
			t.Fatalf("look %d: shown %d, the count is %d", look, shown, sent)
		}
	}
}
