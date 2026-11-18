package nearby_test

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"yonder/internal/meeting"
	"yonder/internal/nearby"
)

// wait is the longest any step of a test may take. Every wait in this file
// goes through until or within; nothing sleeps for a fixed time.
const wait = 20 * time.Second

// until polls for a condition, failing the test if it never holds.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// within is a context for one step.
func within(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	t.Cleanup(cancel)
	return ctx
}

// serve starts a meeting point (or something posing as one) and returns
// its address. Every test client arrives from 127.0.0.1, so all are nearby.
func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// device is a client with a record of who asked it to pair.
type device struct {
	*nearby.Client
	asked chan *nearby.Session
}

// start runs a client until the test ends.
func start(t *testing.T, url, name string) *device {
	t.Helper()
	d := &device{asked: make(chan *nearby.Session, 4)}
	d.Client = &nearby.Client{URL: url, Name: name, Kind: nearby.KindBrowser, Fingerprint: "fingerprint of " + name}
	d.Asked = func(s *nearby.Session) { d.asked <- s }
	// Keys that are the same on every run, different for every device.
	d.SetRandom(&counter{seed: name})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return d
}

// counter is an endless stream of bytes decided by its seed.
type counter struct {
	seed string
	n    uint64
	buf  []byte
}

func (c *counter) Read(p []byte) (int, error) {
	for len(c.buf) < len(p) {
		h := sha256.New()
		h.Write([]byte(c.seed))
		binary.Write(h, binary.BigEndian, c.n)
		c.n++
		c.buf = h.Sum(c.buf)
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

// sees waits until a device sees exactly the named others.
func (d *device) sees(t *testing.T, names ...string) {
	t.Helper()
	want := strings.Join(names, ",")
	until(t, d.Name+" seeing "+want, func() bool { return namesOf(d.Peers()) == want })
}

func namesOf(peers []nearby.Peer) string {
	var out []string
	for _, p := range peers {
		out = append(out, p.Name)
	}
	return strings.Join(out, ",")
}

// peer is the nearby device of that name, as d sees it.
func (d *device) peer(t *testing.T, name string) nearby.Peer {
	t.Helper()
	for _, p := range d.Peers() {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("%s does not see %s", d.Name, name)
	return nearby.Peer{}
}

// request waits for the next pairing request a device is shown.
func (d *device) request(t *testing.T) *nearby.Session {
	t.Helper()
	select {
	case s := <-d.asked:
		return s
	case <-time.After(wait):
		t.Fatalf("%s was never asked", d.Name)
		return nil
	}
}

// ended waits for a session to be over and returns why.
func ended(t *testing.T, s *nearby.Session) error {
	t.Helper()
	select {
	case <-s.Done():
		return s.Err()
	case <-time.After(wait):
		t.Fatal("the session never ended")
		return nil
	}
}

// Two devices find each other, recognise a known fingerprint, agree on a
// code and pass a link that arrives as it was sent.
func TestPair(t *testing.T) {
	url := serve(t, meeting.New(meeting.DefaultLimits))
	a, b := start(t, url, "A"), start(t, url, "B")
	a.sees(t, "B")
	b.sees(t, "A")

	pb := a.peer(t, "B")
	if !pb.Is("fingerprint of B") || pb.Is("fingerprint of A") || pb.Is("") {
		t.Error("A should recognise B by B's fingerprint and by no other")
	}

	sa, err := a.Pair(within(t), pb.ID)
	if err != nil {
		t.Fatal(err)
	}
	sb := b.request(t)
	if len(sa.Code) != 6 || sa.Code != sb.Code {
		t.Fatalf("codes %q and %q, want the same six digits", sa.Code, sb.Code)
	}
	if sa.Asked || !sb.Asked || sb.Peer.Name != "A" || sa.Peer.Name != "B" {
		t.Errorf("sessions: A's %+v, B's %+v", sa, sb)
	}

	const link = "https://yonder.example/app/#pair=tcSECRET"
	if err := sa.SendLink(link); err != nil {
		t.Fatal(err)
	}
	got, err := sb.Link(within(t))
	if err != nil || got != link {
		t.Fatalf("B got %q, %v", got, err)
	}
	sb.Finish()
	sa.Finish()

	// Both are free to pair again, in the other direction too.
	sb2, err := b.Pair(within(t), b.peer(t, "A").ID)
	if err != nil {
		t.Fatal(err)
	}
	sa2 := a.request(t)
	if sb2.Code != sa2.Code {
		t.Fatalf("second pairing: codes %q and %q", sb2.Code, sa2.Code)
	}
}

// Saying no on either device ends the session on the other.
func TestRefuse(t *testing.T) {
	url := serve(t, meeting.New(meeting.DefaultLimits))
	a, b := start(t, url, "A"), start(t, url, "B")
	a.sees(t, "B")
	b.sees(t, "A")

	sa, err := a.Pair(within(t), a.peer(t, "B").ID)
	if err != nil {
		t.Fatal(err)
	}
	sb := b.request(t)
	sb.Close()
	if err := ended(t, sa); !errors.Is(err, nearby.ErrRefused) {
		t.Fatalf("asker's session ended with %v, want ErrRefused", err)
	}
	if err := sa.SendLink("link"); !errors.Is(err, nearby.ErrRefused) {
		t.Fatalf("sending a link after a no: %v, want ErrRefused", err)
	}

	sa, err = a.Pair(within(t), a.peer(t, "B").ID)
	if err != nil {
		t.Fatal(err)
	}
	sb = b.request(t)
	sa.Close()
	if err := ended(t, sb); !errors.Is(err, nearby.ErrRefused) {
		t.Fatalf("answerer's session ended with %v, want ErrRefused", err)
	}
	if _, err := sb.Link(within(t)); !errors.Is(err, nearby.ErrRefused) {
		t.Fatalf("waiting for a link after a no: %v, want ErrRefused", err)
	}
}

// A device in the middle of one pairing refuses a second, and the first
// carries on.
func TestBusy(t *testing.T) {
	url := serve(t, meeting.New(meeting.DefaultLimits))
	a, b, c := start(t, url, "A"), start(t, url, "B"), start(t, url, "C")
	a.sees(t, "B", "C")
	b.sees(t, "A", "C")
	c.sees(t, "A", "B")

	sa, err := a.Pair(within(t), a.peer(t, "B").ID)
	if err != nil {
		t.Fatal(err)
	}
	sb := b.request(t)
	if _, err := c.Pair(within(t), c.peer(t, "B").ID); !errors.Is(err, nearby.ErrRefused) {
		t.Fatalf("C asking a busy B: %v, want ErrRefused", err)
	}
	if _, err := a.Pair(within(t), a.peer(t, "C").ID); !errors.Is(err, nearby.ErrBusy) {
		t.Fatalf("A asking C while pairing with B: %v, want ErrBusy", err)
	}
	if err := sa.SendLink("link"); err != nil {
		t.Fatal(err)
	}
	if got, err := sb.Link(within(t)); err != nil || got != "link" {
		t.Fatalf("B got %q, %v", got, err)
	}
}

// When the other device goes, the session ends and the device leaves the
// list.
func TestGone(t *testing.T) {
	url := serve(t, meeting.New(meeting.DefaultLimits))
	a := start(t, url, "A")
	b := &nearby.Client{URL: url, Name: "B", Kind: nearby.KindApp, Fingerprint: "fingerprint of B"}
	b.Asked = func(*nearby.Session) {}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	a.sees(t, "B")

	sa, err := a.Pair(within(t), a.peer(t, "B").ID)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := ended(t, sa); !errors.Is(err, nearby.ErrGone) {
		t.Fatalf("session ended with %v, want ErrGone", err)
	}
	a.sees(t)
}

// A device that loses the meeting point forgets who was nearby, ends its
// pairing, and finds everyone again when the meeting point is back.
func TestReconnect(t *testing.T) {
	point := meeting.New(meeting.DefaultLimits)
	var mu sync.Mutex
	var conns []func()
	down := false
	url := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if down {
			mu.Unlock()
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		conns = append(conns, cancel)
		mu.Unlock()
		point.ServeHTTP(w, r.WithContext(ctx))
	}))
	a, b := start(t, url, "A"), start(t, url, "B")
	a.sees(t, "B")
	b.sees(t, "A")
	sa, err := a.Pair(within(t), a.peer(t, "B").ID)
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	down = true
	for _, cut := range conns {
		cut()
	}
	mu.Unlock()
	if err := ended(t, sa); err == nil {
		t.Fatal("the session survived losing the meeting point")
	}
	a.sees(t)
	b.sees(t)
	until(t, "A to notice it is offline", func() bool { return !a.Connected() })

	mu.Lock()
	down = false
	mu.Unlock()
	a.sees(t, "B")
	b.sees(t, "A")
}

// A meeting point that puts its own keys in place of the devices' ends up
// with a different code on each screen, which is what the owners catch.
func TestMeddlingKeys(t *testing.T) {
	m1, m2 := meddlerKey(t, "towards B"), meddlerKey(t, "towards A")
	h := &hub{meddle: func(from int, b wire) wire {
		switch b.Kind {
		case "ask": // A's commitment becomes a commitment to the meddler's key
			sum := sha256.Sum256(append([]byte("yonder nearby commitment v1"), m1.PublicKey().Bytes()...))
			b.Commit = sum[:]
		case "key": // B's key becomes the meddler's
			b.Pub = m2.PublicKey().Bytes()
		case "open": // and so does A's
			b.Pub = m1.PublicKey().Bytes()
		}
		return b
	}}
	url := serve(t, h)
	a, b := start(t, url, "A"), start(t, url, "B")
	a.sees(t, "B")
	b.sees(t, "A")

	sa, err := a.Pair(within(t), a.peer(t, "B").ID)
	if err != nil {
		t.Fatal(err)
	}
	sb := b.request(t)
	// The keys are fixed by the names above, so this is the same on every
	// run rather than true 999,999 times in a million.
	if sa.Code == sb.Code {
		t.Fatalf("both devices show %s with a meddler in the middle", sa.Code)
	}
}

// A meeting point that passes the commitment on but changes the key that
// is opened is caught by the device itself, before any code is shown.
func TestMeddlingOpen(t *testing.T) {
	m := meddlerKey(t, "swapped in")
	h := &hub{meddle: func(from int, b wire) wire {
		if b.Kind == "open" {
			b.Pub = m.PublicKey().Bytes()
		}
		return b
	}}
	url := serve(t, h)
	a, b := start(t, url, "A"), start(t, url, "B")
	a.sees(t, "B")
	b.sees(t, "A")

	sa, err := a.Pair(within(t), a.peer(t, "B").ID)
	if err != nil {
		t.Fatal(err)
	}
	// B tells A the pairing is off, and never shows its owner anything.
	if err := ended(t, sa); !errors.Is(err, nearby.ErrRefused) {
		t.Fatalf("A's session ended with %v, want ErrRefused", err)
	}
	if len(b.asked) != 0 {
		t.Fatal("B asked its owner about a pairing whose key did not fit the commitment")
	}
}

// The link crosses the meeting point unreadable, and one that was changed
// on the way is not accepted.
func TestLinkIsSealed(t *testing.T) {
	const link = "https://yonder.example/app/#pair=tcSECRET"
	var mu sync.Mutex
	var seen []byte
	h := &hub{meddle: func(from int, b wire) wire {
		raw, _ := json.Marshal(b)
		mu.Lock()
		seen = append(seen, raw...)
		seen = append(seen, b.Box...)
		mu.Unlock()
		if b.Kind == "link" {
			b.Box[len(b.Box)-1] ^= 1
		}
		return b
	}}
	url := serve(t, h)
	a, b := start(t, url, "A"), start(t, url, "B")
	a.sees(t, "B")
	b.sees(t, "A")

	sa, err := a.Pair(within(t), a.peer(t, "B").ID)
	if err != nil {
		t.Fatal(err)
	}
	sb := b.request(t)
	if sa.Code != sb.Code {
		t.Fatalf("codes %q and %q with nothing changed yet", sa.Code, sb.Code)
	}
	if err := sa.SendLink(link); err != nil {
		t.Fatal(err)
	}
	if got, err := sb.Link(within(t)); !errors.Is(err, nearby.ErrTampered) {
		t.Fatalf("B took a changed link: %q, %v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(string(seen), "SECRET") {
		t.Fatal("the meeting point saw the link")
	}
}

// A meeting point that is not honest cannot keep asking the same device
// with new keys of its own until the code it would share is the one it
// wants: after three attempts that end before the owner is asked, the
// device refuses every request for five minutes. Attempts that reach the
// owner do not count.
func TestTriesAreLimited(t *testing.T) {
	type conn struct {
		c    *websocket.Conn
		peer nearby.Peer
	}
	ready := make(chan conn, 1)
	url := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		var hello nearby.Message
		if readJSON(r.Context(), c, &hello) != nil {
			return
		}
		ready <- conn{c, *hello.Peer}
		<-r.Context().Done()
	}))
	b := start(t, url, "B")
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b.SetClock(func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	later := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }

	ctx := within(t)
	cb := <-ready
	writeJSON(ctx, cb.c, nearby.Message{Type: nearby.TypeWelcome, ID: "second", Peers: []nearby.Peer{{ID: "first", Name: "A"}}})
	b.sees(t, "A")

	to := func(m wire) {
		raw, _ := json.Marshal(m)
		writeJSON(ctx, cb.c, nearby.Message{Type: nearby.TypeFrom, ID: "first", Body: raw})
	}
	from := func() wire {
		for {
			var msg nearby.Message
			if err := readJSON(ctx, cb.c, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Type == nearby.TypeTo {
				var m wire
				json.Unmarshal(msg.Body, &m)
				return m
			}
		}
	}
	// ask sends a request with a new key and returns the key, and B's
	// answer.
	n := 0
	ask := func() (*ecdh.PrivateKey, wire) {
		n++
		m := meddlerKey(t, fmt.Sprint("try ", n))
		sum := sha256.Sum256(append([]byte("yonder nearby commitment v1"), m.PublicKey().Bytes()...))
		to(wire{Kind: "ask", Commit: sum[:]})
		return m, from()
	}

	// Attempts that reach the owner, who says no, do not count.
	for range 4 {
		m, got := ask()
		if got.Kind != "key" {
			t.Fatalf("B answered %q to a request, want its key", got.Kind)
		}
		to(wire{Kind: "open", Pub: m.PublicKey().Bytes()})
		b.request(t).Close()
		if got := from(); got.Kind != "no" {
			t.Fatalf("B's owner said no, B sent %q", got.Kind)
		}
	}

	// The grind: ask, see the key, call it off before the owner is asked.
	keys := 0
	for range 1000 {
		if _, got := ask(); got.Kind != "key" {
			break
		}
		keys++
		to(wire{Kind: "no"})
	}
	if keys != 3 {
		t.Fatalf("B gave its key to %d attempts that ended before its owner was asked, want 3", keys)
	}
	if len(b.asked) != 0 {
		t.Fatal("B's owner was asked during the grind")
	}

	// Still refused just before the five minutes are over; free after.
	later(5*time.Minute - time.Second)
	if _, got := ask(); got.Kind != "no" {
		t.Fatalf("B answered %q during the pause, want no", got.Kind)
	}
	later(time.Second)
	if _, got := ask(); got.Kind != "key" {
		t.Fatalf("B answered %q after the pause, want its key", got.Kind)
	}
}

func meddlerKey(t *testing.T, seed string) *ecdh.PrivateKey {
	t.Helper()
	sum := sha256.Sum256([]byte(seed))
	k, err := ecdh.X25519().NewPrivateKey(sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// wire is a pairing message as the meeting point sees it.
type wire struct {
	Kind   string `json:"k"`
	Commit []byte `json:"commit,omitempty"`
	Pub    []byte `json:"pub,omitempty"`
	Box    []byte `json:"box,omitempty"`
}

// hub poses as a meeting point for exactly two devices and may change
// every pairing message it passes between them.
type hub struct {
	meddle func(from int, b wire) wire

	mu    sync.Mutex
	conns []*websocket.Conn
	peers []nearby.Peer
	both  chan struct{}
}

func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	var hello nearby.Message
	if readJSON(ctx, c, &hello) != nil {
		return
	}
	h.mu.Lock()
	if h.both == nil {
		h.both = make(chan struct{})
	}
	me := len(h.conns)
	hello.Peer.ID = []string{"first", "second"}[me]
	h.conns = append(h.conns, c)
	h.peers = append(h.peers, *hello.Peer)
	if len(h.conns) == 2 {
		close(h.both)
	}
	h.mu.Unlock()
	select {
	case <-h.both:
	case <-ctx.Done():
		return
	}
	other := 1 - me
	writeJSON(ctx, c, nearby.Message{Type: nearby.TypeWelcome, ID: h.peers[me].ID, Peers: []nearby.Peer{h.peers[other]}})
	for {
		var msg nearby.Message
		if readJSON(ctx, c, &msg) != nil {
			return
		}
		if msg.Type != nearby.TypeTo {
			continue
		}
		var b wire
		json.Unmarshal(msg.Body, &b)
		raw, _ := json.Marshal(h.meddle(me, b))
		writeJSON(ctx, h.conns[other], nearby.Message{Type: nearby.TypeFrom, ID: h.peers[me].ID, Body: raw})
	}
}

func readJSON(ctx context.Context, c *websocket.Conn, v any) error {
	_, b, err := c.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	c.Write(ctx, websocket.MessageText, b)
}
