// Package direct connects two paired devices with a WebRTC data channel,
// and gives the rest of Yonder each channel as a net.Conn. It is the only
// package that imports pion, as tunnel is the only one that imports
// tailcat. In a browser (js/wasm) pion is a thin cover over the browser's
// own WebRTC, so the same package serves the web app.
//
// One end calls Offer and sends the description it gets to the other end,
// which calls Answer and sends its description back; the first end gives
// that to SetAnswer. Both then hold a Peer. Either end opens a connection
// with Dial and takes the other's with Accept, one data channel for each,
// as TCP has one connection for each. A Peer is a net.Listener, so one end
// serves an http.Server on it and the other uses Dial as its dial function.
//
// The descriptions must travel where only the paired device can read and
// write them (the tunnel): they carry the fingerprint that WebRTC checks
// the other end against. This package does not check who the other end
// is beyond that. The caller runs TLS with the pinned certificate inside
// each conn, as on every other path, and never leaves it out.
//
// A description is never logged here, and pion's own logging is off.
//
// The two builds differ in what the browser does not let a page set or
// see; pion.go and pion_js.go say what. The one difference a caller can
// meet: in a browser a conn cannot hold the other end back, so what
// arrives and is not yet read is kept in memory without a limit.
//
// Panics: every callback this package gives to pion recovers, and a panic
// there ends the Peer and its conns with an error. A panic inside pion's
// own goroutines (its parsing of packets, for example) cannot be caught
// from outside and ends the program. Nothing here changes that.
//
// What comes later attaches here: the request /api/yonder/v1/direct
// carries what Offer and Answer return, and internal/app/direct.go makes
// the choice between a Peer and the tunnel.
package direct

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// MaxDescription is the longest description that Answer and SetAnswer
// take, in bytes. One with every address of a busy computer is a few
// thousand.
const MaxDescription = 16 << 10

// DefaultWait is the time a connection gets to form when the context of
// Offer or Answer has no deadline.
const DefaultWait = 10 * time.Second

// maxWait bounds the time taken from a context, so that the settings made
// from it stay in range.
const maxWait = 2 * time.Minute

// backlog is how many connections of the other end may wait for Accept.
// More are closed at once.
const backlog = 16

// The labels of the data channels. The first channel only keeps the
// connection: a description needs one channel in it, and its opening is
// the sign that the connection formed.
const (
	holdLabel = "yonder-hold"
	connLabel = "yonder-conn"
)

var (
	// ErrTURN is given for a config with a TURN address. A TURN server
	// is a relay, and no file goes through a relay.
	ErrTURN = errors.New("direct: a TURN address is not allowed")
	// ErrDescription is given for a description that is too long, not a
	// description, or not of the kind expected.
	ErrDescription = errors.New("direct: the description is not valid")
	// ErrTimeout ends a Peer when no connection formed in its time.
	ErrTimeout = errors.New("direct: no connection formed in time")
	// ErrLost ends a Peer when its connection failed or the other end
	// closed it.
	ErrLost = errors.New("direct: the connection was lost")
)

// Config is how a Peer looks for a path.
type Config struct {
	// STUN lists the STUN servers, each as "stun:host:port". With none,
	// only the device's own addresses are offered, which is enough on one
	// network. There is no default.
	STUN []string
	// Fallback lists STUN servers that are asked only when a description
	// made with STUN has no address from a STUN server in it: STUN gave
	// no answer. The first Peer is then dropped and a second one made with
	// these servers, which costs the time the first one waited.
	Fallback []string
	// MDNS lets pion ask the local network for the ".local" names that
	// browsers give in place of their local addresses. Off, nothing from
	// the local network is parsed, but an app and a browser on one
	// network may find no path. It has no effect in a browser.
	MDNS bool

	// Loopback is for tests, of this package and of its callers: only the
	// loopback addresses are used. It has no effect in a browser.
	Loopback bool
}

// check refuses what a Config must not hold.
func (c Config) check() error {
	for _, s := range slices.Concat(c.STUN, c.Fallback) {
		scheme, _, _ := strings.Cut(strings.ToLower(s), ":")
		switch scheme {
		case "stun", "stuns":
		case "turn", "turns":
			return ErrTURN
		default:
			return fmt.Errorf("direct: %q is not a STUN address", s)
		}
	}
	return nil
}

func (c Config) servers() []webrtc.ICEServer {
	if len(c.STUN) == 0 {
		return nil
	}
	return []webrtc.ICEServer{{URLs: c.STUN}}
}

// Path is the kind of address at each end of the path a Peer chose:
// "host" (the device's own address), "srflx" (its public address, from
// STUN), "prflx" (a public address learned during the checks) or "relay".
type Path struct {
	Local, Remote string
}

// Peer is one end of a connection between two devices. It is a
// net.Listener for the connections the other end opens.
type Peer struct {
	pc    *webrtc.PeerConnection
	timer *time.Timer

	up       chan struct{} // closed when the connection formed
	upOnce   sync.Once
	done     chan struct{} // closed when the Peer ended
	incoming chan *conn

	mu      sync.Mutex
	err     error // why the Peer ended
	conns   map[*conn]struct{}
	next    int // numbers the conns
	pcClose sync.Once

	// fallback: see AskedFallback. Set before the Peer is given out.
	fallback bool
}

// Offer starts a connection. It returns the description to send to the
// other end, complete: there is no second message with more addresses.
//
// The deadline of ctx is the time for the whole connect: finding this
// end's addresses, the other end's answer, and the checks between the
// two. When it passes with no connection, the Peer ends with ErrTimeout.
// Cancelling ctx after Offer returned does nothing; Close does.
func Offer(ctx context.Context, cfg Config) (*Peer, []byte, error) {
	return start(ctx, cfg, func(p *Peer) (webrtc.SessionDescription, error) {
		hold, err := p.pc.CreateDataChannel(holdLabel, nil)
		if err != nil {
			return webrtc.SessionDescription{}, err
		}
		p.hold(hold)
		return p.pc.CreateOffer(nil)
	})
}

// Answer takes the description that Offer gave at the other end, and
// returns the one to send back. ctx is as for Offer.
func Answer(ctx context.Context, cfg Config, offer []byte) (*Peer, []byte, error) {
	remote, err := parse(offer, webrtc.SDPTypeOffer)
	if err != nil {
		return nil, nil, err
	}
	return start(ctx, cfg, func(p *Peer) (webrtc.SessionDescription, error) {
		if err := p.pc.SetRemoteDescription(remote); err != nil {
			return webrtc.SessionDescription{}, ErrDescription
		}
		return p.pc.CreateAnswer(nil)
	})
}

// start makes a Peer and its description, with the servers of cfg.STUN,
// and with those of cfg.Fallback if the first gave no address (see
// Config.Fallback). If the second try fails, the first one stands.
//
// A first Peer that has a path already stands too. An answering end starts
// its checks while it still asks STUN, and on one network they can find
// the other end first; a browser then stops asking, so its description
// has no address from STUN although STUN is well. The other end has met
// that Peer by then, and no channel forms with a second one in its place
// (the fault of 0.1.37 to 0.1.41 between two devices on one network).
func start(ctx context.Context, cfg Config, create func(p *Peer) (webrtc.SessionDescription, error)) (*Peer, []byte, error) {
	p, desc, err := describe(ctx, cfg, create)
	if err != nil || len(cfg.STUN) == 0 || len(cfg.Fallback) == 0 || fromSTUN(desc) || p.hasPath() {
		return p, desc, err
	}
	second := cfg
	second.STUN, second.Fallback = cfg.Fallback, nil
	p2, desc2, err := describe(ctx, second, create)
	if err != nil {
		return p, desc, nil
	}
	p.Close()
	p2.fallback = true
	return p2, desc2, nil
}

// hasPath reports whether the checks have found a path to the other end.
func (p *Peer) hasPath() bool {
	s := p.pc.ICEConnectionState()
	return s == webrtc.ICEConnectionStateConnected || s == webrtc.ICEConnectionStateCompleted
}

// fromSTUN reports whether a description holds an address that a STUN
// server gave.
func fromSTUN(desc []byte) bool {
	return bytes.Contains(desc, []byte(" typ srflx"))
}

// AskedFallback reports whether this Peer was made with the servers of
// Config.Fallback, because those of Config.STUN gave no address.
func (p *Peer) AskedFallback() bool { return p.fallback }

// SetAnswer gives the Peer that Offer made the description that Answer
// returned at the other end. The checks start here.
func (p *Peer) SetAnswer(answer []byte) error {
	remote, err := parse(answer, webrtc.SDPTypeAnswer)
	if err != nil {
		return err
	}
	if err := p.pc.SetRemoteDescription(remote); err != nil {
		// pion's error can quote the description, so it is not passed on.
		return ErrDescription
	}
	return nil
}

func newPeer(ctx context.Context, cfg Config) (*Peer, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := DefaultWait
	if t, ok := ctx.Deadline(); ok {
		limit = max(min(time.Until(t), maxWait), time.Millisecond)
	}
	pc, err := newPeerConnection(cfg, limit)
	if err != nil {
		return nil, err
	}
	p := &Peer{
		pc:       pc,
		up:       make(chan struct{}),
		done:     make(chan struct{}),
		incoming: make(chan *conn, backlog),
		conns:    map[*conn]struct{}{},
	}
	p.timer = time.AfterFunc(limit, func() {
		if !isClosed(p.up) {
			p.fail(ErrTimeout)
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		defer p.guard("the connection's state")
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			p.fail(ErrLost)
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		defer p.guard("a new channel")
		switch dc.Label() {
		case holdLabel:
			p.hold(dc)
		case connLabel:
			c := p.attach(dc, func(c *conn) {
				select {
				case p.incoming <- c:
				default:
					c.Close()
				}
			})
			if c == nil {
				dc.Close()
			}
		default:
			dc.Close()
		}
	})
	return p, nil
}

// describe makes a Peer and its description, as the bytes to send.
func describe(ctx context.Context, cfg Config, create func(p *Peer) (webrtc.SessionDescription, error)) (*Peer, []byte, error) {
	p, err := newPeer(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	desc, err := p.describe(ctx, cfg, create)
	if err != nil {
		p.Close()
		return nil, nil, err
	}
	return p, desc, nil
}

// describe makes this end's description with create, waits until its
// addresses are in it, and returns it as the bytes to send.
func (p *Peer) describe(ctx context.Context, cfg Config, create func(p *Peer) (webrtc.SessionDescription, error)) ([]byte, error) {
	desc, err := create(p)
	if err != nil {
		return nil, err
	}
	wait := gathering(p.pc, len(cfg.STUN) > 0)
	if err := p.pc.SetLocalDescription(desc); err != nil {
		return nil, err
	}
	if err := wait(ctx, p.done); err != nil {
		if perr := p.Err(); perr != nil {
			return nil, perr
		}
		return nil, err
	}
	local := p.pc.LocalDescription()
	if local == nil {
		return nil, errors.New("direct: no description was made")
	}
	return json.Marshal(local)
}

// parse reads a description from the other end. The error never holds
// any of the description.
func parse(b []byte, want webrtc.SDPType) (webrtc.SessionDescription, error) {
	var d webrtc.SessionDescription
	if len(b) > MaxDescription {
		return d, fmt.Errorf("%w: longer than %d bytes", ErrDescription, MaxDescription)
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, fmt.Errorf("%w: not a description", ErrDescription)
	}
	if d.Type != want {
		return d, fmt.Errorf("%w: not an %s", ErrDescription, want)
	}
	// The same check in both builds: a browser would be more forgiving.
	if !strings.HasPrefix(d.SDP, "v=0") || !strings.Contains(d.SDP, "a=fingerprint:") {
		return d, fmt.Errorf("%w: not a description", ErrDescription)
	}
	return d, nil
}

// guard goes, deferred, at the top of every callback given to pion: a
// panic there becomes the error of the Peer and of all its conns.
func (p *Peer) guard(where string) {
	if r := recover(); r != nil {
		p.fail(fmt.Errorf("direct: panic in %s: %v", where, r))
	}
}

// fail ends the Peer, once. It can run inside one of pion's callbacks, so
// it does not wait for pion to close.
func (p *Peer) fail(err error) {
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return
	}
	p.err = err
	conns := p.conns
	p.conns = nil
	close(p.done)
	p.mu.Unlock()

	p.timer.Stop()
	for c := range conns {
		c.fail(err)
	}
	go p.closeConnection()
}

func (p *Peer) closeConnection() { p.pcClose.Do(func() { p.pc.Close() }) }

// hold watches the channel that keeps the connection.
func (p *Peer) hold(dc *webrtc.DataChannel) {
	open := func() {
		defer p.guard("the first channel's opening")
		p.upOnce.Do(func() { close(p.up) })
	}
	dc.OnOpen(open)
	dc.OnClose(func() {
		defer p.guard("the first channel's closing")
		p.fail(ErrLost)
	})
	if dc.ReadyState() == webrtc.DataChannelStateOpen {
		open()
	}
}

// attach makes the conn of a data channel and ties the channel's events
// to it. opened, if set, is called once when the channel is open. It
// returns nil when the Peer has ended.
func (p *Peer) attach(dc *webrtc.DataChannel, opened func(*conn)) *conn {
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return nil
	}
	p.next++
	c := newConn(dc, p.next)
	p.conns[c] = struct{}{}
	p.mu.Unlock()
	c.forget = func() {
		p.mu.Lock()
		delete(p.conns, c)
		p.mu.Unlock()
	}

	prepare(dc)
	open := func() {
		defer p.guard("a channel's opening")
		first := false
		c.openOnce.Do(func() { close(c.opened); first = true })
		if first && opened != nil {
			opened(c)
		}
	}
	dc.SetBufferedAmountLowThreshold(lowWater)
	dc.OnBufferedAmountLow(func() {
		defer p.guard("a channel's send buffer")
		c.low()
	})
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		defer p.guard("a channel's message")
		c.receive(m.Data, m.IsString)
	})
	dc.OnError(func(err error) {
		defer p.guard("a channel's error")
		c.fail(fmt.Errorf("direct: the channel failed: %w", err))
	})
	dc.OnClose(func() {
		defer p.guard("a channel's closing")
		c.closedByPeer()
	})
	dc.OnOpen(open)
	if dc.ReadyState() == webrtc.DataChannelStateOpen {
		open()
	}
	return c
}

// Connected waits until the connection formed. It returns the Peer's
// error when it ended first, and ctx's when ctx did.
func (p *Peer) Connected(ctx context.Context) error {
	if err := p.Err(); err != nil {
		return err
	}
	select {
	case <-p.up:
		return nil
	case <-p.done:
		return p.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Dial opens a connection to the other end, which gets it from Accept. It
// waits for the Peer to connect first. To use it where a dial function
// with a network and an address is asked for, leave those out:
//
//	Dial: func(ctx context.Context, _, _ string) (net.Conn, error) { return p.Dial(ctx) }
func (p *Peer) Dial(ctx context.Context) (net.Conn, error) {
	if err := p.Connected(ctx); err != nil {
		return nil, err
	}
	dc, err := p.pc.CreateDataChannel(connLabel, nil)
	if err != nil {
		if perr := p.Err(); perr != nil {
			return nil, perr
		}
		return nil, err
	}
	c := p.attach(dc, nil)
	if c == nil {
		return nil, p.Err()
	}
	select {
	case <-c.opened:
		return c, nil
	case <-p.done:
		return nil, p.Err()
	case <-c.ended:
		c.Close()
		return nil, ErrLost
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	}
}

// Accept waits for the next connection that the other end opened with
// Dial. Once the Peer has ended it returns the Peer's error.
func (p *Peer) Accept() (net.Conn, error) {
	if err := p.Err(); err != nil {
		return nil, err
	}
	select {
	case c := <-p.incoming:
		return c, nil
	case <-p.done:
		return nil, p.Err()
	}
}

// Addr is there for net.Listener.
func (p *Peer) Addr() net.Addr { return addr{"local", 0} }

// Close ends the Peer and every conn of it, at both ends.
func (p *Peer) Close() error {
	p.fail(net.ErrClosed)
	p.closeConnection()
	return nil
}

// Done is closed when the Peer has ended; Err then says why: ErrTimeout,
// ErrLost, net.ErrClosed after Close, or a panic in a callback.
func (p *Peer) Done() <-chan struct{} { return p.done }

// Err is nil while the Peer lives.
func (p *Peer) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Path says which kinds of address the connection runs between, for the
// trace. ok is false before the connection formed, and where a browser
// does not tell.
func (p *Peer) Path() (path Path, ok bool) {
	defer func() {
		if recover() != nil { // a browser without this part of WebRTC
			path, ok = Path{}, false
		}
	}()
	sctp := p.pc.SCTP()
	if sctp == nil || sctp.Transport() == nil || sctp.Transport().ICETransport() == nil {
		return Path{}, false
	}
	pair, err := sctp.Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return Path{}, false
	}
	return Path{Local: pair.Local.Typ.String(), Remote: pair.Remote.Typ.String()}, true
}
