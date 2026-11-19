// Package stun is Yonder's STUN server: it tells a device the public address
// its request came from (RFC 8489, a Binding request and its success
// response, over UDP), and nothing else. A browser and the desktop app ask it
// before a data channel (internal/direct). The relay's own STUN cannot do
// this: it answers only a request from Tailscale's code.
//
// It keeps no record and holds no address. For the limit it counts the
// requests of the last few seconds in a table of fixed size, at a place
// found from the address, and it counts how many requests it answered and
// how many it did not.
package stun

import (
	"encoding/binary"
	"hash/maphash"
	"net"
	"net/netip"
	"sync"
	"time"
)

const (
	headerLen      = 20
	bindingRequest = 0x0001
	bindingSuccess = 0x0101
	magicCookie    = 0x2112A442
	xorMappedAddr  = 0x0020

	// A request has no need to be longer than this (a browser's is 20 to
	// about 100 bytes); a longer packet is not answered.
	requestMost = 512
)

// Answer returns the response to the packet req, which came from the address
// from, or nil if req is not a Binding request. The attributes of a request
// are not read: none changes the answer. The response is 32 bytes to an IPv4
// address and 44 to an IPv6 one, so a 20-byte request with a false sender
// makes this server send at most 2.2 times what it got.
func Answer(req []byte, from netip.AddrPort) []byte {
	if len(req) < headerLen || len(req) > requestMost ||
		binary.BigEndian.Uint16(req[0:2]) != bindingRequest ||
		binary.BigEndian.Uint32(req[4:8]) != magicCookie {
		return nil
	}
	if n := int(binary.BigEndian.Uint16(req[2:4])); n%4 != 0 || n != len(req)-headerLen {
		return nil
	}
	// A socket for both families gives an IPv4 sender as ::ffff:a.b.c.d.
	ip := from.Addr().Unmap()
	if !ip.IsValid() {
		return nil
	}
	addr := ip.AsSlice()
	family := byte(0x01)
	if ip.Is6() {
		family = 0x02
	}
	res := make([]byte, headerLen+8+len(addr))
	binary.BigEndian.PutUint16(res[0:2], bindingSuccess)
	binary.BigEndian.PutUint16(res[2:4], uint16(4+4+len(addr)))
	copy(res[4:headerLen], req[4:headerLen]) // the cookie and the transaction ID
	a := res[headerLen:]
	binary.BigEndian.PutUint16(a[0:2], xorMappedAddr)
	binary.BigEndian.PutUint16(a[2:4], uint16(4+len(addr)))
	a[5] = family
	binary.BigEndian.PutUint16(a[6:8], from.Port()^(magicCookie>>16))
	// The address is XORed with the cookie and, for IPv6, the transaction
	// ID after it: the 16 bytes of the header from the cookie on.
	for i, b := range addr {
		a[8+i] = b ^ req[4+i]
	}
	return res
}

// The limit for one source: an IPv4 address, or the /64 of an IPv6 one. A
// device asks a few times for each of its network interfaces at a transfer,
// and many devices can share one public address, so the limit is far above
// one device's need and low enough that a false sender's address gets little.
const (
	perSource = 60
	window    = 10 * time.Second
	// places is how many counts the table has. It is a power of two.
	places = 1 << 18
)

// limiter counts the requests of each source in the present window. When
// the window ends, all counts are dropped.
//
// The table has a fixed size and each source has one place in it, found by
// a hash that is keyed anew at each start. So the table cannot fill up,
// whatever number of senders, true or false, there is: a list of sources
// with a largest size refused every new source once false senders had
// filled it, which 5,000 packets a second did. Here two sources can have
// the same place and then share one limit. Packets from false senders
// spread over all places, and other sources begin to be refused only when
// the places reach the limit: at more than a million packets a second.
type limiter struct {
	per    int // at most 255, the most a count can hold
	window time.Duration
	seed   maphash.Seed

	mu     sync.Mutex
	counts []uint8
	ends   time.Time
}

func newLimiter() *limiter {
	return &limiter{per: perSource, window: window, seed: maphash.MakeSeed(), counts: make([]uint8, places)}
}

// source is the address the limit counts for.
func source(ip netip.Addr) netip.Addr {
	ip = ip.Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.Addr()
	}
	return ip
}

// place is where the table counts for ip's source.
func (l *limiter) place(ip netip.Addr) int {
	b := source(ip).As16()
	return int(maphash.Bytes(l.seed, b[:]) % uint64(len(l.counts)))
}

// allow reports whether a request from ip at the time now gets an answer.
func (l *limiter) allow(ip netip.Addr, now time.Time) bool {
	i := l.place(ip)
	l.mu.Lock()
	defer l.mu.Unlock()
	if !now.Before(l.ends) {
		clear(l.counts)
		l.ends = now.Add(l.window)
	}
	if int(l.counts[i]) >= l.per {
		return false
	}
	l.counts[i]++
	return true
}

// Server answers Binding requests on one UDP socket.
type Server struct {
	limit *limiter

	mu                sync.Mutex
	answered, refused uint64
}

func NewServer() *Server { return &Server{limit: newLimiter()} }

// Serve answers on conn until conn is closed, and returns the error that
// ended it.
func (s *Server) Serve(conn *net.UDPConn) error {
	// One byte more than the longest request, so that a longer packet is
	// seen as such.
	buf := make([]byte, requestMost+1)
	for {
		n, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return err
		}
		res := Answer(buf[:n], from)
		ok := res != nil && s.limit.allow(from.Addr(), time.Now())
		if ok {
			_, err = conn.WriteToUDPAddrPort(res, from)
			ok = err == nil
		}
		s.mu.Lock()
		if ok {
			s.answered++
		} else {
			s.refused++
		}
		s.mu.Unlock()
	}
}

// Counts returns how many packets got an answer and how many did not (not a
// Binding request, or over the limit) since the last call.
func (s *Server) Counts() (answered, refused uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	answered, refused = s.answered, s.refused
	s.answered, s.refused = 0, 0
	return
}
