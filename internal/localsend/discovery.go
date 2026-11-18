package localsend

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// multicastGroup is LocalSend's default discovery address, and
// multicastGroup6 the one LocalSend adds for IPv6, which only reaches the
// local link. A device is announced on both: its IPv6 address is the one a
// network that rewrites or misroutes IPv4 leaves alone.
var (
	multicastGroup  = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 167), Port: DefaultPort}
	multicastGroup6 = &net.UDPAddr{IP: net.ParseIP("ff12::fd3a:e420"), Port: DefaultPort}
)

// maxAddresses is how many addresses are kept for one device.
const maxAddresses = 8

// answerEvery limits how often one device at one address gets an answer. A
// device announces on each of its interfaces and each is heard on each of
// ours, so one start can arrive many times over, all within milliseconds.
// It is kept short so a device that restarts straight away is still answered.
const answerEvery = time.Second

// Bounds on what a flood of announcements can make this device keep or do.
// maxPeers and maxAnswered limit the two lists that grow with every new
// fingerprint a packet names; a device is dropped from the list only after
// it has been quiet for peerStale (it announces every announceEvery), so a
// flood cannot push real devices out. maxPeersFrom is how many of them one
// address can add, so one device cannot fill the list. maxHandlers is how many packets are
// dealt with at once; the rest are dropped, as UDP allows.
const (
	maxPeers      = 1024
	maxPeersFrom  = 16
	maxAnswered   = 1024
	peerStale     = 10 * time.Minute
	maxHandlers   = 32
	minAnswerPort = 1024
)

// announceEvery is how often a device says again that it is here. Once, at
// the start, is not enough: that message is lost if the network or the
// system's permission to use it comes a moment later, and a device that
// starts afterwards and misses our answer never learns of us.
const announceEvery = 30 * time.Second

// Peer is a device seen on the local network. Host is the address most
// likely to reach it and Others the rest it was seen at, next best first; a
// Sender tries them in that order.
type Peer struct {
	Info     Info
	Host     string
	Others   []string
	LastSeen time.Time
}

// Hosts lists every address of the peer, best first.
func (p Peer) Hosts() []string {
	return append([]string{p.Host}, p.Others...)
}

// Encrypted reports whether files go to the device over TLS, where its
// certificate proves it is the one with the fingerprint. It is false for a
// device that says it takes them without (see Info.encrypted).
func (p Peer) Encrypted() bool { return p.Info.encrypted() }

func (p Peer) baseURL() string {
	protocol, port := "https", p.Info.Port
	if !p.Encrypted() {
		protocol = "http"
	}
	if port == 0 {
		port = DefaultPort
	}
	// A link-local IPv6 address names its interface after a "%", which a
	// URL writes as "%25".
	host := strings.Replace(p.Host, "%", "%25", 1)
	return protocol + "://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// Registry is the list of devices seen, one entry per fingerprint however
// many times or at however many addresses a device shows up. Every address
// is kept, as LocalSend does: the address a device's call seems to come
// from is not always one it can be reached at (a router may have rewritten
// it), so the last address heard must not replace one that works.
type Registry struct {
	// Self is this device's fingerprint, which is never listed.
	Self string
	// Added, if set, is called the first time a device is seen.
	Added func(Peer)
	// TLSOnly, if set, says which fingerprints are only ever listed with
	// TLS: the paired devices, whose files must not go to whoever
	// announces their fingerprint without it.
	TLSOnly func(fingerprint string) bool

	mu    sync.Mutex
	peers map[string]*seen
}

// seen is one device in a Registry.
type seen struct {
	info     Info
	addrs    []address // best first
	lastSeen time.Time
}

// address is one place a device was seen at.
type address struct {
	host string
	// confirmed means this device connected there itself and the device
	// answered with its own certificate; failed means the last try did not.
	confirmed, failed bool
	seen              time.Time
}

// rank orders addresses by how likely they are to work: those that have not
// failed, then those this device has reached itself, then IPv6 (LocalSend's
// order, and the addresses a router leaves alone), then the newest.
func rank(addrs []address) {
	sort.SliceStable(addrs, func(i, j int) bool {
		a, b := addrs[i], addrs[j]
		if a.failed != b.failed {
			return b.failed
		}
		if a.confirmed != b.confirmed {
			return a.confirmed
		}
		if a6, b6 := strings.Contains(a.host, ":"), strings.Contains(b.host, ":"); a6 != b6 {
			return a6
		}
		return a.seen.After(b.seen)
	})
}

func (d *seen) peer() Peer {
	p := Peer{Info: d.info, Host: d.addrs[0].host, LastSeen: d.lastSeen}
	for _, a := range d.addrs[1:] {
		p.Others = append(p.Others, a.host)
	}
	return p
}

// Seen records a device at an address. Its signature matches
// Receiver.Registered and Discovery.Seen.
//
// Anyone on the network can announce another device's fingerprint, so what
// is said of a device is believed with care. A device seen with TLS stays
// so: word that it takes files without is ignored, or one packet would send
// its files elsewhere, unencrypted and unchecked. And once this device has
// reached it, its name and port change only when the news comes from an
// address it was reached at.
//
// What is kept is short: the names are cleaned and cut (see Info.cleaned),
// and one address adds maxPeersFrom devices at most.
func (r *Registry) Seen(info Info, host string) {
	info, ok := info.cleaned()
	if !ok || info.Fingerprint == "" || info.Fingerprint == r.Self {
		return
	}
	now := time.Now()
	r.mu.Lock()
	d, known := r.peers[info.Fingerprint]
	if !known {
		if r.peers == nil {
			r.peers = map[string]*seen{}
		}
		if len(r.peers) >= maxPeers {
			for k, o := range r.peers {
				if now.Sub(o.lastSeen) >= peerStale {
					delete(r.peers, k)
				}
			}
			if len(r.peers) >= maxPeers {
				r.mu.Unlock()
				return
			}
		}
		from := 0
		for _, o := range r.peers {
			if slices.ContainsFunc(o.addrs, func(a address) bool { return a.host == host }) {
				from++
			}
		}
		if from >= maxPeersFrom {
			r.mu.Unlock()
			return
		}
		d = &seen{}
		r.peers[info.Fingerprint] = d
	} else if d.info.encrypted() && !info.encrypted() {
		r.mu.Unlock()
		return
	} else if !d.info.encrypted() && info.encrypted() {
		// Where it was heard without TLS proves nothing: start again.
		d.info, d.addrs = info, nil
	}
	at, reached := -1, false
	for i, a := range d.addrs {
		if a.host == host {
			at = i
		}
		reached = reached || a.confirmed
	}
	if !reached || (at >= 0 && d.addrs[at].confirmed) {
		d.info = info
	}
	d.lastSeen = now
	if at < 0 {
		d.addrs = append(d.addrs, address{host: host})
		at = len(d.addrs) - 1
	}
	d.addrs[at].seen = now
	rank(d.addrs)
	if len(d.addrs) > maxAddresses {
		d.addrs = d.addrs[:maxAddresses]
	}
	p := d.peer()
	r.mu.Unlock()
	if !known && r.Added != nil {
		r.Added(p)
	}
}

// Reached records how a connection to a device at one of its addresses
// went. Its signature matches Sender.Reached.
func (r *Registry) Reached(fingerprint, host string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.peers[fingerprint]
	if d == nil {
		return
	}
	for i, a := range d.addrs {
		if a.host == host {
			d.addrs[i].confirmed, d.addrs[i].failed = ok, !ok
		}
	}
	rank(d.addrs)
}

// Forget drops a device, until it is seen again.
func (r *Registry) Forget(fingerprint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.peers, fingerprint)
}

// List returns the devices seen, by name.
func (r *Registry) List() []Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Peer, 0, len(r.peers))
	for _, d := range r.peers {
		if !d.info.encrypted() && r.TLSOnly != nil && r.TLSOnly(d.info.Fingerprint) {
			continue
		}
		out = append(out, d.peer())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Info.Alias < out[j].Info.Alias })
	return out
}

// Find returns the device with the given name, ignoring case.
func (r *Registry) Find(alias string) (Peer, bool) {
	for _, p := range r.List() {
		if strings.EqualFold(p.Info.Alias, alias) {
			return p, true
		}
	}
	return Peer{}, false
}

// Discovery finds LocalSend devices on the local network: it announces this
// device, listens for others announcing, and answers them.
type Discovery struct {
	// Info is this device, with Port and Protocol set.
	Info Info
	// Sender answers announcements by registering with their sender.
	Sender *Sender
	// Seen, if set, is called for every device heard, repeats included.
	Seen func(info Info, host string)
	// Announced, if set, is called after each announcement with what went
	// wrong, nil if at least one interface took it.
	Announced func(error)

	mu       sync.Mutex
	answered map[string]time.Time
}

// Run announces this device and listens until ctx ends. It fails at once if
// no interface can listen.
func (d *Discovery) Run(ctx context.Context) error {
	conns := listenMulticast()
	if len(conns) == 0 {
		return errors.New("no network interface can listen for LocalSend devices")
	}
	slots := make(chan struct{}, maxHandlers)
	for _, c := range conns {
		go func() {
			buf := make([]byte, 64<<10)
			for {
				n, from, err := c.ReadFromUDP(buf)
				if err != nil {
					return
				}
				select {
				case slots <- struct{}{}:
					msg, host := append([]byte(nil), buf[:n]...), hostOf(from)
					go func() {
						defer func() { <-slots }()
						d.handle(ctx, msg, host)
					}()
				default: // busy: this packet is dropped
				}
			}
		}()
	}
	err := d.announce()
	again := time.NewTicker(announceEvery)
	defer again.Stop()
	for ctx.Err() == nil {
		select {
		case <-again.C:
			d.announce()
		case <-ctx.Done():
		}
	}
	for _, c := range conns {
		c.Close()
	}
	return err
}

func (d *Discovery) announce() error {
	err := Announce(d.Info)
	if d.Announced != nil {
		d.Announced(err)
	}
	return err
}

// handle deals with one discovery message: note the device, and if it was
// announcing, answer so it learns about us.
func (d *Discovery) handle(ctx context.Context, msg []byte, host string) {
	var info Info
	if json.Unmarshal(msg, &info) != nil {
		return
	}
	info, ok := info.cleaned()
	if !ok || info.Fingerprint == "" || info.Fingerprint == d.Info.Fingerprint {
		return
	}
	if d.Seen != nil {
		d.Seen(info, host)
	}
	if info.Announce == nil || !*info.Announce || !answerable(info, host) || !d.shouldAnswer(info.Fingerprint+"|"+host) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := d.Sender.Register(ctx, Peer{Info: info, Host: host}); err != nil {
		// The protocol's fallback: answer over multicast instead.
		multicast(d.Info, false)
	}
}

// answerable reports whether an announcement may be answered by connecting
// back to the address it came from, at the port it names. The sender
// address of a UDP packet can be forged, so only addresses on a local
// network are called, and not a port a service of the host's own would use.
func answerable(info Info, host string) bool {
	if info.Port != 0 && info.Port < minAnswerPort || info.Port > 65535 {
		return false
	}
	return isLocalHost(host)
}

// cgnat is the shared address space (100.64.0.0/10) that carriers and some
// VPNs, Tailscale among them, use.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// isLocalHost reports whether host, with or without an IPv6 zone, is a
// private, link-local, loopback or shared address.
func isLocalHost(host string) bool {
	host, _, _ = strings.Cut(host, "%")
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}

func (d *Discovery) shouldAnswer(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	if now.Sub(d.answered[key]) < answerEvery {
		return false
	}
	if d.answered == nil {
		d.answered = map[string]time.Time{}
	}
	if len(d.answered) >= maxAnswered {
		for k, t := range d.answered { // those older than answerEvery change nothing
			if now.Sub(t) >= answerEvery {
				delete(d.answered, k)
			}
		}
		if len(d.answered) >= maxAnswered {
			return false
		}
	}
	d.answered[key] = now
	return true
}

// Announce tells the local network this device is here. Devices that hear it
// answer by calling register on info.Port.
func Announce(info Info) error {
	return multicast(info, true)
}

// hostOf is the address a message came from, as a host to connect to: a
// link-local IPv6 address needs its interface, which follows a "%".
func hostOf(from *net.UDPAddr) string {
	if from.Zone != "" {
		return from.IP.String() + "%" + from.Zone
	}
	return from.IP.String()
}

// listenMulticast joins the discovery groups on every interface that can.
func listenMulticast() []*net.UDPConn {
	var conns []*net.UDPConn
	ifaces, err := net.Interfaces()
	if err != nil {
		// An Android app may not list the interfaces. Joined with none
		// named, the group is heard on the one the system picks, which
		// is the Wi-Fi when the phone is on it.
		if c, err := net.ListenMulticastUDP("udp4", nil, multicastGroup); err == nil {
			conns = append(conns, c)
		}
		return conns
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 {
			continue
		}
		if c, err := net.ListenMulticastUDP("udp4", &ifc, multicastGroup); err == nil {
			conns = append(conns, c)
		}
		if c, err := net.ListenMulticastUDP("udp6", &ifc, multicastGroup6); err == nil {
			conns = append(conns, c)
		}
	}
	return conns
}

// multicast sends info to the discovery groups once on every interface that
// can multicast: from each IPv4 address, and once over IPv6 where the
// interface has an address for the local link. It fails only if no
// interface took the message.
func multicast(info Info, announce bool) error {
	info.Announce = &announce
	msg, err := json.Marshal(info)
	if err != nil {
		return err
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		// As in listenMulticast: where the interfaces cannot be listed,
		// the system picks the one the message leaves by.
		c, err := net.DialUDP("udp4", nil, multicastGroup)
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = c.Write(msg)
		return err
	}
	sent, errs := 0, []error{}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 {
			continue
		}
		send := func(network string, from, to *net.UDPAddr) {
			c, err := net.DialUDP(network, from, to)
			if err != nil {
				errs = append(errs, err)
				return
			}
			_, err = c.Write(msg)
			c.Close()
			if err != nil {
				errs = append(errs, err)
				return
			}
			sent++
		}
		addrs, _ := ifc.Addrs()
		linkLocal := false
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ipnet.IP.To4() == nil {
				linkLocal = linkLocal || ipnet.IP.IsLinkLocalUnicast()
				continue
			}
			// Sending from the interface's own address is what makes the
			// message leave by that interface.
			send("udp4", &net.UDPAddr{IP: ipnet.IP}, multicastGroup)
		}
		if linkLocal {
			// Over IPv6 the interface is named with the group instead.
			send("udp6", nil, &net.UDPAddr{IP: multicastGroup6.IP, Port: multicastGroup6.Port, Zone: ifc.Name})
		}
	}
	if sent == 0 {
		return errors.Join(append(errs, errors.New("no interface accepted the announcement"))...)
	}
	return nil
}
