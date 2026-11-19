package stun

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"

	pion "github.com/pion/stun/v4"
)

// request is a Binding request with no attributes, as a browser sends.
func request(id byte) []byte {
	req := make([]byte, headerLen)
	binary.BigEndian.PutUint16(req[0:2], bindingRequest)
	binary.BigEndian.PutUint32(req[4:8], magicCookie)
	for i := 8; i < headerLen; i++ {
		req[i] = id
	}
	return req
}

// mapped reads the address out of a response with pion's code, which is the
// code the desktop app reads it with.
func mapped(t *testing.T, res []byte) netip.AddrPort {
	t.Helper()
	m := &pion.Message{Raw: res}
	if err := m.Decode(); err != nil {
		t.Fatalf("pion cannot read the response: %v", err)
	}
	if m.Type != pion.BindingSuccess {
		t.Fatalf("the response is a %s", m.Type)
	}
	var x pion.XORMappedAddress
	if err := x.GetFrom(m); err != nil {
		t.Fatalf("no address in the response: %v", err)
	}
	ip, _ := netip.AddrFromSlice(x.IP)
	return netip.AddrPortFrom(ip.Unmap(), uint16(x.Port))
}

func TestAnswerGivesTheSendersAddress(t *testing.T) {
	for _, from := range []string{"203.0.113.7:51315", "[2001:db8::1234]:40000"} {
		want := netip.MustParseAddrPort(from)
		req := request(7)
		res := Answer(req, want)
		if got := mapped(t, res); got != want {
			t.Errorf("from %s: the response says %s", want, got)
		}
		if !bytes.Equal(res[4:headerLen], req[4:headerLen]) {
			t.Errorf("from %s: the response has another transaction ID", want)
		}
	}
}

func TestAnswerUnmapsAnIPv4Sender(t *testing.T) {
	res := Answer(request(1), netip.MustParseAddrPort("[::ffff:203.0.113.7]:9000"))
	if got, want := mapped(t, res), netip.MustParseAddrPort("203.0.113.7:9000"); got != want {
		t.Errorf("the response says %s, want %s", got, want)
	}
	if len(res) != 32 {
		t.Errorf("the response is %d bytes, want 32", len(res))
	}
}

// A request from pion and one from Tailscale's code both carry attributes.
func TestAnswerTakesARequestWithAttributes(t *testing.T) {
	m, err := pion.Build(pion.TransactionID, pion.BindingRequest, pion.NewSoftware("tailnode"), pion.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	want := netip.MustParseAddrPort("198.51.100.2:1234")
	if got := mapped(t, Answer(m.Raw, want)); got != want {
		t.Errorf("the response says %s, want %s", got, want)
	}
}

func TestAnswerRefusesWhatIsNoBindingRequest(t *testing.T) {
	from := netip.MustParseAddrPort("203.0.113.7:51315")
	change := func(f func(b []byte) []byte) []byte { return f(request(3)) }
	for name, req := range map[string][]byte{
		"empty":          nil,
		"short":          request(3)[:19],
		"a response":     change(func(b []byte) []byte { b[0], b[1] = 0x01, 0x01; return b }),
		"an indication":  change(func(b []byte) []byte { b[1] = 0x11; return b }),
		"no cookie":      change(func(b []byte) []byte { b[4] = 0; return b }),
		"a false length": change(func(b []byte) []byte { b[3] = 8; return b }),
		"an odd length":  append(change(func(b []byte) []byte { b[3] = 3; return b }), 0, 0, 0),
		"bytes after it": append(request(3), 0, 0, 0, 0),
		"longer than most": change(func(b []byte) []byte {
			binary.BigEndian.PutUint16(b[2:4], requestMost)
			return append(b, make([]byte, requestMost)...)
		}),
	} {
		if res := Answer(req, from); res != nil {
			t.Errorf("%s: answered with %d bytes", name, len(res))
		}
	}
}

// apart returns n addresses that have n places in l's table. Which
// addresses share a place changes with each start, so a test that needs
// sources with limits of their own asks for them here.
func apart(l *limiter, n int) []netip.Addr {
	var out []netip.Addr
	taken := map[int]bool{}
	for i := 0; len(out) < n; i++ {
		a := netip.AddrFrom4([4]byte{203, 0, byte(i >> 8), byte(i)})
		if !taken[l.place(a)] {
			taken[l.place(a)] = true
			out = append(out, a)
		}
	}
	return out
}

func TestLimitHoldsForOneSourceAndEndsWithTheWindow(t *testing.T) {
	l := newLimiter()
	l.per = 3
	now := time.Unix(1000, 0)
	two := apart(l, 2)
	a, b := two[0], two[1]
	for i := range 3 {
		if !l.allow(a, now) {
			t.Fatalf("request %d of 3 was refused", i+1)
		}
	}
	if l.allow(a, now) {
		t.Error("the fourth request was answered")
	}
	if !l.allow(b, now) {
		t.Error("another source was refused")
	}
	if !l.allow(a, now.Add(l.window)) {
		t.Error("refused after the window ended")
	}
	if n := l.counts[l.place(b)]; n != 0 {
		t.Errorf("a count of %d is held after the window ended, want 0", n)
	}
}

func TestLimitCountsAnIPv6NetworkAsOneSource(t *testing.T) {
	l := newLimiter()
	l.per = 1
	now := time.Unix(1000, 0)
	if !l.allow(netip.MustParseAddr("2001:db8:1:2::1"), now) {
		t.Fatal("the first request was refused")
	}
	if l.allow(netip.MustParseAddr("2001:db8:1:2:ffff::9"), now) {
		t.Error("a second address of the same /64 was answered")
	}
	if a, b := netip.MustParseAddr("2001:db8:1:2::1"), netip.MustParseAddr("2001:db8:1:2:ffff::9"); l.place(a) != l.place(b) {
		t.Error("two addresses of one /64 have two places")
	}
	if a, b := source(netip.MustParseAddr("2001:db8:1:2::1")), source(netip.MustParseAddr("2001:db8:1:3::1")); a == b {
		t.Error("two /64s are one source")
	}
}

// Many false senders do not get a true one refused: the table cannot fill
// up. Four times as many sources as the old list held each ask once, and a
// source that comes after them is still answered.
func TestLimitDoesNotRefuseNewSourcesUnderAFlood(t *testing.T) {
	l := newLimiter()
	now := time.Unix(1000, 0)
	for i := range 200_000 {
		l.allow(netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}), now)
	}
	refused := 0
	for i := range 1000 {
		if !l.allow(netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)}), now) {
			refused++
		}
	}
	if refused != 0 {
		t.Errorf("%d of 1000 new sources were refused after 200,000 others had asked once", refused)
	}
}

// patience is the one limit of every wait in these tests. It is long, so
// that a loaded machine does not fail a test; a wait that works returns
// far sooner.
const patience = 60 * time.Second

// until waits for cond to hold.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(patience); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// serve starts a Server on a loopback port and returns it with its address.
func serve(t *testing.T) (*Server, *net.UDPAddr) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer()
	done := make(chan struct{})
	go func() { s.Serve(conn); close(done) }()
	t.Cleanup(func() { conn.Close(); <-done })
	return s, conn.LocalAddr().(*net.UDPAddr)
}

// pion's client, which the desktop app's library is built on, gets its own
// address from the server.
func TestServerAnswersPionsClient(t *testing.T) {
	s, addr := serve(t)
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := pion.NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var got netip.AddrPort
	var failed error
	err = c.Do(pion.MustBuild(pion.TransactionID, pion.BindingRequest), func(e pion.Event) {
		if failed = e.Error; failed == nil {
			got = mapped(t, e.Message.Raw)
		}
	})
	if err != nil || failed != nil {
		t.Fatalf("no answer: %v, %v", err, failed)
	}
	if want := conn.LocalAddr().(*net.UDPAddr).AddrPort(); got != want {
		t.Errorf("the server says %s, want %s", got, want)
	}
	if answered, refused := s.Counts(); answered != 1 || refused != 0 {
		t.Errorf("counts: %d answered, %d not; want 1 and 0", answered, refused)
	}
}

// Over the limit the server sends nothing, and answers again in the next
// window. The limiter's own clock is tested above; here the limit is one
// request, and the window is ended by hand.
func TestServerSendsNothingOverTheLimit(t *testing.T) {
	s, addr := serve(t)
	s.limit.mu.Lock()
	s.limit.per = 1
	s.limit.mu.Unlock()
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ask := func(id byte) {
		t.Helper()
		if _, err := conn.Write(request(id)); err != nil {
			t.Fatal(err)
		}
	}
	read := func() byte {
		t.Helper()
		buf := make([]byte, 100)
		conn.SetReadDeadline(time.Now().Add(patience))
		n, err := conn.Read(buf)
		if err != nil || n < headerLen {
			t.Fatalf("no answer: %v", err)
		}
		return buf[8]
	}
	ask(1)
	if id := read(); id != 1 {
		t.Fatalf("the answer is to request %d, want 1", id)
	}
	// The server reads in order, so once it has counted request 2 as not
	// answered, the next answer on the socket can only be to request 3.
	ask(2)
	until(t, "request 2 to be refused", func() bool { _, refused := s.Counts(); return refused == 1 })
	s.limit.mu.Lock()
	s.limit.ends = time.Time{} // the window ends
	s.limit.mu.Unlock()
	ask(3)
	if id := read(); id != 3 {
		t.Errorf("the answer is to request %d, want 3", id)
	}
}
