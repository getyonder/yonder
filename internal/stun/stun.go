// Package stun is Yonder's STUN server: it tells a device the public address
// its request came from (RFC 8489, a Binding request and its success
// response, over UDP), and nothing else. A browser and the desktop app ask it
// before a data channel (internal/direct). The relay's own STUN cannot do
// this: it answers only a request from Tailscale's code.
//
// It keeps no record. For the limit it holds the addresses of the last few
// seconds in memory, and it counts how many requests it answered and how
// many it did not, with no address.
package stun

import (
	"encoding/binary"
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
	perSource  = 60
	window     = 10 * time.Second
	sourceMost = 50000
)

// limiter counts the requests of each source in the present window. When
// the window ends, all counts are dropped: no address is held for longer.
type limiter struct {
	per, most int
	window    time.Duration

	mu     sync.Mutex
	counts map[netip.Addr]int
	ends   time.Time
}

func newLimiter() *limiter {
	return &limiter{per: perSource, most: sourceMost, window: window, counts: map[netip.Addr]int{}}
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

// allow reports whether a request from ip at the time now gets an answer.
// With the table full, a source not in it gets none until the window ends.
func (l *limiter) allow(ip netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !now.Before(l.ends) {
		clear(l.counts)
		l.ends = now.Add(l.window)
	}
	s := source(ip)
	n, known := l.counts[s]
	if n >= l.per || (!known && len(l.counts) >= l.most) {
		return false
	}
	l.counts[s] = n + 1
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
