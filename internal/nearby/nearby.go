package nearby

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"

	"yonder/internal/names"
)

// Reasons a pairing through the meeting point ends early.
var (
	ErrRefused = errors.New("the other device said no")
	ErrGone    = errors.New("the other device is no longer nearby")
	ErrBusy    = errors.New("a pairing with a nearby device is already under way")
	ErrOffline = errors.New("not connected to the meeting point")
	ErrExpired = errors.New("the pairing took too long")
	// ErrTampered means a message did not fit the key agreement: a fault,
	// or something between the two devices changing what they send.
	ErrTampered = errors.New("the pairing messages were tampered with")
)

const (
	// writeWait is how long one message may take to send.
	writeWait = 10 * time.Second
	// retryFirst and retryMost bound the wait before connecting again,
	// which doubles after each failure.
	retryFirst = 2 * time.Second
	retryMost  = time.Minute
	// sessionWait is how long a pairing may take from the first message,
	// the owners' looking at the codes included.
	sessionWait = 3 * time.Minute
)

// The limit on pairing requests, on the device that is asked. A meeting
// point that is not honest can ask again and again with keys of its own,
// calling each attempt off before the owner is asked, until the code it
// would share with this device is the one it already shares with the
// asker. So every request counts from the moment it is taken; the count is
// given back only when the owner is asked. After triesMax counted requests
// within triesWindow, every request is refused for triesPause. With six
// digits that leaves the meeting point a few chances in a million for
// each pairing.
//
// The count is for the device as a whole, not for each sender, because
// the meeting point chooses what a request says about its sender. Such a
// meeting point can use the limit to stop honest pairing for a while; that
// is accepted, as it can stop it anyway by not passing the messages.
const (
	triesMax    = 3
	triesWindow = time.Minute
	triesPause  = 5 * time.Minute
)

// Client is one device's place at a meeting point. Set the exported fields,
// then call Run.
type Client struct {
	// URL is the meeting point, "wss://host/nearby".
	URL string
	// Name and Kind are how this device appears to the others, and
	// Version its version of Yonder as one Yonder tells another.
	Name, Kind string
	Version    string
	// Fingerprint is this device's certificate fingerprint. It is not
	// sent; see Peer.Tag.
	Fingerprint string
	// Changed, if set, is called when the devices nearby have changed.
	Changed func()
	// Asked, if set, is called when a nearby device wants to pair and the
	// code to compare is ready. Without it every request is refused.
	Asked func(*Session)
	// Reach, if set, answers a nearby device that asks whether it can be
	// reached on the local network: it connects to the addresses and says
	// whether the asker itself answered at one. See AskReach.
	Reach func(ctx context.Context, from Peer, addrs []string) bool

	// random is where keys and salts come from; tests replace it.
	random io.Reader
	// now is the clock for the limit on requests; tests replace it.
	now func() time.Time

	mu       sync.Mutex
	conn     *websocket.Conn
	peers    map[string]Peer
	session  *Session
	asks     map[string]chan bool // AskReach calls waiting, by peer and nonce
	reached  map[string]time.Time // when each peer last had Reach run for it
	reaching bool
	tries    []time.Time // requests taken and not given back (pair.go)
	paused   time.Time   // every request is refused until then
	// For a client that is in a room, not among the devices nearby
	// (room.go): what its hello asks for, the code its welcome gave, and
	// welcomed, closed when that welcome has come.
	room     string
	code     string
	welcomed chan struct{}
}

// Run stays connected to the meeting point until ctx ends, connecting
// again whenever the connection is lost.
func (c *Client) Run(ctx context.Context) {
	wait := retryFirst
	for {
		start := time.Now()
		c.connect(ctx)
		if ctx.Err() != nil {
			return
		}
		// A connection that lasted was a good one: start over.
		if time.Since(start) > retryMost {
			wait = retryFirst
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, retryMost)
	}
}

// connect holds one connection until it ends, and says why it ended.
func (c *Client) connect(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dialCtx, dialDone := context.WithTimeout(ctx, writeWait)
	conn, _, err := websocket.Dial(dialCtx, c.URL, nil)
	dialDone()
	if err != nil {
		return ErrOffline
	}
	// Closed as "normal": a browser refuses to close a connection with any
	// of the other standard codes, which the library's own closing uses,
	// and would leave this one open. Not waited for, as the other side may
	// be slow to answer or gone.
	defer func() { go conn.Close(websocket.StatusNormalClosure, "") }()
	conn.SetReadLimit(MaxMessage)

	salt := make([]byte, 16)
	if _, err := io.ReadFull(c.rand(), salt); err != nil {
		return err
	}
	hello := Message{Type: TypeHello, Room: c.room, Peer: &Peer{
		Name: c.Name, Kind: c.Kind, Version: c.Version, Salt: hex.EncodeToString(salt), Tag: tag(salt, c.Fingerprint),
	}}
	if write(ctx, conn, hello) != nil {
		return ErrOffline
	}
	defer c.disconnected()
	for {
		readCtx, readDone := context.WithTimeout(ctx, Quiet)
		typ, b, err := conn.Read(readCtx)
		readDone()
		if err != nil {
			return ErrOffline
		}
		var msg Message
		if typ != websocket.MessageText || json.Unmarshal(b, &msg) != nil {
			return ErrOffline
		}
		if err := c.handle(ctx, conn, msg); err != nil {
			return err
		}
	}
}

// byeBecause turns the reason the meeting point gave for ending the
// connection into an error.
func byeBecause(reason string) error {
	switch reason {
	case ReasonNoRoom:
		return ErrNoRoom
	case ReasonBusy:
		return ErrTooMany
	case ReasonFull:
		return ErrFull
	}
	return ErrOffline
}

func (c *Client) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Client) rand() io.Reader {
	if c.random != nil {
		return c.random
	}
	return rand.Reader
}

// disconnected clears what the lost connection had: the devices nearby and
// any pairing going through it.
func (c *Client) disconnected() {
	c.mu.Lock()
	c.conn, c.peers, c.reached = nil, nil, nil
	s := c.session
	c.mu.Unlock()
	if s != nil {
		s.end(ErrOffline)
	}
	c.changed()
}

func (c *Client) changed() {
	if c.Changed != nil {
		c.Changed()
	}
}

// handle deals with one message from the meeting point. An error means
// the connection is to end.
func (c *Client) handle(ctx context.Context, conn *websocket.Conn, msg Message) error {
	switch msg.Type {
	case TypePing:
		write(ctx, conn, Message{Type: TypePong})
	case TypeBye:
		return byeBecause(msg.Reason)
	case TypeWelcome:
		if c.room != "" {
			// A meeting point from before rooms has put this device among
			// the devices nearby: leave at once.
			if msg.Room == "" {
				return ErrOldServer
			}
			if c.code != "" {
				return ErrOffline // welcomed twice
			}
		}
		c.mu.Lock()
		c.code = msg.Room
		c.conn = conn
		c.peers = map[string]Peer{}
		for _, p := range msg.Peers {
			p.Name = names.Clean(p.Name)
			c.peers[p.ID] = p
		}
		c.mu.Unlock()
		if c.room != "" {
			close(c.welcomed)
		}
		c.changed()
	case TypeJoin:
		if msg.Peer == nil {
			return nil
		}
		c.mu.Lock()
		if c.peers != nil {
			// The meeting point passes names on; it is not trusted to
			// have cleaned them.
			msg.Peer.Name = names.Clean(msg.Peer.Name)
			c.peers[msg.Peer.ID] = *msg.Peer
		}
		c.mu.Unlock()
		c.changed()
	case TypeLeave:
		c.mu.Lock()
		delete(c.peers, msg.ID)
		delete(c.reached, msg.ID)
		s := c.session
		c.mu.Unlock()
		if s != nil && s.Peer.ID == msg.ID {
			s.end(ErrGone)
		}
		c.changed()
		if c.room != "" {
			return ErrGone // a room ends when either device leaves
		}
	case TypeFrom:
		c.mu.Lock()
		from, known := c.peers[msg.ID]
		c.mu.Unlock()
		var body body
		if !known || json.Unmarshal(msg.Body, &body) != nil {
			return nil
		}
		switch body.Kind {
		case kindReach, kindReached:
			c.reach(ctx, from, body)
		default:
			c.pairing(from, body)
		}
	}
	return nil
}

// Connected reports whether this device is at the meeting point now.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Peers lists the devices nearby, by name.
func (c *Client) Peers() []Peer {
	c.mu.Lock()
	defer c.mu.Unlock()
	peers := make([]Peer, 0, len(c.peers))
	for _, p := range c.peers {
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].Name != peers[j].Name {
			return peers[i].Name < peers[j].Name
		}
		return peers[i].ID < peers[j].ID
	})
	return peers
}

// Is reports whether a nearby device is the one with the given certificate
// fingerprint: one this device is paired with, or sees on its own network.
func (p Peer) Is(fingerprint string) bool {
	salt, err := hex.DecodeString(p.Salt)
	return err == nil && fingerprint != "" && p.Tag == tag(salt, fingerprint)
}

func tag(salt []byte, fingerprint string) string {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(fingerprint))
	return hex.EncodeToString(h.Sum(nil))
}

// send passes a pairing message to another device through the meeting
// point.
func (c *Client) send(to string, b body) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return ErrOffline
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return write(context.Background(), conn, Message{Type: TypeTo, ID: to, Body: raw})
}

func write(ctx context.Context, conn *websocket.Conn, msg Message) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, writeWait)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, b)
}
