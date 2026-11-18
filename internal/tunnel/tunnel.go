// Package tunnel connects two devices on different networks. It is the only
// package that imports tailcat: tailcat makes no API stability promise, so
// the rest of Yonder sees just a listener to accept on and a dialler to
// connect with.
package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/wgengine/filter"

	"yonder/internal/address"
)

// Port is the one port served inside the tunnel. It is LocalSend's, since
// that is the protocol spoken through it.
const Port = 53317

// Relay is the main relay, as "host" or "host:port". It is always in the
// list of relays (see Relays), and this device's addresses point at it.
// Empty means Tailscale's public relays, which slow to a crawl after a few
// megabytes, and no list. A device's relay is part of its address, so
// changing this gives every device a new address; paired devices find it
// again by themselves (see Addr.Elsewhere, and app's reach).
var Relay = "relay.getyonder.org"

// ListURL is where the list of relays is published, in tailcat's own form
// (a "DERP map"), one region for each relay with its first node naming it,
// on port 443.
var ListURL = "https://" + address.Service + "/relays.json"

var (
	listMu sync.Mutex
	list   []string // from LoadRelays or UseRelays
)

// Relays is the list of relays, each written as Relay is, Relay first. An
// address on one of them is current; one on any other relay was given
// before that relay left the list. Until a list is loaded, the list built
// in is Relay alone.
func Relays() []string {
	if Relay == "" {
		return nil
	}
	listMu.Lock()
	defer listMu.Unlock()
	out := []string{Relay}
	for _, r := range list {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// UseRelays sets the list of relays, each written as Relay is. Relay is in
// it whatever names says.
func UseRelays(names []string) {
	listMu.Lock()
	defer listMu.Unlock()
	list = slices.Clone(names)
}

// ListCopy keeps the last list of relays fetched, between runs.
type ListCopy interface {
	// RelayList returns the copy kept and when it was kept, or ok false
	// if there is none.
	RelayList() (data []byte, kept time.Time, ok bool)
	// KeepRelayList replaces the copy.
	KeepRelayList(data []byte) error
}

// LoadRelays fetches the list of relays from ListURL, keeps it in keep, and
// uses it from then on; a list with no relay that can be used leaves the
// one built in. A copy kept less than an hour ago is used without
// asking the site, and an older one when the site cannot be reached. With
// neither, it returns an error and the list stays as it was.
func LoadRelays(ctx context.Context, keep ListCopy) error {
	dm, err := tailcat.FetchDERPMap(ctx, tailcat.DERPMapURL(ListURL), listCache{keep})
	if err != nil {
		return fmt.Errorf("relay list: %w", err)
	}
	var names []string
	for _, id := range slices.Sorted(maps.Keys(dm.Regions)) {
		r := dm.Regions[id]
		if r == nil || len(r.Nodes) == 0 {
			continue
		}
		// Yonder's relays take connections on 443, as relay.getyonder.org
		// does behind nginx. A broken or hostile list naming other ports
		// could point devices at any service on any host, so those
		// relays are dropped, and so are names that are not plain DNS names.
		if n := r.Nodes[0]; plainName(n.HostName) && (n.DERPPort == 0 || n.DERPPort == 443) {
			names = append(names, nameOf(n))
		}
		if len(names) == maxRelays {
			break
		}
	}
	UseRelays(names)
	if len(names) == 0 {
		return errors.New("relay list: no relay that can be used")
	}
	return nil
}

// plainName reports whether host is a plain DNS name: only letters, digits,
// "-" and ".", with a dot in it, and a last part that starts with a letter.
// So it is no address with a port, a path or a user, and no IP address in
// any spelling. The list from the site is held to it so that it cannot point
// devices at an address of its own choosing, such as one on the user's
// machine or network.
func plainName(host string) bool {
	for _, c := range host {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && (c < '0' || c > '9') && c != '-' && c != '.' {
			return false
		}
	}
	dot := strings.LastIndexByte(host, '.')
	last := host[dot+1:]
	return dot > 0 && last != "" && (last[0]|0x20 >= 'a' && last[0]|0x20 <= 'z')
}

// maxRelays is the most relays LoadRelays takes from the list, so a broken
// or hostile one cannot have a device try hundreds in turn.
const maxRelays = 16

// listCache is a ListCopy in the form tailcat wants. There is one list, so
// the URL is not kept, and nor is the server's ETag: the list is small
// enough to fetch again whole.
type listCache struct{ keep ListCopy }

func (c listCache) Get(string) ([]byte, string, time.Time, bool) {
	data, kept, ok := c.keep.RelayList()
	return data, "", kept, ok
}

func (c listCache) Put(_ string, data []byte, _ string) error { return c.keep.KeepRelayList(data) }

// relayRegion is Relay in the form tailcat wants, or nil for the public
// relays.
func relayRegion() (*tailcfg.DERPRegion, error) { return regionOf(Relay) }

// regionOf is a relay, written as Relay is, in the form tailcat wants; nil
// for the empty name.
func regionOf(relay string) (*tailcfg.DERPRegion, error) {
	if relay == "" {
		return nil, nil
	}
	node := &tailcfg.DERPNode{Name: "yonder", RegionID: relayRegionID, HostName: relay}
	if host, port, err := net.SplitHostPort(relay); err == nil {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("tunnel: relay %q has a bad port", relay)
		}
		node.HostName, node.DERPPort = host, int(n)
	}
	return &tailcfg.DERPRegion{RegionID: relayRegionID, RegionCode: "yonder", Nodes: []*tailcfg.DERPNode{node}}, nil
}

// relayRegionID is any number but zero, which tailcat refuses; it does not
// travel in the address.
const relayRegionID = 900

// Addr is a device's tunnel address. It is a secret: holding it is the
// first thing a client needs to connect.
type Addr string

// nameOf is a relay node written as Relay is: the opposite of regionOf.
func nameOf(n *tailcfg.DERPNode) string {
	if n.DERPPort == 0 {
		return n.HostName
	}
	return net.JoinHostPort(n.HostName, strconv.Itoa(n.DERPPort))
}

// relays is the relays the address points at, each written as Relay is;
// none if it cannot be read.
func (a Addr) relays() []string {
	info, err := tailcat.ParseAddr(tailcat.Addr(a))
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range info.Region {
		for _, n := range r.Nodes {
			out = append(out, nameOf(n))
		}
	}
	return out
}

// Valid reports whether a is an address that can be read.
func (a Addr) Valid() bool {
	_, err := tailcat.ParseAddr(tailcat.Addr(a))
	return err == nil
}

// Listed reports whether a is an address that can be read and points at a
// relay in the list (see Relays), exactly as Yonder writes it. An address
// from a device that is not yet trusted, such as the one in a pairing
// link, is dialled only if so: its relay is part of it, and it could name
// any host. The name alone is not compared, because an address can also
// say at which IP address its relay is and which certificate it shows:
// the whole address must be the one On makes for a relay in the list.
func (a Addr) Listed() bool {
	if !a.Valid() {
		return false
	}
	listed := Relays()
	if len(listed) == 0 {
		// With no relay of our own, an address names none either.
		return len(a.relays()) == 0
	}
	for _, r := range listed {
		if on, err := a.On(r); err == nil && on == a {
			return true
		}
	}
	return false
}

// Elsewhere is where the device behind a is found once it has moved to
// another relay in the list: the same keys, pointing at each of the others
// in turn. It is empty for an address that cannot be read.
func (a Addr) Elsewhere() []Addr {
	on := a.relays()
	if len(on) == 0 {
		return nil
	}
	var out []Addr
	for _, r := range Relays() {
		if slices.Contains(on, r) {
			continue
		}
		if moved, err := a.On(r); err == nil {
			out = append(out, moved)
		}
	}
	return out
}

// MovedTo reports whether to is where the device behind a may have moved:
// an address with the same keys, on relays that are all in the list. A
// device that says where it is now is believed no further than that.
func (a Addr) MovedTo(to Addr) bool {
	from, err := tailcat.ParseAddr(tailcat.Addr(a))
	if err != nil {
		return false
	}
	now, err := tailcat.ParseAddr(tailcat.Addr(to))
	if err != nil {
		return false
	}
	if len(to.relays()) == 0 || !to.Listed() {
		return false
	}
	return from.ServerPublic == now.ServerPublic && from.ServerDiscoPublic == now.ServerDiscoPublic && from.PresharedKey == now.PresharedKey
}

// On is the address of the same device on another relay, written as Relay
// is: the same keys, pointing there.
func (a Addr) On(relay string) (Addr, error) {
	region, err := regionOf(relay)
	if err != nil {
		return "", err
	}
	if region == nil {
		return "", errors.New("tunnel: no relay named")
	}
	info, err := tailcat.ParseAddr(tailcat.Addr(a))
	if err != nil {
		return "", err
	}
	info.Region, info.RegionID = []*tailcfg.DERPRegion{region}, 0
	return Addr(info.Addr()), nil
}

// ClientID names the key a device connects with. It is not a secret; a
// server lists the ClientIDs it lets in.
type ClientID string

// Keys are a device's saved tunnel identity, in a form that can be written
// to disk as JSON. They are secret.
type Keys struct {
	// Server and PresharedKey, with Region, fix this device's address.
	Server       string `json:"server"`
	PresharedKey string `json:"presharedKey"`
	// Client is the key this device connects to others with.
	Client string `json:"client"`
	// Region is the relay the address points at, the device's home.
	// Listen fills it in on the first start, with the nearest relay in
	// the list, and again when the home has left the list; save the keys
	// again afterwards, or the address may change on the next start.
	Region json.RawMessage `json:"region,omitempty"`
}

// home is the relay the keys' address points at: the saved one while it is
// in the list, and otherwise, as on the first start, the nearest in the
// list. Keys saved before there was a list point at Relay, which stays in
// it. With no list (the public relays) it is "".
func (k *Keys) home(ctx context.Context) string {
	list := Relays()
	if len(list) == 0 {
		return ""
	}
	var saved tailcfg.DERPRegion
	if json.Unmarshal(k.Region, &saved) == nil && len(saved.Nodes) > 0 && slices.Contains(list, nameOf(saved.Nodes[0])) {
		return nameOf(saved.Nodes[0])
	}
	return nearest(ctx, list)
}

// nearest is the relay in list, which must not be empty, that answers
// soonest. With one relay, or when none can be measured, it is the first.
func nearest(ctx context.Context, list []string) string {
	if len(list) == 1 {
		return list[0]
	}
	dm := &tailcfg.DERPMap{Regions: map[tailcfg.DERPRegionID]*tailcfg.DERPRegion{}}
	for i, name := range list {
		if r, err := regionOf(name); err == nil {
			id := tailcfg.DERPRegionID(i + 1)
			r.RegionID, r.Nodes[0].RegionID = id, id
			dm.Regions[id] = r
		}
	}
	id, err := tailcat.PickBestRegion(ctx, dm)
	if err != nil || id == 0 {
		return list[0]
	}
	return list[id-1]
}

// NewKeys makes a fresh identity.
func NewKeys() *Keys {
	server, _ := key.NewNode().MarshalText()
	client, _ := key.NewNode().MarshalText()
	psk, _ := tailcat.NewPresharedKey().MarshalText()
	return &Keys{Server: string(server), PresharedKey: string(psk), Client: string(client)}
}

// ClientID is the name other devices list to let this one in.
func (k *Keys) ClientID() (ClientID, error) {
	var priv key.NodePrivate
	if err := priv.UnmarshalText([]byte(k.Client)); err != nil {
		return "", fmt.Errorf("client key: %w", err)
	}
	return ClientID(priv.Public().String()), nil
}

func quiet(string, ...any) {}

// Server is this device's end of the tunnel: other devices that hold its
// address, and that it allows, can connect to it.
type Server struct {
	ts *tailcat.Server
	ln net.Listener
}

// Listen starts a server. ctx bounds the startup, which reaches a relay
// over the internet.
//
// With keys, the server has the saved address, on the device's home relay
// (see Keys.Region), and lets in only the allowed clients and the device
// itself. With nil keys, the server gets a fresh address that dies
// at Close and lets in whoever holds it, which is what pairing needs.
func Listen(ctx context.Context, keys *Keys, allowed []ClientID) (*Server, error) {
	// A fresh address, for pairing, is on the main relay.
	if keys == nil {
		return listen(ctx, Relay, nil, nil)
	}
	return listen(ctx, keys.home(ctx), keys, allowed)
}

// Start is Listen with keys, for when the program starts. With more than
// one relay in the list, it also checks that the home answers. If it does
// not and the nearest other relay does, that relay becomes the home, and
// Keys.Region changes with it; a device does not move back by itself. On
// an error the keys are left as they were. A running program that finds
// its home silent closes the server and calls Start again (app.CheckHome).
func Start(ctx context.Context, keys *Keys, allowed []ClientID) (*Server, error) {
	saved := keys.Region
	home := keys.home(ctx)
	s, err := listen(ctx, home, keys, allowed)
	others := slices.DeleteFunc(Relays(), func(r string) bool { return r == home })
	if len(others) == 0 {
		return s, err
	}
	if err == nil {
		if s.Answers(ctx, keys) {
			return s, nil
		}
		s.Close()
	}
	// The home does not answer: move to the nearest of the others, if it
	// does. If it does not either, the fault is likely ours, and the home
	// stays.
	if moved, err := listen(ctx, nearest(ctx, others), keys, allowed); err == nil {
		if moved.Answers(ctx, keys) {
			return moved, nil
		}
		moved.Close()
	}
	s, err = listen(ctx, home, keys, allowed)
	if err != nil {
		keys.Region = saved
	}
	return s, err
}

// listen is Listen on the given relay, and keys.Region follows it.
func listen(ctx context.Context, relay string, keys *Keys, allowed []ClientID) (*Server, error) {
	region, err := regionOf(relay)
	if err != nil {
		return nil, err
	}
	ts := &tailcat.Server{
		Logf: quiet,
		// Nothing but the listener's port is let through.
		ServedTCPPorts: []filter.PortRange{},
		Region:         region,
	}
	if keys != nil {
		if err := ts.Key.UnmarshalText([]byte(keys.Server)); err != nil {
			return nil, fmt.Errorf("server key: %w", err)
		}
		if err := ts.PresharedKey.UnmarshalText([]byte(keys.PresharedKey)); err != nil {
			return nil, fmt.Errorf("pre-shared key: %w", err)
		}
		// With the public relays, the saved region is the one picked on
		// the first start.
		if region == nil && len(keys.Region) > 0 {
			ts.Region = new(tailcfg.DERPRegion)
			if err := json.Unmarshal(keys.Region, ts.Region); err != nil {
				return nil, fmt.Errorf("saved relay region: %w", err)
			}
		}
		// The device itself is let in, so that answers can ask, and so
		// the list is never empty, which tailcat would read as "everyone".
		self, err := keys.ClientID()
		if err != nil {
			return nil, err
		}
		for _, id := range slices.Concat(allowed, []ClientID{self}) {
			pub, err := parseClientID(id)
			if err != nil {
				return nil, err
			}
			ts.AllowedClients = append(ts.AllowedClients, pub)
		}
	}
	ln, err := ts.Listen(ctx, "tcp", fmt.Sprintf(":%d", Port))
	if err != nil {
		ts.Close()
		return nil, fmt.Errorf("start tunnel: %w", err)
	}
	if keys != nil {
		if info, err := tailcat.ParseAddr(ts.TailcatAddr()); err == nil && len(info.Region) > 0 {
			if now, _ := json.Marshal(info.Region[0]); !bytes.Equal(now, keys.Region) {
				keys.Region = now
			}
		}
	}
	return &Server{ts: ts, ln: ln}, nil
}

// Answers reports whether the server can be reached through its relay,
// by asking it as the device itself, which it lets in.
func (s *Server) Answers(ctx context.Context, keys *Keys) bool {
	cl, err := Connect(s.Addr(), keys)
	if err != nil {
		return false
	}
	defer cl.Close()
	return cl.Ping(ctx) == nil
}

func parseClientID(id ClientID) (key.NodePublic, error) {
	var pub key.NodePublic
	if err := pub.UnmarshalText([]byte(id)); err != nil || pub.IsZero() {
		return pub, fmt.Errorf("tunnel: %q is not a client id", id)
	}
	return pub, nil
}

// Addr is the address to give to devices that should be able to connect.
func (s *Server) Addr() Addr { return Addr(s.ts.TailcatAddr()) }

// Listener accepts the connections that arrive through the tunnel.
func (s *Server) Listener() net.Listener { return s.ln }

// Allow lets one more client in, without a restart. There is no way to
// take a client out again short of starting a new server without it.
func (s *Server) Allow(id ClientID) error {
	pub, err := parseClientID(id)
	if err != nil {
		return err
	}
	s.ts.AddAllowedClient(pub)
	return nil
}

// PeerID says which client is behind a connection accepted from Listener,
// given the connection's own addresses. The tunnel has already checked the
// client holds that key. It returns "" if the connection is not one of ours.
func (s *Server) PeerID(local, remote net.Addr) ClientID {
	for _, kv := range s.ts.PeerEnv(local, remote) {
		if v, ok := strings.CutPrefix(kv, "TAILCAT_PEER_KEY="); ok {
			return ClientID(v)
		}
	}
	return ""
}

// Path is the way traffic with another device is travelling.
type Path int

const (
	// PathUnknown: the device is not connected, or did not say.
	PathUnknown Path = iota
	// PathDirect: straight between the two devices.
	PathDirect
	// PathRelayed: through the relay, which is slow, and whose traffic
	// Yonder's server pays for.
	PathRelayed
)

// Path is the way traffic with a connected client is travelling.
func (s *Server) Path(id ClientID) Path {
	k, err := parseClientID(id)
	if err != nil {
		return PathUnknown
	}
	peer := s.ts.Status().Peer[k]
	switch {
	case peer == nil:
		return PathUnknown
	case peer.CurAddr != "":
		return PathDirect
	case peer.Relay != "":
		return PathRelayed
	}
	return PathUnknown
}

// Relayed reports whether traffic with a connected client is going through
// a relay rather than straight to it. It is false for a client that is not
// connected.
func (s *Server) Relayed(id ClientID) bool { return s.Path(id) == PathRelayed }

// Drain waits, until ctx ends, for connections in progress to finish, so
// that a reply just written reaches the other side before Close.
func (s *Server) Drain(ctx context.Context) error { return s.ts.DrainTCP(ctx) }

// Close stops the server.
func (s *Server) Close() error { return s.ts.Close() }

// Client reaches one device through the tunnel. The connection is set up on
// first use and kept until Close.
type Client struct {
	tc *tailcat.Client
}

// Connect prepares a client for the device at addr. It does not touch the
// network yet. With keys, the client connects as this device, which a
// server with an allow list requires; with nil keys it is anonymous.
func Connect(addr Addr, keys *Keys) (*Client, error) {
	if _, err := tailcat.ParseAddr(tailcat.Addr(addr)); err != nil {
		return nil, fmt.Errorf("not a tunnel address: %w", err)
	}
	tc := &tailcat.Client{Server: tailcat.Addr(addr), Logf: quiet}
	if keys != nil {
		if err := tc.Key.UnmarshalText([]byte(keys.Client)); err != nil {
			return nil, fmt.Errorf("client key: %w", err)
		}
	}
	return &Client{tc: tc}, nil
}

// DialContext opens a connection to the device. It has the shape of a
// network dialler so an HTTP client can use it; the network and address
// arguments are ignored, because a client reaches exactly one device.
func (c *Client) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	return c.tc.DialTCPPort(ctx, Port)
}

// Ping reports whether the device answers. A device that is offline and one
// that does not allow this client look the same: no answer within about ten
// seconds, or by the time ctx ends.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.tc.Ping(ctx)
	return err
}

// Path is the way traffic to the device is travelling. It asks the device,
// so it takes a moment, and is PathUnknown if there is no answer.
func (c *Client) Path(ctx context.Context) Path {
	res, err := c.tc.DiscoPing(ctx)
	switch {
	case err != nil || res == nil || res.Err != "":
		return PathUnknown
	case res.Endpoint == "":
		return PathRelayed
	}
	return PathDirect
}

// Relayed reports whether traffic to the device is going through a relay
// rather than straight to it. It is false if there is no answer.
func (c *Client) Relayed(ctx context.Context) bool { return c.Path(ctx) == PathRelayed }

// Close ends the connection to the device.
func (c *Client) Close() error { return c.tc.Close() }
