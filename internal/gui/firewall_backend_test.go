package gui

import (
	"testing"

	"yonder/internal/firewall"
)

// With nothing to read, another Yonder on the network finds out: one that
// cannot connect raises a notice with no button, one that can raises none.
func TestNearbyYonderTests(t *testing.T) {
	meeting := meetingPoint(t)
	nothing := Options{Meeting: meeting, Firewall: func(int) firewall.Status { return firewall.Status{} }}
	a, aw, _ := deviceWith(t, "Nimble Harbour", nothing)
	// Nothing listens where a says it is, as if a firewall dropped it.
	a.mu.Lock()
	a.addrs = func(int) []string { return []string{"127.0.0.1:1"} }
	a.mu.Unlock()
	b, bw, _ := deviceWith(t, "Swift Comet", nothing)

	st := aw.until(t, "a's notice", func(st State) bool { return st.Blocked != nil })
	if st.Blocked.Title != "Something is keeping other devices out" || st.Blocked.Button != "" {
		t.Fatalf("a's notice: %+v", st.Blocked)
	}
	bw.until(t, "b to have been tested", func(State) bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return !b.askedAt.IsZero()
	})
	if st := b.State(); st.Blocked != nil {
		t.Fatalf("b can be reached, but shows: %+v", st.Blocked)
	}
}

// A firewall that is on but whose rules cannot be read is blamed once
// another Yonder cannot connect, and then the button is offered.
func TestNearbyYonderBlamesFirewall(t *testing.T) {
	meeting := meetingPoint(t)
	a, aw, _ := deviceWith(t, "Nimble Harbour", Options{Meeting: meeting, Firewall: func(int) firewall.Status { return firewall.Status{On: true} }})
	a.mu.Lock()
	a.addrs = func(int) []string { return []string{"127.0.0.1:1"} }
	a.mu.Unlock()
	deviceWith(t, "Swift Comet", Options{Meeting: meeting})

	st := aw.until(t, "a's notice", func(st State) bool { return st.Blocked != nil })
	if st.Blocked.Title != "This computer's firewall is keeping other devices out" || st.Blocked.Button != "Let them in…" {
		t.Fatalf("a's notice: %+v", st.Blocked)
	}
}

// When the system is seen to send the network's traffic elsewhere and
// another Yonder cannot connect, the notice names that, with no button:
// the firewall is not the cause.
func TestNearbyYonderBlamesDivertedTraffic(t *testing.T) {
	meeting := meetingPoint(t)
	a, aw, _ := deviceWith(t, "Nimble Harbour", Options{Meeting: meeting, Firewall: func(int) firewall.Status { return firewall.Status{On: true, Diverted: true} }})
	a.mu.Lock()
	a.addrs = func(int) []string { return []string{"127.0.0.1:1"} }
	a.mu.Unlock()
	deviceWith(t, "Swift Comet", Options{Meeting: meeting})

	st := aw.until(t, "a's notice", func(st State) bool { return st.Blocked != nil })
	if st.Blocked.Title != "A VPN on this computer is taking your network's traffic" || st.Blocked.Button != "" {
		t.Fatalf("a's notice: %+v", st.Blocked)
	}
}
