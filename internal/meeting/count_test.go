package meeting

import (
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"yonder/internal/nearby"
)

// counted is the events a meeting point has reported, in order.
type counted struct {
	mu     sync.Mutex
	events []Event
}

func (c *counted) add(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

// expect checks what has been reported since the last call.
func (c *counted) expect(t *testing.T, want ...Event) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Equal(c.events, want) {
		t.Fatalf("the meeting point reported %v, want %v", c.events, want)
	}
	c.events = nil
}

// pass sends a body from one device to another and waits until it is
// there, by when the meeting point has reported it.
func pass(t *testing.T, from, to *device, b string) {
	t.Helper()
	from.send(nearby.Message{Type: nearby.TypeTo, ID: to.id, Body: json.RawMessage(b)})
	if msg := to.next(); msg.Type != nearby.TypeFrom || msg.ID != from.id || string(msg.Body) != b {
		t.Fatalf("expected %s to be passed on as it was, got %+v", b, msg)
	}
}

// The meeting point reports a room that is opened, and a pairing that
// starts (ask) and whose link is sent (link): by code in a room, nearby
// otherwise. Other messages are passed and not reported.
func TestCount(t *testing.T) {
	p, url := point(t, DefaultLimits)
	var c counted
	p.Count = c.add

	near1 := arrive(t, url, "203.0.113.7", "Near A")
	near2 := arrive(t, url, "203.0.113.7", "Near B")
	near1.next() // Near B's arrival
	c.expect(t)
	pass(t, near1, near2, `{"k":"ask","commit":"AAAA"}`)
	c.expect(t, NearbyStarted)
	pass(t, near2, near1, `{"k":"key","pub":"AAAA"}`)
	pass(t, near1, near2, `{"k":"open","pub":"AAAA"}`)
	pass(t, near2, near1, `"ask"`)
	pass(t, near2, near1, `{"kind":"link"}`)
	c.expect(t)
	pass(t, near2, near1, `{"k":"link","box":"AAAA"}`)
	c.expect(t, NearbyCompleted)

	a, code := open(t, url, "203.0.113.7", "A")
	c.expect(t, RoomOpened)
	b := enter(t, url, "198.51.100.9", "B", code)
	a.next() // B's arrival
	c.expect(t)
	pass(t, b, a, `{"k":"ask","commit":"AAAA"}`)
	c.expect(t, CodeStarted)
	pass(t, a, b, `{"k":"no"}`)
	pass(t, b, a, `{"k":"ask","commit":"AAAA"}`)
	pass(t, a, b, `{"k":"link","box":"AAAA"}`)
	c.expect(t, CodeStarted, CodeCompleted)

	// A message that is not passed is not a pairing: here, one to a
	// device that is not in the room. The message after it shows that the
	// meeting point has dealt with it.
	b.send(nearby.Message{Type: nearby.TypeTo, ID: near1.id, Body: json.RawMessage(`{"k":"ask"}`)})
	pass(t, b, a, `{"k":"key"}`)
	c.expect(t)
}
