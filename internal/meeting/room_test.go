package meeting

import (
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"

	"yonder/internal/nearby"
)

// roomHello is a hello that asks for a room: nearby.RoomNew, or a code.
func roomHello(name, room string) nearby.Message {
	msg := hello(name)
	msg.Room = room
	return msg
}

// open connects from an address and opens a room; the code is in d.room.
func open(t *testing.T, url, from, name string) (d *device, code string) {
	t.Helper()
	d = &device{t: t, c: dial(t, url, from)}
	d.send(roomHello(name, nearby.RoomNew))
	msg := d.next()
	if msg.Type != nearby.TypeWelcome || msg.ID == "" || len(msg.Room) != nearby.RoomDigits {
		t.Fatalf("%s: expected a welcome to a room, got %+v", name, msg)
	}
	d.id = msg.ID
	return d, msg.Room
}

// enter connects from an address and goes to the room with a code.
func enter(t *testing.T, url, from, name, code string) *device {
	t.Helper()
	d := &device{t: t, c: dial(t, url, from)}
	d.send(roomHello(name, code))
	msg := d.next()
	if msg.Type != nearby.TypeWelcome || msg.Room != code {
		t.Fatalf("%s: expected a welcome to room %s, got %+v", name, code, msg)
	}
	d.id, d.here = msg.ID, msg.Peers
	return d
}

// refused connects from an address, asks for a room and returns the reason
// the meeting point ends the connection with, which it must give both in a
// last message and as the reason of the close.
func refused(t *testing.T, url, from, room string) string {
	t.Helper()
	d := &device{t: t, c: dial(t, url, from)}
	d.send(roomHello("X", room))
	msg := d.next()
	if msg.Type != nearby.TypeBye || msg.Reason == "" {
		t.Fatalf("expected a bye with a reason, got %+v", msg)
	}
	if reason := d.closed(); reason != msg.Reason {
		t.Fatalf("the bye says %q and the close %q", msg.Reason, reason)
	}
	return msg.Reason
}

// wrong is a code that is not the given one.
func wrong(code string) string {
	if code == "000000" {
		return "000001"
	}
	return "000000"
}

// Two devices from different addresses meet in a room by its code and can
// pass messages; devices at their addresses that asked for no room see
// neither of them.
func TestRoom(t *testing.T) {
	_, url := point(t, DefaultLimits)
	near := arrive(t, url, "203.0.113.7", "Near A")
	a, code := open(t, url, "203.0.113.7", "A")
	b := enter(t, url, "198.51.100.9", "B", code)
	if names(b.here) != "A" {
		t.Fatalf("B saw %q in the room, want A", names(b.here))
	}
	if msg := a.next(); msg.Type != nearby.TypeJoin || msg.Peer == nil || msg.Peer.Name != "B" || msg.Peer.ID != b.id {
		t.Fatalf("A expected B's arrival, got %+v", msg)
	}
	b.send(nearby.Message{Type: nearby.TypeTo, ID: a.id, Body: body("hi")})
	if msg := a.next(); msg.Type != nearby.TypeFrom || msg.ID != b.id || string(msg.Body) != `"hi"` {
		t.Fatalf("A got %+v", msg)
	}
	// The room's devices are not among the devices of A's address: one that
	// arrives there now finds only the one that asked for no room, and that
	// one hears of it next, not of A or B before it.
	later := arrive(t, url, "203.0.113.7", "Near B")
	if names(later.here) != "Near A" {
		t.Fatalf("a device at A's address saw %q, want Near A", names(later.here))
	}
	if msg := near.next(); msg.Type != nearby.TypeJoin || msg.Peer.Name != "Near B" {
		t.Fatalf("the device at A's address heard %+v first, want Near B's arrival", msg)
	}
	// A message to a device outside the room is not passed.
	b.send(nearby.Message{Type: nearby.TypeTo, ID: near.id, Body: body("out")})
	b.send(nearby.Message{Type: nearby.TypeTo, ID: a.id, Body: body("in")})
	if msg := a.next(); string(msg.Body) != `"in"` {
		t.Fatalf("A got %+v", msg)
	}
	arrive(t, url, "203.0.113.7", "Near C") // B's messages have been handled by now
	if msg := near.next(); msg.Type != nearby.TypeJoin || msg.Peer.Name != "Near C" {
		t.Fatalf("the device at A's address heard %+v, want Near C's arrival", msg)
	}
}

// A code that names no room is refused, and so is a room that has its two
// devices: a third cannot come in, with the same reason.
func TestRoomTakesTwo(t *testing.T) {
	_, url := point(t, DefaultLimits)
	a, code := open(t, url, "203.0.113.7", "A")
	if reason := refused(t, url, "198.51.100.9", wrong(code)); reason != ReasonNoRoom {
		t.Fatalf("a wrong code: closed with %q, want %q", reason, ReasonNoRoom)
	}
	enter(t, url, "198.51.100.9", "B", code)
	a.next() // B's arrival
	if reason := refused(t, url, "192.0.2.1", code); reason != ReasonNoRoom {
		t.Fatalf("a third device: closed with %q, want %q", reason, ReasonNoRoom)
	}
}

// A room ends when either device leaves: the other hears of the leaving,
// is turned out when it next says something, and nobody can come with the
// code again.
func TestRoomEndsWhenOneLeaves(t *testing.T) {
	_, url := point(t, DefaultLimits)
	a, code := open(t, url, "203.0.113.7", "A")
	b := enter(t, url, "198.51.100.9", "B", code)
	a.next() // B's arrival
	b.c.Close(websocket.StatusNormalClosure, "")
	if msg := a.next(); msg.Type != nearby.TypeLeave || msg.ID != b.id {
		t.Fatalf("A expected B's leaving, got %+v", msg)
	}
	if reason := refused(t, url, "198.51.100.9", code); reason != ReasonNoRoom {
		t.Fatalf("coming back: closed with %q, want %q", reason, ReasonNoRoom)
	}
	a.send(nearby.Message{Type: nearby.TypePong})
	if reason := a.closed(); reason != ReasonNoRoom {
		t.Fatalf("A: closed with %q, want %q", reason, ReasonNoRoom)
	}

	// The same when the one that opened it leaves before anyone came.
	c, code := open(t, url, "203.0.113.7", "C")
	d, other := open(t, url, "203.0.113.7", "D")
	c.c.Close(websocket.StatusNormalClosure, "")
	// C's leaving has been handled once the meeting point, which takes one
	// thing at a time, has let E into D's room after it.
	enter(t, url, "198.51.100.9", "E", other)
	d.next()
	if reason := refused(t, url, "198.51.100.9", code); reason != ReasonNoRoom {
		t.Fatalf("a room whose opener left: closed with %q, want %q", reason, ReasonNoRoom)
	}
}

// A room waits ten minutes for its second device. After that the code
// names no room and the device that opened it is turned out; a room that
// has its two devices ends ten minutes after the second came.
func TestRoomLife(t *testing.T) {
	p, url := point(t, DefaultLimits)
	clock := &fakeClock{now: time.Now()}
	p.now = clock.Now

	met, metCode := open(t, url, "203.0.113.7", "Met")
	other := enter(t, url, "198.51.100.9", "Other", metCode)
	met.next() // Other's arrival
	a, code := open(t, url, "203.0.113.7", "A")

	clock.Add(roomLife - time.Second)
	b := enter(t, url, "198.51.100.9", "B", code) // still in time
	a.next()
	b.c.Close(websocket.StatusNormalClosure, "")
	a.next() // B's leaving

	c, code := open(t, url, "203.0.113.7", "C")
	clock.Add(roomLife)
	if reason := refused(t, url, "198.51.100.9", code); reason != ReasonNoRoom {
		t.Fatalf("after ten minutes: closed with %q, want %q", reason, ReasonNoRoom)
	}
	c.send(nearby.Message{Type: nearby.TypePong})
	if reason := c.closed(); reason != ReasonNoRoom {
		t.Fatalf("the opener after ten minutes: closed with %q, want %q", reason, ReasonNoRoom)
	}
	// The room that had met at the start has ended too: both its devices
	// are told when they next say something.
	for _, d := range []*device{met, other} {
		d.send(nearby.Message{Type: nearby.TypePong})
		if reason := d.closed(); reason != ReasonNoRoom {
			t.Fatalf("a room ten minutes after it met: closed with %q, want %q", reason, ReasonNoRoom)
		}
	}
}

// A room that has met lasts until ten minutes after its second device came,
// not longer.
func TestMetRoomLife(t *testing.T) {
	p, url := point(t, DefaultLimits)
	clock := &fakeClock{now: time.Now()}
	p.now = clock.Now

	a, code := open(t, url, "203.0.113.7", "A")
	clock.Add(roomLife - time.Second)
	b := enter(t, url, "198.51.100.9", "B", code)
	a.next() // B's arrival
	clock.Add(roomLife - time.Second)
	b.send(nearby.Message{Type: nearby.TypeTo, ID: a.id, Body: body("still")})
	if msg := a.next(); msg.Type != nearby.TypeFrom || string(msg.Body) != `"still"` {
		t.Fatalf("a second before its end: got %+v", msg)
	}
	clock.Add(time.Second)
	b.send(nearby.Message{Type: nearby.TypePong})
	if reason := b.closed(); reason != ReasonNoRoom {
		t.Fatalf("ten minutes after B came: closed with %q, want %q", reason, ReasonNoRoom)
	}
}

// One address may have so many rooms open, and the meeting point so many
// in all; a room that has ended, or waited too long, is a place free.
func TestRoomCaps(t *testing.T) {
	limits := DefaultLimits
	limits.AddressRooms, limits.Rooms = 2, 3
	p, url := point(t, limits)
	clock := &fakeClock{now: time.Now()}
	p.now = clock.Now

	a, _ := open(t, url, "203.0.113.7", "A")
	open(t, url, "203.0.113.7", "B")
	if reason := refused(t, url, "203.0.113.7", nearby.RoomNew); reason != ReasonFull {
		t.Fatalf("a third room from one address: closed with %q, want %q", reason, ReasonFull)
	}
	_, code := open(t, url, "198.51.100.9", "C") // another address may
	if reason := refused(t, url, "192.0.2.1", nearby.RoomNew); reason != ReasonFull {
		t.Fatalf("a fourth room in all: closed with %q, want %q", reason, ReasonFull)
	}

	// A's room ends with A. That has been handled once D, after it, is in
	// C's room.
	a.c.Close(websocket.StatusNormalClosure, "")
	enter(t, url, "192.0.2.1", "D", code)
	open(t, url, "203.0.113.7", "E")
	if reason := refused(t, url, "192.0.2.1", nearby.RoomNew); reason != ReasonFull {
		t.Fatalf("with three rooms again: closed with %q, want %q", reason, ReasonFull)
	}

	// B's and E's rooms have waited too long, and C's has had both its
	// devices for as long: all three places are free.
	clock.Add(roomLife)
	open(t, url, "203.0.113.7", "F")
	open(t, url, "203.0.113.7", "G")
	open(t, url, "192.0.2.1", "H")
	if reason := refused(t, url, "198.51.100.9", nearby.RoomNew); reason != ReasonFull {
		t.Fatalf("with three new rooms: closed with %q, want %q", reason, ReasonFull)
	}
}

// One block of addresses may open only BlockRooms rooms, from however many
// of its addresses; the devices in rooms count for their block and in total,
// and give their places back when they leave.
func TestRoomsOfOneBlock(t *testing.T) {
	p, url := point(t, DefaultLimits)
	var opener, last *device
	for n := range DefaultLimits.BlockRooms {
		from := fmt.Sprintf("2001:db8:1:%x::1", n) // a new /64 in the same /48
		var code string
		opener, code = open(t, url, from, "Opener")
		last = enter(t, url, fmt.Sprintf("2001:db8:1:%x::2", 0x8000+n), "Joiner", code)
		opener.next() // the joiner's arrival
	}
	if reason := refused(t, url, "2001:db8:1:ffff::1", nearby.RoomNew); reason != ReasonFull {
		t.Fatalf("a room past BlockRooms from a new /64 of the block: closed with %q, want %q", reason, ReasonFull)
	}
	held := 2 * DefaultLimits.BlockRooms
	p.mu.Lock()
	inBlock, total := p.blocks["2001:db8:1::/48"], p.total
	p.mu.Unlock()
	if inBlock != held || total != held {
		t.Fatalf("devices in rooms counted: %d for the block and %d in total, want %d", inBlock, total, held)
	}

	// The last joiner leaves: its place is given back before its opener is
	// told.
	last.c.Close(websocket.StatusNormalClosure, "")
	if msg := opener.next(); msg.Type != nearby.TypeLeave {
		t.Fatalf("the opener: got %+v, want the joiner's leaving", msg)
	}
	p.mu.Lock()
	inBlock, total = p.blocks["2001:db8:1::/48"], p.total
	p.mu.Unlock()
	if inBlock != held-1 || total != held-1 {
		t.Fatalf("after a joiner left: %d counted for the block and %d in total, want %d", inBlock, total, held-1)
	}
}

// Devices in rooms count for the Block limit: a block that has used its
// places with rooms cannot connect one more nearby device, nor go into a
// room.
func TestRoomMembersCountForBlock(t *testing.T) {
	limits := DefaultLimits
	limits.Block = 3
	_, url := point(t, limits)
	_, code := open(t, url, "203.0.113.1", "A")
	enter(t, url, "203.0.113.2", "B", code)
	_, code = open(t, url, "203.0.113.3", "C")
	arrive(t, url, "198.51.100.9", "Other") // another block may
	d := &device{t: t, c: dial(t, url, "203.0.113.4")}
	d.send(hello("D"))
	if msg := d.next(); msg.Type != nearby.TypeBye || msg.Reason != ReasonFull {
		t.Fatalf("a nearby device past the block's places: got %+v, want a bye %q", msg, ReasonFull)
	}
	if reason := refused(t, url, "203.0.113.5", code); reason != ReasonFull {
		t.Fatalf("a room member past the block's places: closed with %q, want %q", reason, ReasonFull)
	}
}

// An address that asks for too many rooms that are not there is refused
// for a while, the right code included; other addresses are not, until
// their block has asked for too many.
func TestRoomMisses(t *testing.T) {
	limits := DefaultLimits
	limits.Misses, limits.BlockMisses = 3, 5
	for _, from := range [][3]string{
		{"203.0.113.1", "203.0.113.2", "203.0.113.3"},
		{"2001:db8:1:1::1", "2001:db8:1:2::1", "2001:db8:1:3::1"},
	} {
		p, url := point(t, limits)
		clock := &fakeClock{now: time.Now()}
		p.now = clock.Now
		a, code := open(t, url, "198.51.100.9", "A")

		for i := range 3 {
			if reason := refused(t, url, from[0], wrong(code)); reason != ReasonNoRoom {
				t.Fatalf("%s, miss %d: closed with %q, want %q", from[0], i+1, reason, ReasonNoRoom)
			}
		}
		for _, room := range []string{wrong(code), code} {
			if reason := refused(t, url, from[0], room); reason != ReasonBusy {
				t.Fatalf("%s, after three misses: closed with %q, want %q", from[0], reason, ReasonBusy)
			}
		}
		// The block has three misses. Another address in it has two more,
		// and then the whole block is refused.
		for i := range 2 {
			if reason := refused(t, url, from[1], wrong(code)); reason != ReasonNoRoom {
				t.Fatalf("%s, miss %d: closed with %q, want %q", from[1], i+1, reason, ReasonNoRoom)
			}
		}
		if reason := refused(t, url, from[2], code); reason != ReasonBusy {
			t.Fatalf("%s, the block having missed five times: closed with %q, want %q", from[2], reason, ReasonBusy)
		}
		// Another block is not affected, and opening a room is not asking
		// for one.
		if reason := refused(t, url, "192.0.2.1", wrong(code)); reason != ReasonNoRoom {
			t.Fatalf("another block: closed with %q, want %q", reason, ReasonNoRoom)
		}
		open(t, url, from[0], "B")

		// Just before the pause is over, still refused; then not.
		clock.Add(missPause - time.Second)
		if reason := refused(t, url, from[2], code); reason != ReasonBusy {
			t.Fatalf("%s, a second early: closed with %q, want %q", from[2], reason, ReasonBusy)
		}
		clock.Add(time.Second)
		// A's room has waited too long by now; a new one is found.
		a.c.Close(websocket.StatusNormalClosure, "")
		_, code = open(t, url, "198.51.100.9", "C")
		enter(t, url, from[0], "D", code)
	}
}

// Misses are counted within ten minutes: older ones are forgotten.
func TestRoomMissesAreForgotten(t *testing.T) {
	limits := DefaultLimits
	limits.Misses = 3
	p, url := point(t, limits)
	clock := &fakeClock{now: time.Now()}
	p.now = clock.Now
	for round := range 2 {
		for i := range 2 {
			if reason := refused(t, url, "203.0.113.1", "123456"); reason != ReasonNoRoom {
				t.Fatalf("round %d, miss %d: closed with %q, want %q", round, i+1, reason, ReasonNoRoom)
			}
		}
		clock.Add(missWindow)
	}
	_, code := open(t, url, "198.51.100.9", "A")
	enter(t, url, "203.0.113.1", "B", code)
}

// Something that is neither "new" nor a code names no room.
func TestRoomNames(t *testing.T) {
	_, url := point(t, DefaultLimits)
	for _, room := range []string{"x", "12345", "1234567", "NEW", fmt.Sprint(-12345)} {
		if reason := refused(t, url, "203.0.113.1", room); reason != ReasonNoRoom {
			t.Fatalf("room %q: closed with %q, want %q", room, reason, ReasonNoRoom)
		}
	}
}

// A block that is paused makes no new counts for its addresses: wrong
// codes from many /64s of one /48 leave at most one count for each address
// that missed before the pause, one for the block and one for its wide block.
func TestRoomMissesOfPausedBlock(t *testing.T) {
	p, url := point(t, DefaultLimits)
	for n := range 300 {
		refused(t, url, fmt.Sprintf("2001:db8:2:%x::1", n), "123456")
	}
	p.mu.Lock()
	size := len(p.misses)
	p.mu.Unlock()
	if size > DefaultLimits.BlockMisses+2 {
		t.Fatalf("%d counts after 300 wrong codes from one /48, want at most %d", size, DefaultLimits.BlockMisses+2)
	}
}

// Over IPv6 a wide block (a /32) has a limit on misses of its own: wrong
// codes from many /48s, each under its block's limit, get the whole wide
// block refused, the right code included. Another wide block is not, and
// neither is IPv4, which has no wide blocks.
func TestRoomMissesOfWideBlock(t *testing.T) {
	limits := DefaultLimits
	limits.WideMisses = 5
	_, url := point(t, limits)
	_, code := open(t, url, "198.51.100.9", "A")
	for n := range 5 {
		from := fmt.Sprintf("2001:db8:%x::1", n) // a new /48 in the same /32
		if reason := refused(t, url, from, wrong(code)); reason != ReasonNoRoom {
			t.Fatalf("%s: closed with %q, want %q", from, reason, ReasonNoRoom)
		}
	}
	if reason := refused(t, url, "2001:db8:ffff::1", code); reason != ReasonBusy {
		t.Fatalf("a new /48 of the wide block, after five misses: closed with %q, want %q", reason, ReasonBusy)
	}
	if reason := refused(t, url, "2001:db9::1", wrong(code)); reason != ReasonNoRoom {
		t.Fatalf("another wide block: closed with %q, want %q", reason, ReasonNoRoom)
	}
	for n := range 6 {
		from := fmt.Sprintf("203.0.%d.1", n) // a new /24 each
		if reason := refused(t, url, from, wrong(code)); reason != ReasonNoRoom {
			t.Fatalf("%s: closed with %q, want %q", from, reason, ReasonNoRoom)
		}
	}
	enter(t, url, "203.0.113.1", "B", code)
}

// A device that brings the right code leaves no count behind.
func TestRightCodeMakesNoCount(t *testing.T) {
	p, url := point(t, DefaultLimits)
	_, code := open(t, url, "198.51.100.9", "A")
	enter(t, url, "2001:db8:1:1::1", "B", code)
	p.mu.Lock()
	size := len(p.misses)
	p.mu.Unlock()
	if size != 0 {
		t.Fatalf("%d counts of misses after a right code, want none", size)
	}
}

// The list of counts has a largest size. While it is full, a miss that has
// no count is not counted and nobody is refused for that: the right code
// still gets in. Old counts are looked for at most once a minute, and then
// forgotten.
func TestRoomMissesList(t *testing.T) {
	p, url := point(t, DefaultLimits)
	clock := &fakeClock{now: time.Now()}
	p.now = clock.Now

	count := func(key string) *misses {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.missesOf(key, p.now())
	}
	has := func(key string) bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.misses[key] != nil
	}

	count("old")
	clock.Add(missWindow - 30*time.Second)
	count("new") // looks for old counts; "old" is not old yet
	clock.Add(30 * time.Second)
	count("newer") // half a minute after the last look: "old" is kept
	if !has("old") {
		t.Fatal("old counts were looked for again within a minute")
	}
	clock.Add(30 * time.Second)
	count("newest")
	if has("old") {
		t.Fatal("an old count was kept a minute after it ended")
	}

	p.mu.Lock()
	for i := len(p.misses); i < maxMisses; i++ {
		p.misses[fmt.Sprint("filler ", i)] = &misses{start: p.now()}
	}
	p.mu.Unlock()
	if c := count("one more"); c != nil {
		t.Fatal("a count was made in a full list")
	}
	_, code := open(t, url, "198.51.100.9", "A")
	if reason := refused(t, url, "203.0.113.1", wrong(code)); reason != ReasonNoRoom {
		t.Fatalf("a wrong code with the list full: closed with %q, want %q", reason, ReasonNoRoom)
	}
	if has("address 203.0.113.1") {
		t.Fatal("a count was made in a full list")
	}
	enter(t, url, "203.0.113.1", "B", code)
}

// One wide block of IPv6 addresses may open only WideRooms rooms, from
// however many of its blocks.
func TestRoomsOfOneWideBlock(t *testing.T) {
	limits := DefaultLimits
	limits.WideRooms = 3
	_, url := point(t, limits)
	for n := range 3 {
		open(t, url, fmt.Sprintf("2001:db8:%x::1", n), "Opener") // a new /48 in the same /32
	}
	if reason := refused(t, url, "2001:db8:ffff::1", nearby.RoomNew); reason != ReasonFull {
		t.Fatalf("a room past WideRooms from a new /48 of the wide block: closed with %q, want %q", reason, ReasonFull)
	}
	open(t, url, "2001:db9::1", "Another wide block")
}
