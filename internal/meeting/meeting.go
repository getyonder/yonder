// Package meeting is the meeting point on Yonder's server: it introduces
// devices that arrive from the same public address to each other and
// passes their messages along (see package nearby for why and for what a
// device does with it). It keeps who is connected in memory only, writes
// nothing to disk and logs nothing about who met whom. For the server's
// totals it can say that a pairing started or that its link was sent
// (Point.Count), never between whom.
package meeting

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"yonder/internal/nearby"
	"yonder/internal/tally/agent"
)

// Limits keep one address from crowding the meeting point or the devices
// behind it. Strangers can share an address (a mobile carrier, an office),
// so they apply to each group and to each device in it.
type Limits struct {
	// Group is the most devices one address may have connected.
	Group int
	// Block is the most devices one block of addresses (see BlockOf) may
	// have connected, so that someone with many addresses cannot take
	// every place.
	Block int
	// Total is the most devices connected at once, over all addresses.
	Total int
	// PerDevice and PerGroup are the most messages a device, and all the
	// devices of one address together, may send in a minute. Arriving
	// counts as a message. A device over its own limit is dropped; a
	// message over its address's limit is dropped instead, so that a
	// stranger at the same address cannot get an honest device dropped.
	PerDevice int
	PerGroup  int
	// Rooms is the most rooms open at once, AddressRooms the most that one
	// address may have opened, and BlockRooms the most that one block of
	// addresses may have opened (see room.go). The devices in a room count
	// for Block and Total as any other.
	Rooms        int
	AddressRooms int
	BlockRooms   int
	// Misses is how many times one address may ask for a room that is not
	// there within missWindow before it is refused for missPause;
	// BlockMisses is the same for one block of addresses.
	Misses      int
	BlockMisses int
}

// DefaultLimits are the limits Yonder's server runs with. Pairing takes
// about five messages from each device. A block holds two addresses' worth,
// so filling the meeting point takes at least 32 blocks, and filling its
// rooms at least 25.
var DefaultLimits = Limits{
	Group: 32, Block: 64, Total: 2000, PerDevice: 40, PerGroup: 240,
	Rooms: 200, AddressRooms: 4, BlockRooms: 8, Misses: 30, BlockMisses: 120,
}

// Reasons a connection is refused or dropped, sent as the close reason.
const (
	ReasonFull = nearby.ReasonFull
	ReasonBusy = nearby.ReasonBusy
	// ReasonClosed is for a browser while the web version is closed.
	ReasonClosed = "closed"
	ReasonNoRoom = nearby.ReasonNoRoom
)

const (
	// helloWait is how long a new connection has to say who it is.
	helloWait = 10 * time.Second
	// writeWait is how long one message may take to send.
	writeWait = 10 * time.Second
	// backlog is how many messages may wait for a slow device before it
	// is dropped.
	backlog = 32
)

// Point is a meeting point. The zero value is not usable; call New.
type Point struct {
	limits Limits
	// Browsers, if set, says whether browsers may join just now; the
	// desktop apps always may. Set it before the first connection.
	Browsers func() bool
	// Origins is the origins of the pages, besides a page of the host
	// asked for, that may connect from a browser, each as "https://name":
	// the web app's, which is hosted apart from the meeting point. A page
	// of any other site is refused, so no other site can use the rooms
	// from a visitor's browser. An app sends no origin and is not held to
	// this.
	Origins []string
	// Count, if set, is told of each thing in Events as it happens, so
	// the server can keep totals. It is called with the meeting point
	// locked and must return at once. Set it before the first connection.
	Count func(Event)
	// addr finds a visitor's address; tests replace it.
	addr func(r *http.Request) (netip.Addr, error)
	// now is the clock; tests replace it.
	now func() time.Time

	mu     sync.Mutex
	total  int
	groups map[string]*group
	blocks map[string]int     // devices connected, by block
	rooms  map[string]*group  // rooms open, by code (room.go)
	misses map[string]*misses // rooms asked for in vain, by address and by block
	swept  time.Time          // when misses was last cleared of old counts
}

// Event is something the meeting point can report to Point.Count. It says
// that a thing happened and nothing of who did it.
type Event int

const (
	// RoomOpened: a device opened a room, to pair by code.
	RoomOpened Event = iota
	// CodeStarted and CodeCompleted: in a room, a device asked the other
	// to pair, and a device sent the other the pairing link.
	CodeStarted
	CodeCompleted
	// NearbyStarted and NearbyCompleted: the same between the devices of
	// one address.
	NearbyStarted
	NearbyCompleted
)

// count reports an event, if anyone asked to hear of them.
func (p *Point) count(e Event) {
	if p.Count != nil {
		p.Count(e)
	}
}

// countPassed reports the pairing step, if any, that a message passed
// within g is. The kind of the message is the one thing read of its body;
// that the link was sent is all "completed" can mean here, since the link
// is sealed between the devices.
func (p *Point) countPassed(g *group, body json.RawMessage) {
	if p.Count == nil {
		return
	}
	switch kind := nearby.PairingKind(body); {
	case kind == nearby.PairingAsk && g.room != nil:
		p.count(CodeStarted)
	case kind == nearby.PairingAsk:
		p.count(NearbyStarted)
	case kind == nearby.PairingLink && g.room != nil:
		p.count(CodeCompleted)
	case kind == nearby.PairingLink:
		p.count(NearbyCompleted)
	}
}

// group is the devices of one address. It outlives its last member until
// its minute is over, so reconnecting does not reset the count. A room is a
// group too, of the two devices that came with its code: see room.go.
type group struct {
	members map[string]*member
	budget  budget
	room    *room // nil for the devices of an address
}

type member struct {
	peer   nearby.Peer
	group  *group
	block  string
	out    chan []byte
	budget budget
	// kick ends the connection of a member that has stopped taking
	// messages.
	kick func()
}

// budget counts messages in the current minute.
type budget struct {
	start time.Time
	used  int
}

// spend takes one message from the budget and reports whether there was one.
func (b *budget) spend(now time.Time, limit int) bool {
	if now.Sub(b.start) >= time.Minute {
		b.start, b.used = now, 0
	}
	b.used++
	return b.used <= limit
}

// New returns a meeting point that groups visitors by the address their
// connection comes from.
func New(limits Limits) *Point {
	return &Point{limits: limits, addr: remoteAddr, now: time.Now, groups: map[string]*group{}, blocks: map[string]int{}, rooms: map[string]*group{}, misses: map[string]*misses{}}
}

func remoteAddr(r *http.Request) (netip.Addr, error) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	return ap.Addr(), err
}

// GroupOf names the group an address belongs to: the address itself for
// IPv4, and the /64 for IPv6, since a home network has a whole /64 and each
// device in it its own address.
func GroupOf(a netip.Addr) string {
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	p, _ := a.WithZone("").Prefix(64)
	return p.String()
}

// BlockOf names the block an address belongs to, for the limit on places:
// the /24 for IPv4 and the /48 for IPv6, which is what one customer or
// site is commonly given.
func BlockOf(a netip.Addr) string {
	a = a.Unmap()
	bits := 48
	if a.Is4() {
		bits = 24
	}
	p, _ := a.WithZone("").Prefix(bits)
	return p.String()
}

// ServeHTTP takes one device's connection and holds it until it ends.
func (p *Point) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a, err := p.addr(r)
	if err != nil || !a.IsValid() {
		http.Error(w, "no address", http.StatusBadRequest)
		return
	}
	// The library's rule: a browser's Origin must be the host asked for,
	// or one of Origins.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: p.Origins})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(nearby.MaxMessage)
	p.serve(r.Context(), c, GroupOf(a), BlockOf(a))
}

func (p *Point) serve(ctx context.Context, c *websocket.Conn, key, block string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	helloCtx, helloDone := context.WithTimeout(ctx, helloWait)
	hello, err := read(helloCtx, c)
	helloDone()
	if err != nil || hello.Type != nearby.TypeHello || hello.Peer == nil || !validPeer(*hello.Peer) {
		c.Close(websocket.StatusPolicyViolation, "say hello first")
		return
	}

	if hello.Peer.Kind == nearby.KindBrowser && p.Browsers != nil && !p.Browsers() {
		bye(ctx, c, websocket.StatusTryAgainLater, ReasonClosed)
		return
	}

	// A version that is not one is not passed on; the device is still let
	// in, as an older Yonder that says none is.
	if v := hello.Peer.Version; v != "dev" && !agent.KnownVersion(v) {
		hello.Peer.Version = ""
	}
	m := &member{peer: *hello.Peer, block: block, out: make(chan []byte, backlog), kick: cancel}
	m.peer.ID = newID()
	var reason string
	switch hello.Room {
	case "":
		reason = p.join(key, m)
	case nearby.RoomNew:
		reason = p.openRoom(key, m)
	default:
		reason = p.joinRoom(hello.Room, key, m)
	}
	if reason != "" {
		bye(ctx, c, websocket.StatusTryAgainLater, reason)
		return
	}
	defer p.leave(m)

	go p.write(ctx, cancel, c, m)
	for {
		readCtx, readDone := context.WithTimeout(ctx, nearby.Quiet)
		msg, err := read(readCtx, c)
		readDone()
		if err != nil {
			return
		}
		if reason := p.handle(m, msg); reason != "" {
			bye(ctx, c, websocket.StatusPolicyViolation, reason)
			return
		}
	}
}

// bye ends a connection and says why, twice: in a last message, which
// every device can read, and as the reason of the close, which a web
// page's connection cannot.
func bye(ctx context.Context, c *websocket.Conn, code websocket.StatusCode, reason string) {
	ctx, done := context.WithTimeout(ctx, writeWait)
	defer done()
	c.Write(ctx, websocket.MessageText, encode(nearby.Message{Type: nearby.TypeBye, Reason: reason}))
	c.Close(code, reason)
}

// write sends a member its messages and a ping now and then.
func (p *Point) write(ctx context.Context, cancel context.CancelFunc, c *websocket.Conn, m *member) {
	defer cancel()
	ping := time.NewTicker(nearby.PingEvery)
	defer ping.Stop()
	pingMsg := encode(nearby.Message{Type: nearby.TypePing})
	for {
		var b []byte
		select {
		case <-ctx.Done():
			return
		case b = <-m.out:
		case <-ping.C:
			b = pingMsg
		}
		writeCtx, done := context.WithTimeout(ctx, writeWait)
		err := c.Write(writeCtx, websocket.MessageText, b)
		done()
		if err != nil {
			return
		}
	}
}

// join adds a member to the group of its address and introduces it, or
// says why not.
func (p *Point) join(key string, m *member) (reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	g := p.groups[key]
	if g == nil {
		p.sweep(now)
		g = &group{members: map[string]*member{}}
		p.groups[key] = g
	}
	if !g.budget.spend(now, p.limits.PerGroup) {
		return ReasonBusy
	}
	if len(g.members) >= p.limits.Group || !p.hasPlace(m.block) {
		return ReasonFull
	}
	p.enter(g, m, now)
	return ""
}

// hasPlace reports whether one more device of a block may connect, under
// the limits for its block and in total. Called with the lock held.
func (p *Point) hasPlace(block string) bool {
	return p.blocks[block] < p.limits.Block && p.total < p.limits.Total
}

// enter puts a member in a group, counts it for its block and in total,
// tells it who is there and tells them it has come. Called with the lock
// held.
func (p *Point) enter(g *group, m *member, now time.Time) {
	p.blocks[m.block]++
	p.total++
	m.group = g
	m.budget.spend(now, p.limits.PerDevice)
	welcome := nearby.Message{Type: nearby.TypeWelcome, ID: m.peer.ID, Peers: []nearby.Peer{}}
	if g.room != nil {
		welcome.Room = g.room.code
	}
	arrived := encode(nearby.Message{Type: nearby.TypeJoin, Peer: &m.peer})
	for _, other := range g.members {
		welcome.Peers = append(welcome.Peers, other.peer)
		other.send(arrived)
	}
	g.members[m.peer.ID] = m
	m.send(encode(welcome))
}

// leave takes a member out, gives its place back and tells the rest of its
// group. A room ends with it.
func (p *Point) leave(m *member) {
	p.mu.Lock()
	defer p.mu.Unlock()
	g := m.group
	delete(g.members, m.peer.ID)
	if p.blocks[m.block]--; p.blocks[m.block] == 0 {
		delete(p.blocks, m.block)
	}
	p.total--
	if g.room != nil {
		p.endRoom(g)
	}
	gone := encode(nearby.Message{Type: nearby.TypeLeave, ID: m.peer.ID})
	for _, other := range g.members {
		other.send(gone)
	}
}

// sweep forgets empty groups whose minute is over. Called with the lock held.
func (p *Point) sweep(now time.Time) {
	for key, g := range p.groups {
		if len(g.members) == 0 && now.Sub(g.budget.start) >= time.Minute {
			delete(p.groups, key)
		}
	}
}

// handle deals with one message from a member. A reason in return means
// its connection is to be closed with it.
func (p *Point) handle(m *member, msg nearby.Message) (reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	g := m.group
	if g.room != nil && p.roomOver(g, now) {
		return ReasonNoRoom
	}
	if !m.budget.spend(now, p.limits.PerDevice) {
		return ReasonBusy
	}
	switch msg.Type {
	case nearby.TypePong:
		return ""
	case nearby.TypeTo:
		if !g.budget.spend(now, p.limits.PerGroup) {
			return "" // dropped; the sender may be the honest one
		}
		// Only ever within the group: another group's ids are not found.
		if to := g.members[msg.ID]; to != nil && to != m {
			p.countPassed(g, msg.Body)
			to.send(encode(nearby.Message{Type: nearby.TypeFrom, ID: m.peer.ID, Body: msg.Body}))
		}
		return ""
	default:
		return "unknown message"
	}
}

// send queues a message for a member, dropping the member if it has stopped
// taking them. It never blocks, so it is safe under the lock.
func (m *member) send(b []byte) {
	select {
	case m.out <- b:
	default:
		m.kick()
	}
}

func read(ctx context.Context, c *websocket.Conn) (nearby.Message, error) {
	var msg nearby.Message
	typ, b, err := c.Read(ctx)
	if err != nil {
		return msg, err
	}
	if typ != websocket.MessageText {
		return msg, errors.New("not a text message")
	}
	return msg, json.Unmarshal(b, &msg)
}

func encode(msg nearby.Message) []byte {
	b, err := json.Marshal(msg)
	if err != nil {
		panic(err) // only our own types, which always encode
	}
	return b
}

// validPeer reports whether a device describes itself acceptably.
func validPeer(p nearby.Peer) bool {
	name := strings.TrimSpace(p.Name)
	return name != "" && name == p.Name && utf8.RuneCountInString(name) <= 64 &&
		(p.Kind == nearby.KindBrowser || p.Kind == nearby.KindApp) &&
		isHex(p.Salt, 32) && isHex(p.Tag, 64)
}

// isHex reports whether s is n lower-case hexadecimal digits.
func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
