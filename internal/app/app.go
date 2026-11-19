// Package app is Yonder without an interface: one Node that is findable on
// the local network, reachable by paired devices through the tunnel, and
// can send either way. The command line and the window both drive a Node.
package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"yonder/internal/identity"
	"yonder/internal/localsend"
	"yonder/internal/ownroute"
	"yonder/internal/pairing"
	"yonder/internal/tally/agent"
	"yonder/internal/tunnel"
)

// Config is what a Node needs to start.
type Config struct {
	// Dir holds this device's identity, tunnel keys and paired devices.
	Dir string
	// SaveDir is where received files go.
	SaveDir string
	// Name is how this device appears to others.
	Name string
	// Listen is the local network address to serve on, ":53317" if empty.
	// If the port is taken, a free one is used instead.
	Listen string
	// Browser says this Yonder is running in a web page, which it tells
	// the devices it talks to: see TypeWeb.
	Browser bool
	// Version is this Yonder's version, which it tells paired devices:
	// see versionHeader.
	Version string
}

// Model is what Yonder calls itself to other devices, where LocalSend names
// the kind of machine; it is how one Yonder knows another.
const Model = "Yonder"

// Node is one running Yonder.
type Node struct {
	// Info is how this device describes itself on the local network.
	Info localsend.Info
	// Receiver takes offers from both the local network and the tunnel.
	// Set its Accept and Received before offers are expected; until then
	// every offer is declined.
	Receiver *localsend.Receiver
	// Nearby lists the devices seen on the local network.
	Nearby *localsend.Registry
	// Updated, if set, is called when a paired device turns out to have a
	// new name or a new tunnel address, after it is stored.
	Updated func()
	// Announced, if set, is called each time this device has announced
	// itself on the local network, with what went wrong if no interface
	// took the announcement.
	Announced func(error)

	sender *localsend.Sender
	lender *localsend.Lender
	log    *tracer // the send log; nil in a browser
	store  *pairing.Store
	keys   *tunnel.Keys
	selfID tunnel.ClientID
	srv    *http.Server
	tlsCfg *tls.Config

	// ctx ends when the Node is closed.
	ctx  context.Context
	stop func()

	mu      sync.Mutex
	tun     *tunnel.Server
	clients map[string]link        // by fingerprint
	sending map[string]int         // transfers under way on our connection to a device, by fingerprint
	lending map[string]int         // sends under way that the device fetches, by fingerprint
	web     map[string]bool        // paired devices the relay may be used with, by fingerprint
	fetches map[string]bool        // paired devices that can fetch what we send, by fingerprint
	looking map[string]bool        // paired devices a direct path is being looked for to, by fingerprint
	started string                 // a random word for this run, told to paired devices: see startHeader
	starts  map[string]string      // the run each paired device last said it is on, by fingerprint
	version string                 // this Yonder's version as it is told: see versionHeader
	heard   map[string]bool        // paired devices that have said something in this run, by fingerprint
	said    map[string]string      // the version each of them said it has, by fingerprint
	renewed map[string]bool        // paired devices a new connection is being made to, by fingerprint
	finding map[string]*sync.Mutex // one search for a moved device at a time, by fingerprint
	// tied keeps the listeners that are tied to an interface (tied.go);
	// nil when there are none. tiedKick has it look at the interfaces now.
	tied     *tied
	tiedKick chan struct{}
	// For CheckHome: checks in a row the home relay did not answer, how
	// many it takes before the tunnel is opened again, and whether CheckHome
	// closed a tunnel it could not open again.
	homeMisses, homePatience int
	homeLost                 bool
	// homeAnswers says whether the tunnel answers through its home relay;
	// tests stand in for a relay that has stopped.
	homeAnswers func(ctx context.Context, tun *tunnel.Server) bool
	rule        pathRule
	// dc is how data channels are made, and chans what there is of them
	// (channel.go); n.mu guards chans.
	dc    channelRule
	chans channelState
	// peerID says which paired key is behind a tunnel connection; tests
	// stand in for the tunnel.
	peerID func(local, remote net.Addr) tunnel.ClientID
	// connect prepares a tunnel connection to a device, and listening says
	// whether paired devices can reach this one through the tunnel; tests
	// stand in for the tunnel.
	connect   func(addr tunnel.Addr, keys *tunnel.Keys) (link, error)
	listening func() bool
}

// link is a tunnel connection to one device: a tunnel.Client.
type link interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	Path(ctx context.Context) tunnel.Path
	Close() error
}

// ErrUnreachable means a paired device did not answer through the tunnel.
// The tunnel cannot tell the two reasons apart.
var ErrUnreachable = errors.New("the device is offline, or it has unpaired this one")

// ErrLinkEnded means a pairing link ended without a device paired: its
// owner said no, or nobody used it in time.
var ErrLinkEnded = errors.New("the pairing link has ended; make a new one to pair")

// Why a join failed, for whoever has to tell the user which step it was.
var (
	// ErrTunnelDown means this device's own tunnel is not open.
	ErrTunnelDown = errors.New("the tunnel is not open")
	// ErrOtherRelay means a pairing link points at a relay that is not in
	// Yonder's list, so the device behind it was not called.
	ErrOtherRelay = errors.New("the link points at a relay that is not Yonder's")
	// ErrNotReached means no connection to the other device was made.
	ErrNotReached = errors.New("the other device was not reached")
	// ErrNoAnswer means the other device was reached but its answer did not
	// come: its owner did not say yes in time, or the connection was lost.
	ErrNoAnswer = errors.New("the other device was reached but did not answer")
	// ErrNotStored means the other device paired, but this one could not
	// store it.
	ErrNotStored = errors.New("the pairing could not be stored on this device")
)

// linkLife is how long a pairing link works if nobody uses it.
const linkLife = 10 * time.Minute

const (
	// keptWait is how long a connection kept from earlier gets to answer;
	// a live one does in well under a second.
	keptWait = 4 * time.Second
	// freshWait is how long a new connection gets, which includes setting
	// up the tunnel. tailcat itself gives up after ten seconds.
	freshWait = 12 * time.Second
	// nearbyWait is how long a device on the local network gets to take a
	// connection. A firewall that drops what comes in never answers, and
	// the system would wait some two minutes.
	nearbyWait = 10 * time.Second
	// headerWait is how long a connection gets to send a request's
	// headers, the TLS handshake included, and idleWait how long it may
	// wait for its next request. They are long enough for the tunnel.
	headerWait = 20 * time.Second
	idleWait   = 2 * time.Minute
)

// newServer is an HTTP server for h with limits on the time a connection
// may hold it without a request: one that sends nothing, or sends it very
// slowly, is cut off. A request itself has no limit, since a file upload
// or a question to the owner takes as long as it takes.
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: headerWait,
		IdleTimeout:       idleWait,
		ErrorLog:          log.New(os.Stderr, "", 0),
	}
}

// tunnelPeerKey holds, in a request's ctx, the sender's address for a
// connection that came in through the tunnel, and nothing for one from the
// local network.
type tunnelPeerKey struct{}

// tunnelListener marks each connection it accepts as come through the
// tunnel, so that the one server for both sides can tell them apart: an
// address on the local network proves nothing.
type tunnelListener struct{ net.Listener }

type tunnelConn struct{ net.Conn }

func (l tunnelListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return tunnelConn{c}, nil
}

// Start brings up the local network side: the listener, but not discovery
// (see Discover) and not the tunnel (see OpenTunnel).
func Start(cfg Config) (*Node, error) {
	id, err := identity.LoadOrCreate(cfg.Dir)
	if err != nil {
		return nil, err
	}
	store := pairing.Open(cfg.Dir)
	keys, err := store.Keys()
	if err != nil {
		return nil, fmt.Errorf("tunnel keys: %w", err)
	}
	selfID, err := keys.ClientID()
	if err != nil {
		return nil, err
	}
	addr := cfg.Listen
	if addr == "" {
		addr = fmt.Sprintf(":%d", localsend.DefaultPort)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Most likely the stock LocalSend app has the default port. The
		// next one up is tried before any free one, so that a firewall
		// can be opened for Yonder beside LocalSend.
		host, port, _ := net.SplitHostPort(addr)
		if p, perr := strconv.Atoi(port); perr == nil && p > 0 {
			ln, err = net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p+1)))
		}
		if err != nil {
			ln, err = net.Listen("tcp", net.JoinHostPort(host, "0"))
		}
		if err != nil {
			return nil, err
		}
	}
	deviceType := "desktop"
	if cfg.Browser {
		deviceType = TypeWeb
	}
	n := &Node{
		Info: localsend.Info{
			Alias:       cfg.Name,
			Version:     localsend.Version,
			DeviceModel: Model,
			DeviceType:  deviceType,
			Fingerprint: id.Fingerprint,
			Port:        ln.Addr().(*net.TCPAddr).Port,
			Protocol:    "https",
		},
		Nearby:   &localsend.Registry{Self: id.Fingerprint},
		store:    store,
		keys:     keys,
		selfID:   selfID,
		tlsCfg:   id.TLSConfig(),
		lender:   &localsend.Lender{},
		clients:  map[string]link{},
		sending:  map[string]int{},
		lending:  map[string]int{},
		web:      map[string]bool{},
		fetches:  map[string]bool{},
		looking:  map[string]bool{},
		started:  rand.Text(),
		starts:   map[string]string{},
		version:  ToldVersion(cfg.Version),
		heard:    map[string]bool{},
		said:     map[string]string{},
		renewed:  map[string]bool{},
		finding:  map[string]*sync.Mutex{},
		tiedKick: make(chan struct{}, 1),
	}
	if !cfg.Browser {
		n.log = openTrace(cfg.Dir)
		n.lender.Trace = n.log.logf
	}
	n.ctx, n.stop = context.WithCancel(context.Background())
	n.rule = pathRule{path: n.path, wait: directWait, every: directEvery, guards: guardEvery}
	n.dc, n.chans = n.newChannelRule(), newChannelState()
	n.peerID = n.tunnelPeer
	n.connect = func(addr tunnel.Addr, keys *tunnel.Keys) (link, error) { return tunnel.Connect(addr, keys) }
	n.listening = func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.tun != nil
	}
	n.sender = &localsend.Sender{Info: n.Info, Cert: id.Cert, Reached: n.Nearby.Reached}
	n.Receiver = &localsend.Receiver{Info: n.Info, SaveDir: cfg.SaveDir, Registered: n.Nearby.Seen, Lender: n.lender}
	if !cfg.Browser {
		n.Receiver.Trace = n.log.logf
		// A browser may use the relay, so it has no need to fetch.
		n.Receiver.Source = n.source
	}
	n.srv = newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == directPath {
			n.answerDirect(w, r)
			return
		}
		if said := r.Header.Get(relayedHeader); said != "" {
			// For AdmitOffer, which writes it in the send log.
			r = r.WithContext(context.WithValue(r.Context(), relayedKey{}, said))
		}
		n.hear(w, r)
		n.Receiver.ServeHTTP(w, r)
	}))
	// Remember where each tunnel connection came from, and whose data
	// channel each of those connections is, for PairedSender.
	n.srv.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		if tc, ok := c.(*tls.Conn); ok {
			switch nc := tc.NetConn().(type) {
			case tunnelConn:
				return context.WithValue(ctx, tunnelPeerKey{}, c.RemoteAddr())
			case channelConn:
				return context.WithValue(ctx, channelPeerKey{}, nc.fingerprint)
			}
		}
		return ctx
	}
	n.Nearby.TLSOnly = func(fingerprint string) bool {
		devices, _ := n.store.Devices()
		return slices.ContainsFunc(devices, func(d pairing.Device) bool { return strings.EqualFold(d.Fingerprint, fingerprint) })
	}
	go n.serve(ln, false)
	if host, _, _ := net.SplitHostPort(addr); host == "" && ownroute.Share(ln) == nil {
		// Beside the listener for every interface, one for each interface
		// of a local network (tied.go). A listener at one address gets
		// none.
		n.tied = &tied{port: n.Info.Port, list: ownroute.Local, open: ownroute.Listen, serve: func(ln net.Listener) { n.serve(ln, false) }}
		go n.tied.run(n.ctx, n.tiedKick)
	}
	return n, nil
}

// serve answers LocalSend requests arriving on a listener, from the local
// network or, if tunneled, through the tunnel.
func (n *Node) serve(ln net.Listener, tunneled bool) {
	if tunneled {
		ln = tunnelListener{ln}
	}
	n.srv.Serve(tls.NewListener(ln, n.tlsCfg))
}

// Discover announces this device on the local network and listens for
// others until ctx ends.
func (n *Node) Discover(ctx context.Context) error {
	d := &localsend.Discovery{Info: n.Info, Sender: n.sender, Seen: n.Nearby.Seen, Announced: n.Announced}
	return d.Run(ctx)
}

// OpenTunnel makes this device reachable by its paired devices on other
// networks. It needs the internet. Only one running Node per Dir may have
// the tunnel open, since they would share an address.
func (n *Node) OpenTunnel(ctx context.Context) error {
	// The list of relays is loaded, and the home checked, only here, at
	// the start. The list is loaded before the lock is taken, since it may
	// wait on the network. The web app stays on the main relay, so needs
	// no list; without the site or a copy, the list built in will do.
	if n.Info.DeviceType != TypeWeb {
		tunnel.LoadRelays(ctx, n.store)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.openTunnel(ctx, tunnel.Start)
}

// A home relay that has stopped answering. CheckHome is called every
// HomeEvery; after homeMissesFirst checks in a row without an answer (some
// minutes), the tunnel is opened again as at the start, which moves the
// home to the nearest other relay if that one answers. If the home stays
// (the fault is more likely this device's own connection), the next try
// waits twice as many checks, up to homeMissesMost, so that a device
// without internet does not open its tunnel again every few minutes.
const (
	HomeEvery       = time.Minute
	homeAnswerWait  = 20 * time.Second
	homeMissesFirst = 3
	homeMissesMost  = 48
)

// CheckHome asks whether this device still answers through its home relay,
// and after enough silence opens the tunnel again, which moves the home if
// another relay answers (see tunnel.Start). It reports whether the address
// changed; paired devices learn the new one as this device next asks them
// anything. It does nothing with one relay in the list. Opening the tunnel
// again interrupts transfers through it, so do not call it during one.
func (n *Node) CheckHome(ctx context.Context) (moved bool, err error) {
	if n.Info.DeviceType == TypeWeb || len(tunnel.Relays()) < 2 {
		return false, nil
	}
	n.mu.Lock()
	tun, lost := n.tun, n.homeLost
	answers := n.homeAnswers
	n.mu.Unlock()
	if tun == nil && !lost {
		return false, nil // never opened, or closed for good
	}
	there := false
	if tun != nil {
		if answers == nil {
			answers = func(ctx context.Context, tun *tunnel.Server) bool { return tun.Answers(ctx, n.keys) }
		}
		actx, cancel := context.WithTimeout(ctx, homeAnswerWait)
		there = answers(actx, tun)
		cancel()
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
	}
	n.mu.Lock()
	if n.tun != tun {
		n.mu.Unlock()
		return false, nil // opened again meanwhile, as by Unpair
	}
	if n.homePatience == 0 {
		n.homePatience = homeMissesFirst
	}
	if there {
		n.homeMisses, n.homePatience = 0, homeMissesFirst
		n.mu.Unlock()
		return false, nil
	}
	var before tunnel.Addr
	if tun != nil {
		if n.homeMisses++; n.homeMisses < n.homePatience {
			n.mu.Unlock()
			return false, nil
		}
		before = tun.Addr()
		tun.Close()
		n.tun, n.homeLost = nil, true
	}
	n.homeMisses = 0
	if err := n.openTunnel(ctx, tunnel.Start); err != nil {
		n.mu.Unlock()
		return false, err // tried again at the next check
	}
	n.homeLost = false
	moved = before != "" && n.tun.Addr() != before
	if moved {
		n.homePatience = homeMissesFirst
	} else {
		n.homePatience = min(2*n.homePatience, homeMissesMost)
	}
	n.mu.Unlock()
	if moved && n.Updated != nil {
		n.Updated()
	}
	return moved, nil
}

// openTunnel opens the tunnel with listen, tunnel.Start or tunnel.Listen.
func (n *Node) openTunnel(ctx context.Context, listen func(context.Context, *tunnel.Keys, []tunnel.ClientID) (*tunnel.Server, error)) error {
	devices, err := n.store.Devices()
	if err != nil {
		return err
	}
	var allowed []tunnel.ClientID
	for _, d := range devices {
		allowed = append(allowed, d.ClientID)
	}
	region := string(n.keys.Region)
	tun, err := listen(ctx, n.keys, allowed)
	if err != nil {
		return err
	}
	// The first start, or the home relay has left the list: keep the new
	// address.
	if string(n.keys.Region) != region {
		if err := n.store.SaveKeys(n.keys); err != nil {
			tun.Close()
			return err
		}
	}
	n.tun = tun
	go n.serve(tun.Listener(), true)
	return nil
}

// self is this device's record for a device it pairs with.
func (n *Node) self() (pairing.Device, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.tun == nil {
		return pairing.Device{}, ErrTunnelDown
	}
	return pairing.Device{Name: n.Info.Alias, Fingerprint: n.Info.Fingerprint, Addr: n.tun.Addr(), ClientID: n.selfID}, nil
}

// add stores a newly paired device and lets it in.
func (n *Node) add(d pairing.Device) error {
	if d.Fingerprint == n.Info.Fingerprint {
		return errors.New("a device cannot pair with itself")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.tun == nil {
		return ErrTunnelDown
	}
	// Stored first, since the store may refuse it, and a key once let in
	// stays in until the app starts again.
	if err := n.store.Add(d); err != nil {
		return err
	}
	if err := n.tun.Allow(d.ClientID); err != nil {
		n.store.Remove(d.Fingerprint)
		return err
	}
	n.dropClient(d.Fingerprint) // its address may have changed
	return nil
}

// Pairing is an open invitation for one device to pair with this one.
type Pairing struct {
	// Link is what the other device needs: paste it there, or show it as
	// a QR code. It is a secret until the pairing is over.
	Link string

	ended   chan pairEnd
	expires time.Time
	asking  func() bool
	close   func()
}

// pairEnd is how a pairing ended: with the joiner paired, or err.
type pairEnd struct {
	joiner pairing.Device
	err    error
}

// Pair opens a pairing. confirm asks this device's owner about the device
// that answers. Call Wait to see it through; the link stops working when
// Wait returns, after the owner has been asked once, or after linkLife.
func (n *Node) Pair(ctx context.Context, confirm func(ctx context.Context, joiner pairing.Device) bool) (*Pairing, error) {
	self, err := n.self()
	if err != nil {
		return nil, err
	}
	// A separate, short-lived address: the saved one only lets in devices
	// that are already paired, and never appears in a link.
	temp, err := tunnel.Listen(ctx, nil, nil)
	if err != nil {
		return nil, err
	}
	p := &Pairing{Link: pairing.Link(temp.Addr()), ended: make(chan pairEnd, 1), expires: time.Now().Add(linkLife)}
	host := &pairing.Host{Self: self, Confirm: confirm, Paired: n.add, Expires: p.expires,
		Ended: func(joiner pairing.Device, err error) {
			if errors.Is(err, pairing.ErrDeclined) {
				err = ErrLinkEnded
			}
			p.ended <- pairEnd{joiner, err}
		}}
	p.asking = host.Asking
	srv := newServer(host)
	go srv.Serve(temp.Listener())
	p.close = func() {
		// Let the reply to the joiner get out before the address dies.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		temp.Drain(ctx)
		temp.Close()
	}
	return p, nil
}

// Expires is when the link stops working if nobody has used it.
func (p *Pairing) Expires() time.Time { return p.expires }

// Wait blocks until the owner has been asked, the link has expired
// (ErrLinkEnded) or ctx ends, then closes the pairing. A "no" is
// ErrLinkEnded; a device that could not be stored is the reason, such as
// pairing.ErrPairedUnderAnotherKey.
func (p *Pairing) Wait(ctx context.Context) (pairing.Device, error) {
	defer p.close()
	expired := time.NewTimer(time.Until(p.expires))
	defer expired.Stop()
	select {
	case e := <-p.ended:
		return e.joiner, e.err
	case <-expired.C:
		if !p.asking() {
			return pairing.Device{}, ErrLinkEnded
		}
		// The owner is being asked about a device that came in time:
		// their answer still counts.
		select {
		case e := <-p.ended:
			return e.joiner, e.err
		case <-ctx.Done():
			return pairing.Device{}, ctx.Err()
		}
	case <-ctx.Done():
		return pairing.Device{}, ctx.Err()
	}
}

// Join pairs with the device that made link.
func (n *Node) Join(ctx context.Context, link string) (pairing.Device, error) {
	addr, err := pairing.ParseLink(link)
	if err != nil {
		return pairing.Device{}, err
	}
	// The link is from anyone; its address is dialled only on Yonder's
	// relays.
	if addr.Valid() && !addr.Listed() {
		return pairing.Device{}, ErrOtherRelay
	}
	self, err := n.self()
	if err != nil {
		return pairing.Device{}, err
	}
	cl, err := tunnel.Connect(addr, nil)
	if err != nil {
		return pairing.Device{}, err
	}
	defer cl.Close()
	// Whether a connection was made tells "not reached" from "no answer".
	var reached atomic.Bool
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := cl.DialContext(ctx, network, addr)
		if err == nil {
			reached.Store(true)
		}
		return conn, err
	}
	host, err := pairing.Join(ctx, dial, self)
	switch {
	case errors.Is(err, pairing.ErrDeclined), errors.Is(err, pairing.ErrPairedUnderAnotherKey), errors.Is(err, pairing.ErrHostNotStored):
		return pairing.Device{}, err
	case err != nil && reached.Load():
		return pairing.Device{}, fmt.Errorf("%w: %w", ErrNoAnswer, err)
	case err != nil:
		return pairing.Device{}, fmt.Errorf("%w: %w", ErrNotReached, err)
	}
	if err := n.add(host); err != nil {
		if errors.Is(err, pairing.ErrPairedUnderAnotherKey) {
			return pairing.Device{}, err
		}
		return pairing.Device{}, fmt.Errorf("%w: %w", ErrNotStored, err)
	}
	return host, nil
}

// Paired lists the paired devices, by name.
func (n *Node) Paired() ([]pairing.Device, error) { return n.store.Devices() }

// FindPaired returns the paired device with the given name, ignoring case.
func (n *Node) FindPaired(name string) (pairing.Device, bool) {
	devices, _ := n.store.Devices()
	for _, d := range devices {
		if strings.EqualFold(d.Name, name) {
			return d, true
		}
	}
	return pairing.Device{}, false
}

// paired is the stored record of the paired device with this fingerprint.
func (n *Node) paired(fingerprint string) (pairing.Device, bool) {
	devices, _ := n.store.Devices()
	for _, d := range devices {
		if d.Fingerprint == fingerprint {
			return d, true
		}
	}
	return pairing.Device{}, false
}

// Unpair forgets a device and stops letting it in. If the tunnel is open it
// is restarted, because a client cannot be taken out of a running one; that
// interrupts transfers from other paired devices.
func (n *Node) Unpair(ctx context.Context, fingerprint string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.store.Remove(fingerprint); err != nil {
		return err
	}
	n.dropClient(fingerprint)
	n.closeChannels(fingerprint)
	if n.tun == nil {
		return nil
	}
	n.tun.Close()
	n.tun = nil
	// The same home, without looking at the list again.
	return n.openTunnel(ctx, tunnel.Listen)
}

// client returns the tunnel connection to a paired device, making it on
// first use.
func (n *Node) client(d pairing.Device) (link, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if cl, ok := n.clients[d.Fingerprint]; ok {
		return cl, nil
	}
	cl, err := n.connect(d.Addr, n.keys)
	if err != nil {
		return nil, err
	}
	n.clients[d.Fingerprint] = cl
	return cl, nil
}

// dropClient closes the connection to a device, if any. n.mu must be held.
func (n *Node) dropClient(fingerprint string) {
	if cl, ok := n.clients[fingerprint]; ok {
		cl.Close()
		delete(n.clients, fingerprint)
	}
}

// through returns a sender that reaches a paired device by way of cl, and
// the device as that sender addresses it.
func (n *Node) through(cl link, d pairing.Device) (*localsend.Sender, localsend.Peer) {
	sender := *n.sender
	sender.Dial = cl.DialContext
	// Each question tells it where we are now, and which run of the
	// program this is; see hear.
	sender.AskHeader = http.Header{startHeader: {n.started}, versionHeader: {n.version}}
	sender.OfferHeader = http.Header{versionHeader: {n.version}}
	n.mu.Lock()
	if n.tun != nil {
		sender.AskHeader.Set(addrHeader, string(n.tun.Addr()))
	}
	n.mu.Unlock()
	// The host is a placeholder, since the tunnel's dialler ignores it.
	return &sender, localsend.Peer{Host: "device.yonder", Info: localsend.Info{
		Alias: d.Name, Fingerprint: d.Fingerprint, Port: tunnel.Port, Protocol: "https",
	}}
}

// addrHeader carries a device's own tunnel address with each question it
// asks a paired device through the tunnel, so that a device whose address
// has changed, as when its relay does, is still found. The tunnel keeps it
// secret, as it does everything else.
const addrHeader = "Yonder-Address"

// startHeader carries a random word that a device makes each time it
// starts, with each question it asks a paired device through the tunnel
// (from 0.1.21). A new word means the device has started again, and then
// this device's kept connection to it is dead without saying so: see
// restarted.
const startHeader = "Yonder-Start"

// versionHeader carries a device's version of Yonder with each question it
// asks a paired device through the tunnel, with the answer, and with each
// offer of files (from 0.1.31). A development build says "dev". A device
// that says none has a Yonder from before; see PeerVersion.
const versionHeader = "Yonder-Version"

// ToldVersion is a version as one Yonder tells it to another: three
// numbers, or "dev" for anything else.
func ToldVersion(version string) string {
	if !agent.KnownVersion(version) {
		return "dev"
	}
	return version
}

// heardFrom records that a paired device has said something in this run,
// and the version it said it has: empty if it said none, or one that is
// not a version.
func (n *Node) heardFrom(d pairing.Device, version string) {
	if version != "dev" && !agent.KnownVersion(version) {
		version = ""
	}
	n.mu.Lock()
	was, before := n.said[d.Fingerprint], n.heard[d.Fingerprint]
	n.heard[d.Fingerprint], n.said[d.Fingerprint] = true, version
	n.mu.Unlock()
	switch {
	case before && was == version:
	case version == "":
		n.log.logf("version: %q says none, so its Yonder is from before 0.1.31", localsend.Clip(d.Name))
	default:
		n.log.logf("version: %q has Yonder %s; this one is %s", localsend.Clip(d.Name), version, n.version)
	}
}

// PeerVersion is the version of Yonder a paired device said it has, in
// this run of the program: "dev" for a development build, and empty for a
// device that said none, whose Yonder is older than the first that says
// it. heard is false if the device has said nothing at all yet, and then
// nothing is known.
func (n *Node) PeerVersion(fingerprint string) (version string, heard bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.said[fingerprint], n.heard[fingerprint]
}

// hear takes what a paired device says beside LocalSend's own words, from a
// request that came through the tunnel, where who is asking is certain: the
// address it is now at, its version, and that it is about to send (see
// direct.go). Its questions are also answered with this device's version
// and whether it can fetch.
func (n *Node) hear(w http.ResponseWriter, r *http.Request) {
	addr := tunnel.Addr(r.Header.Get(addrHeader))
	question := localsend.IsQuestion(r)
	version := r.Header.Get(versionHeader)
	if addr == "" && !question && version == "" {
		return
	}
	d, ok := n.tunnelSender(r.Context())
	if !ok {
		return
	}
	if question || version != "" {
		n.heardFrom(d, version)
	}
	if question {
		w.Header().Set(versionHeader, n.version)
	}
	if start := r.Header.Get(startHeader); start != "" {
		n.restarted(d, start)
	}
	if addr != "" && d.Addr != addr {
		if changed, _ := n.store.Readdress(d.Fingerprint, addr); changed {
			n.readdressed(d.Fingerprint)
			d.Addr = addr
		}
	}
	if question && n.Receiver.Source != nil {
		w.Header().Set(localsend.FetchHeader, fetchCan)
		if r.Header.Get(localsend.FetchHeader) == fetchLook {
			n.log.logf("look: %q is about to send", localsend.Clip(d.Name))
			n.look(d)
		}
	}
}

// restarted takes the run that a paired device says it is on. If it is a
// new one, the connection kept to the device is from before it started
// again, and the next send would wait for it in vain before making another
// (see reachAt); so it is dropped and a new one made now, unless a transfer
// is using it.
func (n *Node) restarted(d pairing.Device, start string) {
	n.mu.Lock()
	before := n.starts[d.Fingerprint]
	n.starts[d.Fingerprint] = start
	_, kept := n.clients[d.Fingerprint]
	renew := before != "" && before != start && kept && n.sending[d.Fingerprint] == 0 && !n.renewed[d.Fingerprint]
	if renew {
		n.dropClient(d.Fingerprint)
		n.renewed[d.Fingerprint] = true
	}
	n.mu.Unlock()
	if !renew {
		return
	}
	go func() {
		defer func() {
			n.mu.Lock()
			delete(n.renewed, d.Fingerprint)
			n.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(n.ctx, freshWait)
		defer cancel()
		start := time.Now()
		_, err := n.reach(ctx, d)
		n.log.logf("%q has started again: a new connection to it after %v, error: %v", localsend.Clip(d.Name), since(start), err)
	}()
}

// readdressed is called when a paired device's stored address has changed.
func (n *Node) readdressed(fingerprint string) {
	n.mu.Lock()
	// A connection to the old address is no use, unless a send shows
	// otherwise.
	if n.sending[fingerprint] == 0 {
		n.dropClient(fingerprint)
	}
	n.mu.Unlock()
	if n.Updated != nil {
		n.Updated()
	}
}

// reach returns a connection to a paired device that is known to work, or
// ErrUnreachable. If the device does not answer at its address, it is
// looked for on the other relays in the list, where it is once it has moved
// (see find); an answer there is remembered.
func (n *Node) reach(ctx context.Context, d pairing.Device) (link, error) {
	cl, err := n.reachAt(ctx, d)
	if err == nil || ctx.Err() != nil {
		return cl, err
	}
	// One search at a time for a device. One that waited may find the
	// device already found, at the address it is now stored with.
	n.mu.Lock()
	finding := n.finding[d.Fingerprint]
	if finding == nil {
		finding = new(sync.Mutex)
		n.finding[d.Fingerprint] = finding
	}
	n.mu.Unlock()
	finding.Lock()
	defer finding.Unlock()
	if now, ok := n.paired(d.Fingerprint); ok && now.Addr != d.Addr {
		return n.reachAt(ctx, now)
	}
	cl, addr, ok := n.find(ctx, d)
	if !ok {
		return nil, err
	}
	n.mu.Lock()
	if kept, ok := n.clients[d.Fingerprint]; ok && n.sending[d.Fingerprint] > 0 {
		// A send began meanwhile, and shows the kept connection works.
		n.mu.Unlock()
		cl.Close()
		return kept, nil
	}
	n.dropClient(d.Fingerprint)
	n.clients[d.Fingerprint] = cl
	n.mu.Unlock()
	if changed, _ := n.store.Readdress(d.Fingerprint, addr); changed && n.Updated != nil {
		n.Updated()
	}
	return cl, nil
}

// searchers is how many relays find tries at once.
const searchers = 3

// find looks for a paired device on each relay in the list other than the
// one its address points at, a few at a time, and returns a connection to
// it at the first address it answers at.
func (n *Node) find(ctx context.Context, d pairing.Device) (link, tunnel.Addr, bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type found struct {
		cl   link
		addr tunnel.Addr
	}
	addrs := d.Addr.Elsewhere()
	answers := make(chan found, len(addrs))
	turns := make(chan struct{}, searchers)
	var wg sync.WaitGroup
	for _, addr := range addrs {
		wg.Go(func() {
			select {
			case turns <- struct{}{}:
				defer func() { <-turns }()
			case <-ctx.Done():
				return
			}
			cl, err := n.connect(addr, n.keys)
			if err != nil {
				return
			}
			at := d
			at.Addr = addr
			if n.ask(ctx, cl, at, freshWait) != nil {
				cl.Close()
				return
			}
			answers <- found{cl, addr}
			cancel()
		})
	}
	wg.Wait()
	close(answers)
	first, ok := <-answers
	for other := range answers {
		other.cl.Close()
	}
	return first.cl, first.addr, ok
}

// reachAt is reach for the address in d. A connection kept from earlier is
// tried first, with a real request, because it goes stale without saying so
// when the device restarts or unpairs us; if it fails, one fresh connection
// is tried.
//
// A connection with a transfer under way on it, a send or a fetch, is not
// questioned: the transfer shows it works, and on a slow relay the request
// waits behind the transfer's data, so its silence would close the
// connection and end the transfer.
func (n *Node) reachAt(ctx context.Context, d pairing.Device) (link, error) {
	n.mu.Lock()
	cl, kept := n.clients[d.Fingerprint]
	busy := kept && n.sending[d.Fingerprint] > 0
	n.mu.Unlock()
	if busy {
		return cl, nil
	}
	for range 2 {
		cl, err := n.client(d)
		if err != nil {
			return nil, err
		}
		wait := freshWait
		if kept {
			wait = keptWait
		}
		if n.ask(ctx, cl, d, wait) == nil {
			return cl, nil
		}
		n.mu.Lock()
		if n.sending[d.Fingerprint] > 0 {
			// A send began while the request was out.
			n.mu.Unlock()
			return cl, nil
		}
		n.dropClient(d.Fingerprint)
		n.mu.Unlock()
		if !kept || ctx.Err() != nil {
			break
		}
		kept = false
	}
	return nil, fmt.Errorf("%s: %w", d.Name, ErrUnreachable)
}

// ask puts a real question to a paired device through cl, waiting at most
// wait for the answer, and takes in what the answer says about it.
func (n *Node) ask(ctx context.Context, cl link, d pairing.Device, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	sender, to := n.through(cl, d)
	info, said, err := sender.Question(ctx, to, nil)
	if err != nil {
		return err
	}
	// Its answer carries its name, which it may have changed since
	// pairing; this is the only way that reaches us.
	if changed, _ := n.store.Rename(d.Fingerprint, info.Alias); changed && n.Updated != nil {
		n.Updated()
	}
	// And what kind of device it is, and whether it can fetch what this
	// one sends, for the rule in direct.go.
	n.met(d.Fingerprint, info.DeviceType)
	n.heardFrom(d, said.Get(versionHeader))
	n.mu.Lock()
	n.fetches[d.Fingerprint] = said.Get(localsend.FetchHeader) == fetchCan
	n.mu.Unlock()
	return nil
}

// Online reports whether a paired device answers through the tunnel. "No"
// takes up to about fifteen seconds, or until ctx ends, and also covers a
// device that has unpaired us.
func (n *Node) Online(ctx context.Context, d pairing.Device) bool {
	_, err := n.reach(ctx, d)
	return err == nil
}

// NearbyOnline reports whether a device on the local network still answers,
// by introducing this device to it again.
func (n *Node) NearbyOnline(ctx context.Context, p localsend.Peer) bool {
	_, err := n.sender.Register(ctx, p)
	return err == nil
}

// Reaches reports whether this device can connect to another on the local
// network at one of addrs, for a device that asked to have its firewall
// tested from outside. is recognises the asker by the fingerprint of the
// certificate that answers; anything else at an address counts as not
// reached, so the answer says nothing about other machines. Addresses on
// the internet are not tried.
func (n *Node) Reaches(ctx context.Context, addrs []string, is func(fingerprint string) bool) bool {
	conf := &tls.Config{
		InsecureSkipVerify: true, // who answered is checked by is
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &n.sender.Cert, nil
		},
	}
	for _, a := range addrs {
		ap, err := netip.ParseAddrPort(a)
		if ip := ap.Addr(); err != nil || !(ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback()) {
			continue
		}
		d := ownroute.Dialer(ap.Addr().String())
		d.Timeout = nearbyWait / 2
		conn, err := (&tls.Dialer{NetDialer: d, Config: conf}).DialContext(ctx, "tcp", ap.String())
		if err != nil {
			continue
		}
		certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
		conn.Close()
		if len(certs) > 0 && is(identity.Fingerprint(certs[0].Raw)) {
			return true
		}
	}
	return false
}

// Relayed reports whether traffic with a paired device is going through a
// relay, which is slow. outgoing says who connected: true when this device
// reached out to it, false when it reached in to us. For files that arrive,
// that is whether this device fetches them (Offer.Fetch).
func (n *Node) Relayed(ctx context.Context, d pairing.Device, outgoing bool) bool {
	return n.path(ctx, d, outgoing) == tunnel.PathRelayed
}

// SendRelayed is Relayed for the send to a paired device that is under way:
// its files go out on this device's connection, unless the device fetches
// them. A send over a data channel is not relayed, whatever the tunnel is.
func (n *Node) SendRelayed(ctx context.Context, d pairing.Device) bool {
	n.mu.Lock()
	lent, channelled := n.lending[d.Fingerprint] > 0, n.chans.sending[d.Fingerprint] > 0
	n.mu.Unlock()
	return !channelled && n.Relayed(ctx, d, !lent)
}

// SendNearby sends files to a device on the local network and returns how
// many it took.
func (n *Node) SendNearby(ctx context.Context, to localsend.Peer, paths []string) (int, error) {
	return n.sender.Send(ctx, to, paths)
}

// SendPaired sends files to a paired device through the tunnel and returns
// how many it took. It fails with ErrUnreachable if the device does not
// answer, and with ErrNoDirectPath if the device is another app and the
// files would go through the relay (see direct.go, which also says which of
// the two devices' connections carries them). With a browser at one end the
// files go over a data channel if one forms, and through the relay if not:
// WithWay tells which.
func (n *Node) SendPaired(ctx context.Context, d pairing.Device, paths []string) (sent int, err error) {
	start := time.Now()
	n.log.logf("send to %q: started, files: %d", localsend.Clip(d.Name), len(paths))
	defer func() {
		n.log.logf("send to %q: ended after %v, sent: %d, error: %v", localsend.Clip(d.Name), since(start), sent, localsend.Unnamed(err))
	}()
	cl, err := n.reach(ctx, d)
	if err != nil {
		n.log.logf("send to %q: no answer after %v", localsend.Clip(d.Name), since(start))
		return 0, err
	}
	n.log.logf("send to %q: it answered after %v", localsend.Clip(d.Name), since(start))
	defer n.busy(d)()
	sender, to := n.through(cl, d)
	if n.relayOK(d) {
		return n.sendWeb(ctx, sender, to, d, paths)
	}
	return n.sendDirect(ctx, sender, to, d, paths)
}

// busy counts a transfer on this device's connection to a paired device,
// until the function it returns is called.
func (n *Node) busy(d pairing.Device) (done func()) {
	n.mu.Lock()
	n.sending[d.Fingerprint]++
	n.mu.Unlock()
	return func() {
		n.mu.Lock()
		n.sending[d.Fingerprint]--
		n.mu.Unlock()
	}
}

// PairedSender says which paired device an offer came from, given the ctx
// passed to Receiver.Accept. It is false for offers from the local network,
// whatever address they come from. What proves the device is the tunnel
// (tunnelSender) or, on a data channel, what channel.go says at its top.
func (n *Node) PairedSender(ctx context.Context) (pairing.Device, bool) {
	if d, channel, ok := n.channelSender(ctx); channel {
		return d, ok
	}
	return n.tunnelSender(ctx)
}

// tunnelSender is PairedSender for what came through the tunnel itself, and
// false for everything else: what only the tunnel may carry asks here.
func (n *Node) tunnelSender(ctx context.Context) (pairing.Device, bool) {
	local, _ := ctx.Value(http.LocalAddrContextKey).(net.Addr)
	remote, _ := ctx.Value(tunnelPeerKey{}).(net.Addr)
	if local == nil || remote == nil {
		return pairing.Device{}, false
	}
	id := n.peerID(local, remote)
	if id == "" {
		return pairing.Device{}, false
	}
	devices, _ := n.store.Devices()
	for _, d := range devices {
		if d.ClientID == id {
			return d, true
		}
	}
	return pairing.Device{}, false
}

// tunnelPeer is the key behind a connection that came through the tunnel,
// or "" without one.
func (n *Node) tunnelPeer(local, remote net.Addr) tunnel.ClientID {
	n.mu.Lock()
	tun := n.tun
	n.mu.Unlock()
	if tun == nil {
		return ""
	}
	return tun.PeerID(local, remote)
}

// Close stops the Node.
func (n *Node) Close() {
	n.stop()
	n.mu.Lock()
	defer n.mu.Unlock()
	n.srv.Close()
	n.closeChannels("")
	n.log.close()
	for fp := range n.clients {
		n.dropClient(fp)
	}
	if n.tun != nil {
		n.tun.Close()
		n.tun = nil
	}
}
