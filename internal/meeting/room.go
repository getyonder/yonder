package meeting

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	"yonder/internal/nearby"
)

// A room brings together two devices that are not at the same address, so
// that they can pair as nearby devices do: one opens the room and is given
// its code, its owner tells the other owner the code, and the other device
// comes with it. The code only brings the two together. What keeps the
// pairing safe is the code the two owners compare afterwards (package
// nearby), so the meeting point, which knows every room's code, gains
// nothing by it, and somebody who guesses one gets as far as a stranger at
// a shared address does.
//
// A room takes two devices. It ends when either leaves, after roomLife if
// no second device has come, and roomLife after the second came, which is
// time enough to pair.

const (
	// roomLife is how long a room waits for its second device, as long as
	// a pairing link lasts, and how long it lasts once that has come.
	roomLife = 10 * time.Minute
	// missWindow and missPause are the time in which the misses of one
	// address are counted, and how long it is refused after too many.
	missWindow = 10 * time.Minute
	missPause  = 10 * time.Minute
	// maxMisses is the most counts of misses kept at once, and missSweep
	// how often the counts that no longer matter are looked for. While
	// the list is full, rooms are not looked for.
	maxMisses = 30_000
	missSweep = time.Minute
)

// room is what a group that is a room has besides its members.
type room struct {
	code   string
	owner  string // the address that opened it, as GroupOf names it
	block  string // and its block, as BlockOf names it
	opened time.Time
	met    time.Time // when the second device came; no other may then
	ended  bool
}

// misses counts the rooms one address, or one block, asked for in vain.
type misses struct {
	start  time.Time
	missed int
	paused time.Time // refused until then
}

// openRoom opens a room for a member and puts it there, or says why not.
func (p *Point) openRoom(key string, m *member) (reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	owned, blockOwned := 0, 0
	for _, g := range p.rooms {
		if p.roomOver(g, now) {
			continue // and no longer in p.rooms
		}
		if g.room.owner == key {
			owned++
		}
		if g.room.block == m.block {
			blockOwned++
		}
	}
	if owned >= p.limits.AddressRooms || blockOwned >= p.limits.BlockRooms ||
		len(p.rooms) >= p.limits.Rooms || !p.hasPlace(m.block) {
		return ReasonFull
	}
	code := newCode()
	for p.rooms[code] != nil {
		code = newCode()
	}
	g := &group{members: map[string]*member{}, room: &room{code: code, owner: key, block: m.block, opened: now}}
	p.rooms[code] = g
	p.count(RoomOpened)
	p.enter(g, m, now)
	return ""
}

// joinRoom puts a member in the room with the given code, or says why not.
func (p *Point) joinRoom(code, key string, m *member) (reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	// The block first, so that a paused block makes no counts for its
	// addresses.
	block := p.missesOf("block "+m.block, now)
	if block == nil || now.Before(block.paused) {
		return ReasonBusy
	}
	address := p.missesOf("address "+key, now)
	if address == nil || now.Before(address.paused) {
		return ReasonBusy
	}
	g := p.rooms[code]
	if g == nil || !g.room.met.IsZero() || p.roomOver(g, now) {
		address.miss(now, p.limits.Misses)
		block.miss(now, p.limits.BlockMisses)
		return ReasonNoRoom
	}
	if !p.hasPlace(m.block) {
		return ReasonFull
	}
	g.room.met = now
	p.enter(g, m, now)
	return ""
}

// roomOver reports whether a room has ended, and ends one that has waited
// too long for its second device or has had both for roomLife. Called with
// the lock held.
func (p *Point) roomOver(g *group, now time.Time) bool {
	since := g.room.opened
	if !g.room.met.IsZero() {
		since = g.room.met
	}
	if !g.room.ended && now.Sub(since) >= roomLife {
		p.endRoom(g)
	}
	return g.room.ended
}

// endRoom closes a room to everyone. A device still in it is told so when
// it next says something, which is within nearby.PingEvery. Called with
// the lock held.
func (p *Point) endRoom(g *group) {
	if g.room.ended {
		return
	}
	g.room.ended = true
	delete(p.rooms, g.room.code)
}

// missesOf finds or makes the count for an address or a block. When it has
// to make one, it forgets the counts that no longer matter, at most once in
// missSweep, and it returns nil if the list is still full. Called with the
// lock held.
func (p *Point) missesOf(key string, now time.Time) *misses {
	if c := p.misses[key]; c != nil {
		return c
	}
	if now.Sub(p.swept) >= missSweep {
		p.swept = now
		for k, old := range p.misses {
			if now.Sub(old.start) >= missWindow && !now.Before(old.paused) {
				delete(p.misses, k)
			}
		}
	}
	if len(p.misses) >= maxMisses {
		return nil
	}
	c := &misses{start: now}
	p.misses[key] = c
	return c
}

// miss counts one room asked for in vain.
func (c *misses) miss(now time.Time, limit int) {
	if now.Sub(c.start) >= missWindow {
		c.start, c.missed = now, 0
	}
	if c.missed++; c.missed >= limit {
		c.paused, c.missed = now.Add(missPause), 0
	}
}

func newCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err) // the system has no random numbers
	}
	return fmt.Sprintf("%0*d", nearby.RoomDigits, n)
}
