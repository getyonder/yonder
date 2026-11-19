package meeting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"yonder/internal/nearby"
)

// wait is the longest any step of a test may take. Every wait in this file
// goes through it; nothing sleeps.
const wait = 10 * time.Second

// until waits for cond to hold.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(wait); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// point starts a meeting point that takes each visitor's address from the
// "from" part of the URL, so a test can arrive from anywhere.
func point(t *testing.T, limits Limits) (*Point, string) {
	t.Helper()
	p := New(limits)
	p.addr = func(r *http.Request) (netip.Addr, error) { return netip.ParseAddr(r.URL.Query().Get("from")) }
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// device is one connection to the meeting point.
type device struct {
	t    *testing.T
	c    *websocket.Conn
	id   string
	here []nearby.Peer // who was there on arrival
}

func dial(t *testing.T, url, from string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url+"?from="+from, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func hello(name string) nearby.Message {
	return nearby.Message{Type: nearby.TypeHello, Peer: &nearby.Peer{
		Name: name, Kind: nearby.KindBrowser, Salt: strings.Repeat("0a", 16), Tag: strings.Repeat("1b", 32),
	}}
}

// arrive connects from an address, says hello and takes the welcome.
func arrive(t *testing.T, url, from, name string) *device {
	t.Helper()
	d := &device{t: t, c: dial(t, url, from)}
	d.send(hello(name))
	msg := d.next()
	if msg.Type != nearby.TypeWelcome || msg.ID == "" {
		t.Fatalf("%s: expected a welcome, got %+v", name, msg)
	}
	d.id, d.here = msg.ID, msg.Peers
	return d
}

func (d *device) send(msg nearby.Message) {
	d.t.Helper()
	b, _ := json.Marshal(msg)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if err := d.c.Write(ctx, websocket.MessageText, b); err != nil {
		d.t.Fatal(err)
	}
}

// next is the next message from the meeting point.
func (d *device) next() nearby.Message {
	d.t.Helper()
	msg, err := d.read()
	if err != nil {
		d.t.Fatal(err)
	}
	return msg
}

func (d *device) read() (nearby.Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var msg nearby.Message
	_, b, err := d.c.Read(ctx)
	if err != nil {
		return msg, err
	}
	return msg, json.Unmarshal(b, &msg)
}

// closed waits for the meeting point to end the connection and returns its
// reason, passing over anything sent before.
func (d *device) closed() string {
	d.t.Helper()
	for {
		_, err := d.read()
		if err == nil {
			continue
		}
		var ce websocket.CloseError
		if !errors.As(err, &ce) {
			d.t.Fatalf("expected the meeting point to close the connection, got %v", err)
		}
		return ce.Reason
	}
}

func names(peers []nearby.Peer) string {
	var out []string
	for _, p := range peers {
		out = append(out, p.Name)
	}
	return strings.Join(out, ",")
}

func body(s string) json.RawMessage { b, _ := json.Marshal(s); return b }

// Two devices from one address see each other and can pass messages; a
// third from another address sees neither and cannot reach them.
func TestGroups(t *testing.T) {
	_, url := point(t, DefaultLimits)
	a := arrive(t, url, "203.0.113.7", "A")
	b := arrive(t, url, "203.0.113.7", "B")
	c := arrive(t, url, "198.51.100.9", "C")

	if len(a.here) != 0 || names(b.here) != "A" || len(c.here) != 0 {
		t.Fatalf("on arrival: A saw %q, B saw %q, C saw %q", names(a.here), names(b.here), names(c.here))
	}
	if msg := a.next(); msg.Type != nearby.TypeJoin || msg.Peer.Name != "B" || msg.Peer.ID != b.id {
		t.Fatalf("A expected B to be announced, got %+v", msg)
	}

	// C tries to reach A; then B does. Messages to one device keep their
	// order, so if C's had been passed on, A would see it first.
	c.send(nearby.Message{Type: nearby.TypeTo, ID: a.id, Body: body("from C")})
	d := arrive(t, url, "198.51.100.9", "D") // C's message has been handled once C hears of D
	if msg := c.next(); msg.Type != nearby.TypeJoin || msg.Peer.Name != "D" {
		t.Fatalf("C expected D to be announced, got %+v", msg)
	}
	b.send(nearby.Message{Type: nearby.TypeTo, ID: a.id, Body: body("from B")})
	if msg := a.next(); msg.Type != nearby.TypeFrom || msg.ID != b.id || string(msg.Body) != `"from B"` {
		t.Fatalf("A expected B's message, got %+v", msg)
	}
	if names(d.here) != "C" {
		t.Fatalf("D saw %q, want C", names(d.here))
	}
}

func TestGroupOf(t *testing.T) {
	same := [][2]string{
		{"203.0.113.7", "203.0.113.7"},
		{"203.0.113.7", "::ffff:203.0.113.7"},
		{"2001:db8:1:2::1", "2001:db8:1:2:aaaa:bbbb:cccc:dddd"},
	}
	for _, pair := range same {
		if a, b := GroupOf(netip.MustParseAddr(pair[0])), GroupOf(netip.MustParseAddr(pair[1])); a != b {
			t.Errorf("%s and %s are in groups %s and %s, want one group", pair[0], pair[1], a, b)
		}
	}
	apart := [][2]string{
		{"203.0.113.7", "203.0.113.8"},
		{"2001:db8:1:2::1", "2001:db8:1:3::1"},
	}
	for _, pair := range apart {
		if a, b := GroupOf(netip.MustParseAddr(pair[0])), GroupOf(netip.MustParseAddr(pair[1])); a == b {
			t.Errorf("%s and %s are both in group %s, want two groups", pair[0], pair[1], a)
		}
	}
}

// IPv6 devices meet when they share a /64 and not otherwise.
func TestIPv6Groups(t *testing.T) {
	_, url := point(t, DefaultLimits)
	arrive(t, url, "2001:db8:1:2::1", "A")
	b := arrive(t, url, "2001:db8:1:2:aaaa:bbbb:cccc:dddd", "B")
	c := arrive(t, url, "2001:db8:1:3::1", "C")
	if names(b.here) != "A" || len(c.here) != 0 {
		t.Fatalf("B saw %q (want A), C saw %q (want nobody)", names(b.here), names(c.here))
	}
}

// A device that leaves is announced to its group.
func TestLeave(t *testing.T) {
	p, url := point(t, DefaultLimits)
	a := arrive(t, url, "203.0.113.7", "A")
	b := arrive(t, url, "203.0.113.7", "B")
	a.next() // B's arrival
	b.c.Close(websocket.StatusNormalClosure, "")
	if msg := a.next(); msg.Type != nearby.TypeLeave || msg.ID != b.id {
		t.Fatalf("A expected B's leaving, got %+v", msg)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.total != 1 {
		t.Errorf("%d devices counted, want 1", p.total)
	}
}

// One more device than an address may have is refused; the ones already
// there stay, and a place freed is a place to take.
func TestGroupCap(t *testing.T) {
	limits := DefaultLimits
	limits.Group = 2
	_, url := point(t, limits)
	a := arrive(t, url, "203.0.113.7", "A")
	b := arrive(t, url, "203.0.113.7", "B")
	a.next() // B's arrival

	c := &device{t: t, c: dial(t, url, "203.0.113.7")}
	c.send(hello("C"))
	if reason := c.closed(); reason != ReasonFull {
		t.Fatalf("third device: closed with %q, want %q", reason, ReasonFull)
	}
	// Another address is not affected.
	arrive(t, url, "198.51.100.9", "D")

	b.c.Close(websocket.StatusNormalClosure, "")
	a.next() // B's leaving, so the place is free
	e := arrive(t, url, "203.0.113.7", "E")
	if names(e.here) != "A" {
		t.Fatalf("E saw %q, want A", names(e.here))
	}
}

// One block of addresses (an IPv4 /24, an IPv6 /48) has a limit of its own
// besides each address's, so that many addresses cannot take every place.
func TestBlockCap(t *testing.T) {
	limits := DefaultLimits
	limits.Group, limits.Block = 2, 3
	for _, from := range [][4]string{
		{"203.0.113.1", "203.0.113.1", "203.0.113.2", "203.0.113.3"},
		{"2001:db8:1:1::1", "2001:db8:1:1::2", "2001:db8:1:2::1", "2001:db8:1:ffff::1"},
	} {
		_, url := point(t, limits)
		a := arrive(t, url, from[0], "A")
		b := arrive(t, url, from[1], "B")
		a.next() // B's arrival
		arrive(t, url, from[2], "C")
		d := &device{t: t, c: dial(t, url, from[3])}
		d.send(hello("D"))
		if reason := d.closed(); reason != ReasonFull {
			t.Fatalf("fourth device in the block of %s: closed with %q, want %q", from[0], reason, ReasonFull)
		}
		// Another block is not affected, and a place that is left is
		// free again.
		arrive(t, url, "198.51.100.9", "E")
		arrive(t, url, "2001:db8:2::1", "F")
		a.c.Close(websocket.StatusNormalClosure, "")
		b.next() // A's leaving, so its place is free
		arrive(t, url, from[3], "D again")
	}
}

// Over IPv6 a wide block (a /32) has a limit of its own, so that many
// blocks cannot take every place. IPv4 has no wide blocks.
func TestWideCap(t *testing.T) {
	limits := DefaultLimits
	limits.Wide = 3
	p, url := point(t, limits)
	var first *device
	for n := range 3 {
		d := arrive(t, url, fmt.Sprintf("2001:db8:%x::1", n), "A") // a new /48 in the same /32
		if first == nil {
			first = d
		}
	}
	d := &device{t: t, c: dial(t, url, "2001:db8:ffff::1")}
	d.send(hello("D"))
	if reason := d.closed(); reason != ReasonFull {
		t.Fatalf("fourth device in the wide block: closed with %q, want %q", reason, ReasonFull)
	}
	// No group was made for the device that had no place.
	p.mu.Lock()
	groups := len(p.groups)
	p.mu.Unlock()
	if groups != 3 {
		t.Fatalf("%d groups, want 3: none for a device with no place", groups)
	}
	// Another wide block and IPv4 are not affected, and a place that is
	// left is free again.
	arrive(t, url, "2001:db9::1", "E")
	for n := range 4 {
		arrive(t, url, fmt.Sprintf("203.0.%d.1", n), "F")
	}
	first.c.Close(websocket.StatusNormalClosure, "")
	until(t, "the first device's place to be free", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.blocks["2001:db8::/32"] == 2
	})
	arrive(t, url, "2001:db8:ffff::1", "D again")
}

// Empty groups are looked for at most once a minute, and all of them are
// forgotten at once when maxGroups are kept.
func TestGroupsAreForgotten(t *testing.T) {
	p, _ := point(t, DefaultLimits)
	now := time.Now()
	empty := func(key string, start time.Time) {
		p.groups[key] = &group{members: map[string]*member{}, budget: budget{start: start}}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	empty("old", now.Add(-2*time.Minute))
	p.sweep(now)
	if p.groups["old"] != nil {
		t.Fatal("an empty group was kept after its minute")
	}
	empty("old", now.Add(-2*time.Minute))
	p.sweep(now.Add(30 * time.Second))
	if p.groups["old"] == nil {
		t.Fatal("empty groups were looked for again within a minute")
	}
	for i := len(p.groups); i < maxGroups; i++ {
		empty(fmt.Sprint("filler ", i), now)
	}
	p.groups["held"] = &group{members: map[string]*member{"m": {}}}
	p.sweep(now.Add(31 * time.Second))
	if len(p.groups) != 1 || p.groups["held"] == nil {
		t.Fatalf("%d groups after a look with the list full, want the one with a device in it", len(p.groups))
	}
}

// Connections that do not say hello are limited in number: one more lets
// the one that has waited longest go, and the others stay.
func TestWaitingForHello(t *testing.T) {
	limits := DefaultLimits
	limits.Waiting = 2
	p, url := point(t, limits)
	// silent connects, says nothing, and returns when the meeting point
	// has it among those that wait, so that the order is certain.
	silent := func(waiting int) *device {
		d := &device{t: t, c: dial(t, url, "203.0.113.1")}
		until(t, "the connection to wait for its hello", func() bool { return p.waiting.Len() == waiting })
		return d
	}
	first, second := silent(1), silent(2)
	third := silent(2)
	if msg, err := first.read(); err == nil {
		t.Fatalf("the one that waited longest: got %+v, want its connection ended", msg)
	}
	// The two others are still let in.
	for _, d := range []*device{second, third} {
		d.send(hello("Late"))
		if msg := d.next(); msg.Type != nearby.TypeWelcome {
			t.Fatalf("one that still waited: got %+v, want a welcome", msg)
		}
	}
	until(t, "nobody to wait", func() bool { return p.waiting.Len() == 0 })
}

func TestTotalCap(t *testing.T) {
	limits := DefaultLimits
	limits.Total = 2
	_, url := point(t, limits)
	arrive(t, url, "203.0.113.1", "A")
	arrive(t, url, "203.0.113.2", "B")
	c := &device{t: t, c: dial(t, url, "203.0.113.3")}
	c.send(hello("C"))
	if reason := c.closed(); reason != ReasonFull {
		t.Fatalf("closed with %q, want %q", reason, ReasonFull)
	}
}

// A device that sends more than its share in a minute is dropped, and its
// group is told it left. When the minute is over it may come back.
func TestDeviceMessageCap(t *testing.T) {
	limits := DefaultLimits
	limits.PerDevice = 5 // arriving is the first
	p, url := point(t, limits)
	clock := &fakeClock{now: time.Now()}
	p.now = clock.Now
	a := arrive(t, url, "203.0.113.7", "A")
	b := arrive(t, url, "203.0.113.7", "B")
	a.next() // B's arrival

	for i := range 5 {
		b.send(nearby.Message{Type: nearby.TypeTo, ID: a.id, Body: body(fmt.Sprint(i))})
	}
	if reason := b.closed(); reason != ReasonBusy {
		t.Fatalf("closed with %q, want %q", reason, ReasonBusy)
	}
	// A got the four that were within the limit, then B's leaving.
	for i := range 4 {
		if msg := a.next(); msg.Type != nearby.TypeFrom || string(msg.Body) != fmt.Sprintf("%q", fmt.Sprint(i)) {
			t.Fatalf("message %d: got %+v", i, msg)
		}
	}
	if msg := a.next(); msg.Type != nearby.TypeLeave || msg.ID != b.id {
		t.Fatalf("A expected B's leaving, got %+v", msg)
	}

	clock.Add(time.Minute)
	arrive(t, url, "203.0.113.7", "B again")
}

// The devices of one address share a limit too: a message that goes over it
// is dropped, not its sender, who may be an honest device next to a noisy
// one; new arrivals are refused until the minute is over.
func TestGroupMessageCap(t *testing.T) {
	limits := DefaultLimits
	limits.PerGroup = 6
	p, url := point(t, limits)
	clock := &fakeClock{now: time.Now()}
	p.now = clock.Now
	a := arrive(t, url, "203.0.113.7", "A") // 1
	b := arrive(t, url, "203.0.113.7", "B") // 2
	a.next()                                // B's arrival
	for i := range 2 {                      // 3 to 6
		a.send(nearby.Message{Type: nearby.TypeTo, ID: b.id, Body: body(fmt.Sprint(i))})
		b.next()
		b.send(nearby.Message{Type: nearby.TypeTo, ID: a.id, Body: body(fmt.Sprint(i))})
		a.next()
	}
	a.send(nearby.Message{Type: nearby.TypeTo, ID: b.id, Body: body("one too many")})
	c := &device{t: t, c: dial(t, url, "203.0.113.7")}
	c.send(hello("C"))
	if reason := c.closed(); reason != ReasonBusy {
		t.Fatalf("new arrival: closed with %q, want %q", reason, ReasonBusy)
	}
	// Another address has its own limit.
	arrive(t, url, "198.51.100.9", "D")

	clock.Add(time.Minute)
	arrive(t, url, "203.0.113.7", "E")
	b.next() // E's arrival
	// A is still there, and its message over the limit never arrived.
	a.send(nearby.Message{Type: nearby.TypeTo, ID: b.id, Body: body("next minute")})
	if msg := b.next(); msg.Type != nearby.TypeFrom || string(msg.Body) != `"next minute"` {
		t.Fatalf("B expected A's message of the next minute, got %+v", msg)
	}
}

// Only small messages are taken.
func TestMessageSize(t *testing.T) {
	_, url := point(t, DefaultLimits)
	a := arrive(t, url, "203.0.113.7", "A")
	b := arrive(t, url, "203.0.113.7", "B")
	a.next() // B's arrival
	b.send(nearby.Message{Type: nearby.TypeTo, ID: a.id, Body: body(strings.Repeat("x", nearby.MaxMessage))})
	b.closed()
	if msg := a.next(); msg.Type != nearby.TypeLeave {
		t.Fatalf("A expected B's leaving and nothing of its message, got %+v", msg)
	}
}

// A connection must say who it is, acceptably, before anything else.
func TestHello(t *testing.T) {
	_, url := point(t, DefaultLimits)
	watcher := arrive(t, url, "203.0.113.7", "Watcher")
	bad := map[string]nearby.Message{
		"no hello":       {Type: nearby.TypeTo, ID: watcher.id, Body: body("hi")},
		"no name":        hello(""),
		"spaced name":    hello(" A "),
		"long name":      hello(strings.Repeat("n", 65)),
		"unknown kind":   {Type: nearby.TypeHello, Peer: &nearby.Peer{Name: "A", Kind: "toaster", Salt: strings.Repeat("0a", 16), Tag: strings.Repeat("1b", 32)}},
		"short salt":     {Type: nearby.TypeHello, Peer: &nearby.Peer{Name: "A", Kind: nearby.KindApp, Salt: "0a", Tag: strings.Repeat("1b", 32)}},
		"tag not hex":    {Type: nearby.TypeHello, Peer: &nearby.Peer{Name: "A", Kind: nearby.KindApp, Salt: strings.Repeat("0a", 16), Tag: strings.Repeat("zz", 32)}},
		"no peer at all": {Type: nearby.TypeHello},
	}
	for what, msg := range bad {
		d := &device{t: t, c: dial(t, url, "203.0.113.7")}
		d.send(msg)
		if _, err := d.read(); err == nil {
			t.Errorf("%s: was let in", what)
		}
	}
	// None of them was ever announced: the next thing the watcher hears
	// of is a proper arrival.
	arrive(t, url, "203.0.113.7", "Proper")
	if msg := watcher.next(); msg.Type != nearby.TypeJoin || msg.Peer.Name != "Proper" {
		t.Fatalf("watcher expected Proper, got %+v", msg)
	}
}

// The id a device claims in its hello is not used.
func TestIDIsGiven(t *testing.T) {
	_, url := point(t, DefaultLimits)
	a := arrive(t, url, "203.0.113.7", "A")
	msg := hello("B")
	msg.Peer.ID = a.id
	b := &device{t: t, c: dial(t, url, "203.0.113.7")}
	b.send(msg)
	if w := b.next(); w.ID == a.id {
		t.Fatal("B was given the id it asked for, which is A's")
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// While the web version is closed a browser is turned away and a desktop
// app is not; when it opens again the browser gets in.
func TestBrowsersTurnedAwayWhileClosed(t *testing.T) {
	p, url := point(t, DefaultLimits)
	var open atomic.Bool
	p.Browsers = open.Load

	browser := &device{t: t, c: dial(t, url, "192.0.2.1")}
	browser.send(hello("browser"))
	if got := browser.closed(); got != ReasonClosed {
		t.Fatalf("a browser was turned away with %q, want %q", got, ReasonClosed)
	}

	desktop := &device{t: t, c: dial(t, url, "192.0.2.1")}
	app := hello("app")
	app.Peer.Kind = nearby.KindApp
	desktop.send(app)
	if msg := desktop.next(); msg.Type != nearby.TypeWelcome {
		t.Fatalf("a desktop app got %+v, want a welcome", msg)
	}

	open.Store(true)
	if again := arrive(t, url, "192.0.2.1", "browser"); names(again.here) != "app" {
		t.Fatalf("the browser found %q there, want the app", names(again.here))
	}
}

// A device's version goes to the others with its name; one that is not a
// version is not passed on, and the device is let in all the same.
func TestVersionIsPassedOn(t *testing.T) {
	_, url := point(t, DefaultLimits)
	said := map[string]string{"A": "0.1.31", "B": "dev", "C": "", "D": "<b>9.9.9</b>", "E": strings.Repeat("1", 60)}
	want := map[string]string{"A": "0.1.31", "B": "dev", "C": "", "D": "", "E": ""}
	for _, name := range []string{"A", "B", "C", "D", "E"} {
		d := &device{t: t, c: dial(t, url, "203.0.113.7")}
		msg := hello(name)
		msg.Peer.Version = said[name]
		d.send(msg)
		if msg := d.next(); msg.Type != nearby.TypeWelcome {
			t.Fatalf("%s: expected a welcome, got %+v", name, msg)
		}
	}
	last := arrive(t, url, "203.0.113.7", "Z")
	if len(last.here) != len(want) {
		t.Fatalf("Z sees %q", names(last.here))
	}
	for _, p := range last.here {
		if p.Version != want[p.Name] {
			t.Errorf("%s: version %q passed on, want %q", p.Name, p.Version, want[p.Name])
		}
	}
}
