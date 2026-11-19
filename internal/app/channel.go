package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"yonder/internal/direct"
	"yonder/internal/identity"
	"yonder/internal/localsend"
	"yonder/internal/pairing"
)

// This file makes and takes the data channels of transfers with a browser
// at one end (way B): a WebRTC connection straight between the two devices,
// in place of the relay. The choice between a channel and the relay is
// sendWeb's, in direct.go; the WebRTC itself is the direct package's.
//
// The device that sends the files makes the offer: it is the one that then
// dials, and the other serves what arrives as it serves its tunnel. The
// two descriptions travel only in the request directPath, which is
// answered only through the tunnel, so only to a paired device. Inside a
// channel the same TLS and the same LocalSend requests run as everywhere
// else, and both ends must show the certificate they were paired with: the
// sender checks the receiver's as it always does (localsend.Sender), and
// the receiver checks the sender's here, which on a tunnel connection the
// tunnel has done.

// directPath is the request that carries the two descriptions of a data
// channel: the sender's as its body, the receiver's as the answer. A Yonder
// from before it answers 404.
const directPath = "/api/yonder/v1/direct"

// stunServer is the one STUN server, which tells a device its public
// address. It is Yonder's own (cmd/yonder-stun, beside the relay; the
// relay's STUN on port 3478 answers only Tailscale's code), so no other
// company learns the address of a device that asks. The app and the web app
// both get it from here. There is no TURN server, and there must be none:
// that would be a relay.
const stunServer = "stun:stun.getyonder.org:3479"

// stunFallback is asked only when stunServer gave no address
// (direct.Config.Fallback), so that a fault of Yonder's server does not
// stop transfers between two networks. It is Cloudflare's, and so learns
// the address of a device that asks it; the privacy page says so.
const stunFallback = "stun:stun.cloudflare.com:3478"

// The limits on directPath for one paired device. A send asks once, so
// these are far above what a person does, and low enough that a device
// cannot make this one open sockets without end.
const (
	// channelsMost is how many data channel connections, forming or open,
	// one paired device may have here at a time.
	channelsMost = 4
	// channelAsks is how many times it may ask in channelAsksIn.
	channelAsks   = 20
	channelAsksIn = time.Minute
)

// channelWait is how long a data channel gets to form. It is long, because
// nothing comes after it: a phone on mobile data that is busy with another
// transfer has taken more than 10 seconds.
const channelWait = 30 * time.Second

// relayFallBack is whether a transfer with a browser goes through the relay
// when no data channel forms. It does not (from 0.1.34): the transfer is
// refused, and no file passes through Yonder's server. The path through
// the relay is kept, and tested, for the day it has a use again.
const relayFallBack = false

// ErrNoChannel means a transfer with a browser was not made, because no
// data channel formed and files do not go through the relay.
var ErrNoChannel = errors.New("the two devices could not connect directly, and Yonder does not pass files through its server")

// ErrOldDevice means the same, because the other device's Yonder is from
// before data channels.
var ErrOldDevice = errors.New("the other device has an older Yonder, which cannot connect directly")

// relayedHeader goes with an offer of files that travels through the relay
// although one end is a browser, and says why no data channel carries it:
// one of the words below. The receiver only writes it in its send log, so
// that the log of an app says it for a browser, which keeps no log.
const relayedHeader = "Yonder-Relayed"

// Why a transfer with a browser went through the relay.
const (
	whyTime  = "time"  // no data channel formed within the wait
	whyOld   = "old"   // the other device's Yonder is from before data channels
	whyError = "error" // making the channel, or asking for it, failed
)

// whyText is each of those as the send log says it.
var whyText = map[string]string{
	whyTime:  "no data channel formed in time",
	whyOld:   "the other device's Yonder is older and has no data channels",
	whyError: "the data channel could not be asked for or made",
}

// why sorts an error of making a channel.
func why(err error) string {
	if errors.Is(err, direct.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return whyTime
	}
	return whyError
}

// Way is how the files of a transfer travel, for whoever shows it.
type Way int

const (
	// WayOther is the local network, or the tunnel between two apps:
	// nothing that this file changed.
	WayOther Way = iota
	// WayChannel is a data channel, straight between the two devices.
	WayChannel
	// WayRelay is the tunnel through the relay, which only a transfer with
	// a browser uses.
	WayRelay
	// WayConnecting is not a way yet: a data channel is being made, which
	// can take the whole of channelWait. WayChannel or WayRelay follows,
	// or the send ends.
	WayConnecting
)

type wayKey struct{}

// WithWay returns a ctx for SendPaired that calls told when it is known how
// the files will travel, and before that with WayConnecting if a data
// channel is tried first.
func WithWay(ctx context.Context, told func(Way)) context.Context {
	return context.WithValue(ctx, wayKey{}, told)
}

func tellWay(ctx context.Context, way Way) {
	if told, ok := ctx.Value(wayKey{}).(func(Way)); ok {
		told(way)
	}
}

// channelRule is how data channels are made; tests replace its parts.
type channelRule struct {
	cfg direct.Config
	// wait is the time a channel gets to form, from the first step to the
	// last, before the transfer is given up.
	wait time.Duration
	// relay is whether the transfer then goes through the relay instead:
	// see relayFallBack.
	relay bool
	// The limits for one paired device: see channelsMost and channelAsks.
	most, asks int
	asksIn     time.Duration
	// answer is direct.Answer, and serverTLS the Node's channelTLS.
	answer    func(ctx context.Context, cfg direct.Config, offer []byte) (*direct.Peer, []byte, error)
	serverTLS func(d pairing.Device) *tls.Config
}

func (n *Node) newChannelRule() channelRule {
	return channelRule{
		cfg:  direct.Config{STUN: []string{stunServer}, Fallback: []string{stunFallback}, MDNS: true},
		wait: channelWait, relay: relayFallBack,
		most: channelsMost, asks: channelAsks, asksIn: channelAsksIn,
		answer: direct.Answer, serverTLS: n.channelTLS,
	}
}

// channelState is the data channels there are; all by the paired device's
// fingerprint but peers.
type channelState struct {
	peers     map[*direct.Peer]string // the connections this device answered, each with its device
	answering map[string]int          // how many of those a device has here
	asked     map[string][]time.Time  // when it asked for one, within asksIn
	sending   map[string]int          // sends to it that are going over a channel
}

func newChannelState() channelState {
	return channelState{peers: map[*direct.Peer]string{}, answering: map[string]int{}, asked: map[string][]time.Time{}, sending: map[string]int{}}
}

// channelPeerKey holds, in a request's ctx, the fingerprint of the paired
// device at the other end of the data channel the request came in on.
type channelPeerKey struct{}

// channelListener marks each connection it accepts as come through a data
// channel with one paired device, as tunnelListener does for the tunnel.
type channelListener struct {
	net.Listener
	fingerprint string
}

type channelConn struct {
	net.Conn
	fingerprint string
}

func (l channelListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return channelConn{c, l.fingerprint}, nil
}

// channelSender says which paired device a request came from over a data
// channel. channel is false for every other request.
func (n *Node) channelSender(ctx context.Context) (d pairing.Device, channel, ok bool) {
	fingerprint, channel := ctx.Value(channelPeerKey{}).(string)
	if !channel {
		return pairing.Device{}, false, false
	}
	// Asked for each request: a device unpaired since is not one.
	d, ok = n.paired(fingerprint)
	return d, true, ok
}

// channelOK reports whether a data channel may be made with a paired
// device: not between two apps, which stay as they were. A device that has
// not yet said what it is gets one; its offer of files then says it, and
// AdmitOffer holds it to the same rule.
func (n *Node) channelOK(d pairing.Device) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	web, known := n.web[d.Fingerprint]
	return web || !known || mayRelay(n.Info.DeviceType, "")
}

// openChannel makes a data channel to a paired device that sender reaches
// through the tunnel. If none forms within the wait it returns why, as one
// of the words for relayedHeader, and the error for the send log.
func (n *Node) openChannel(ctx context.Context, sender *localsend.Sender, to localsend.Peer) (*direct.Peer, string, error) {
	ctx, cancel := context.WithTimeout(ctx, n.dc.wait)
	defer cancel()
	p, offer, err := direct.Offer(ctx, n.dc.cfg)
	if err != nil {
		return nil, why(err), err
	}
	status, answer, err := sender.Post(ctx, to, directPath, offer)
	old := err == nil && status == http.StatusNotFound
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("it answered HTTP %d", status)
	}
	if err == nil {
		err = p.SetAnswer(answer)
	}
	if err == nil {
		err = p.Connected(ctx)
	}
	switch {
	case err == nil:
		return p, "", nil
	case old:
		p.Close()
		return nil, whyOld, err
	}
	p.Close()
	return nil, why(err), err
}

// pathText is a Peer's path as the send log shows it.
func pathText(p *direct.Peer) string {
	text := "addresses not known"
	if path, ok := p.Path(); ok {
		text = path.Local + " to " + path.Remote
	}
	if p.AskedFallback() {
		text += "; this end asked the fallback STUN server"
	}
	return text
}

// admitChannel applies the limits to one more request from a paired
// device. If it is admitted, release must be called when the connection
// that follows has ended.
func (n *Node) admitChannel(fingerprint string) (release func(), ok bool) {
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	asked := n.chans.asked[fingerprint]
	for len(asked) > 0 && now.Sub(asked[0]) >= n.dc.asksIn {
		asked = asked[1:]
	}
	n.chans.asked[fingerprint] = asked
	if len(asked) >= n.dc.asks || n.chans.answering[fingerprint] >= n.dc.most {
		return nil, false
	}
	n.chans.asked[fingerprint] = append(asked, now)
	n.chans.answering[fingerprint]++
	return func() {
		n.mu.Lock()
		n.chans.answering[fingerprint]--
		n.mu.Unlock()
	}, true
}

// answerDirect answers directPath: it takes the sender's description, makes
// this end of the data channel, and answers with its own. Only a paired
// device asking through the tunnel is answered; anyone else, a request that
// came over a data channel included, gets what a Yonder from before gives.
func (n *Node) answerDirect(w http.ResponseWriter, r *http.Request) {
	d, ok := n.tunnelSender(r.Context())
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	name := localsend.Clip(d.Name)
	if !n.channelOK(d) {
		n.log.logf("direct: %q asked for a data channel, refused: both devices are apps", name)
		http.Error(w, "data channels are for transfers with a browser", http.StatusForbidden)
		return
	}
	release, ok := n.admitChannel(d.Fingerprint)
	if !ok {
		n.log.logf("direct: %q asked for a data channel, refused: too many", name)
		http.Error(w, "too many data channels asked for", http.StatusTooManyRequests)
		return
	}
	offer, err := io.ReadAll(http.MaxBytesReader(w, r.Body, direct.MaxDescription))
	if err != nil {
		release()
		n.log.logf("direct: %q asked for a data channel, refused: its description is too long", name)
		http.Error(w, "the description is too long", http.StatusRequestEntityTooLarge)
		return
	}
	// Not the request's ctx alone: the connection outlives the request.
	ctx, cancel := context.WithTimeout(n.ctx, n.dc.wait)
	defer cancel()
	defer context.AfterFunc(r.Context(), cancel)()
	p, answer, err := n.dc.answer(ctx, n.dc.cfg, offer)
	if err != nil {
		release()
		n.log.logf("direct: %q asked for a data channel, not made: %v", name, err)
		status := http.StatusInternalServerError
		if errors.Is(err, direct.ErrDescription) {
			status = http.StatusBadRequest
		}
		http.Error(w, "no data channel", status)
		return
	}
	n.mu.Lock()
	n.chans.peers[p] = d.Fingerprint
	closed := n.ctx.Err() != nil
	n.mu.Unlock()
	if closed {
		p.Close()
	}
	go n.serveChannel(p, d, release)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(answer)
}

// serveChannel waits for a data channel to form with a paired device that
// asked for it, and then answers the LocalSend requests that arrive on it
// until it ends, which is when the other device closes it after its send.
func (n *Node) serveChannel(p *direct.Peer, d pairing.Device, release func()) {
	defer func() {
		p.Close()
		n.mu.Lock()
		delete(n.chans.peers, p)
		n.mu.Unlock()
		release()
	}()
	name := localsend.Clip(d.Name)
	start := time.Now()
	if err := p.Connected(n.ctx); err != nil {
		n.log.logf("direct: no data channel with %q after %v: %v", name, since(start), err)
		return
	}
	n.log.logf("direct: data channel with %q open after %v (%s)", name, since(start), pathText(p))
	n.srv.Serve(tls.NewListener(channelListener{p, d.Fingerprint}, n.dc.serverTLS(d)))
	n.log.logf("direct: data channel with %q closed after %v", name, since(start))
}

// channelTLS is the TLS of the server side of a data channel with a paired
// device: this device's certificate as on every path, and the other
// device must show the certificate it was paired with. That the channel was
// set up through the tunnel is not taken as proof by itself.
func (n *Node) channelTLS(d pairing.Device) *tls.Config {
	conf := n.tlsCfg.Clone()
	conf.ClientAuth = tls.RequireAnyClientCert
	conf.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 || !strings.EqualFold(identity.Fingerprint(cs.PeerCertificates[0].Raw), d.Fingerprint) {
			return errors.New("the device's certificate is not the paired device's")
		}
		return nil
	}
	return conf
}

// closeChannels ends the data channels this device answered: all of them,
// or with fingerprint only that device's. n.mu must be held.
func (n *Node) closeChannels(fingerprint string) {
	for p, fp := range n.chans.peers {
		if fingerprint == "" || fp == fingerprint {
			go p.Close()
		}
	}
}

// OfferWay says how the files of an offer will arrive, given the ctx passed
// to Receiver.Accept, once AdmitOffer has admitted it.
func (n *Node) OfferWay(ctx context.Context, o localsend.Offer) Way {
	if _, channel, _ := n.channelSender(ctx); channel {
		return WayChannel
	}
	if d, ok := n.tunnelSender(ctx); ok && o.Fetch == "" && n.relayOK(d) {
		return WayRelay
	}
	return WayOther
}
