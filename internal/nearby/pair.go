package nearby

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// Pairing two nearby devices. The meeting point passes every message, so
// it must learn nothing useful from them and must not be able to put
// itself in the middle unnoticed:
//
//	asker    -> ask:  a commitment to its public key (a hash of it)
//	answerer -> key:  its public key
//	asker    -> open: its public key, which must fit the commitment
//
// Both now hold the same secret (X25519) and work out a six-digit code
// from it, which their owners compare. A meeting point that swapped the
// keys for its own shares a different secret with each device, so the codes
// differ. The commitment is what makes six digits enough: without it the
// meeting point could see one device's key and then try keys of its own
// until the two codes happened to match; with it, it has to choose before
// it has seen the key it would need. It can still ask the answerer again
// with a new key, so the answerer limits the attempts (see triesMax).
//
//	asker    -> link: the pairing link, encrypted under the secret, sent
//	                  once the asker's owner has said the codes match
//	either   -> no:   the pairing is off
//
// The answerer uses the link once its own owner has said yes, and from
// there it is the ordinary pairing exchange through the tunnel.

// body is a pairing message, carried in Message.Body.
type body struct {
	Kind   string `json:"k"`
	Commit []byte `json:"commit,omitempty"`
	Pub    []byte `json:"pub,omitempty"`
	Box    []byte `json:"box,omitempty"`
	// For the reach check (reach.go).
	Addrs []string `json:"addrs,omitempty"`
	N     string   `json:"n,omitempty"`
	OK    bool     `json:"ok,omitempty"`
}

// codeDigits is the length of the code; codeValues is how many codes there
// are.
const (
	codeDigits = 6
	codeValues = 1_000_000
)

const (
	kindAsk  = "ask"
	kindKey  = "key"
	kindOpen = "open"
	kindLink = "link"
	kindNo   = "no"
)

// The kinds the meeting point counts: see PairingKind.
const (
	PairingAsk  = kindAsk
	PairingLink = kindLink
)

// PairingKind is the kind of a pairing message, read from the body a device
// asks the meeting point to pass on; "" if the body is not one. It is all
// the meeting point reads of a body, to count the pairings that start
// (PairingAsk) and those whose link is sent (PairingLink).
func PairingKind(raw json.RawMessage) string {
	var b struct {
		Kind string `json:"k"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return ""
	}
	return b.Kind
}

// Session is one pairing with a nearby device, from the first message
// until the link has crossed or it is called off.
type Session struct {
	// Peer is the other device.
	Peer Peer
	// Code is the six digits both owners compare. Set once the session
	// is handed out.
	Code string
	// Asked is true on the device that was asked, false on the one that
	// asked.
	Asked bool

	client *Client
	priv   *ecdh.PrivateKey
	commit []byte        // answerer: what the asker committed to
	tried  time.Time     // answerer: when the request was counted
	key    []byte        // encrypts the link; nil until the code is known
	ready  chan struct{} // closed when Code is set
	link   chan string   // answerer: the link, once it arrives
	timer  *time.Timer

	once sync.Once
	done chan struct{}
	err  error
}

func (c *Client) newSession(peer Peer, asked bool) (*Session, error) {
	seed := make([]byte, 32)
	if _, err := io.ReadFull(c.rand(), seed); err != nil {
		return nil, err
	}
	priv, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		return nil, err
	}
	s := &Session{
		Peer: peer, Asked: asked, client: c, priv: priv,
		ready: make(chan struct{}), link: make(chan string, 1), done: make(chan struct{}),
	}
	s.timer = time.AfterFunc(sessionWait, func() { s.stop(ErrExpired) })
	return s, nil
}

// Pair asks a nearby device to pair and returns once both hold the code.
// The caller shows Code, and when its owner says it matches, sends the
// pairing link with SendLink; Close calls the pairing off.
func (c *Client) Pair(ctx context.Context, peerID string) (*Session, error) {
	c.mu.Lock()
	peer, ok := c.peers[peerID]
	if !ok {
		c.mu.Unlock()
		return nil, ErrGone
	}
	if c.session != nil {
		c.mu.Unlock()
		return nil, ErrBusy
	}
	s, err := c.newSession(peer, false)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	c.session = s
	c.mu.Unlock()

	if err := c.send(peer.ID, body{Kind: kindAsk, Commit: commitment(s.priv.PublicKey().Bytes())}); err != nil {
		s.end(err)
		return nil, err
	}
	select {
	case <-s.ready:
		return s, nil
	case <-s.done:
		return nil, s.err
	case <-ctx.Done():
		s.Close()
		return nil, ctx.Err()
	}
}

// pairing deals with one pairing message from a nearby device.
func (c *Client) pairing(from Peer, b body) {
	c.mu.Lock()
	s := c.session
	if s != nil && s.Peer.ID != from.ID {
		s = nil
	}
	// A new request, when free to take one and the limit allows it.
	if b.Kind == kindAsk && c.session == nil && c.Asked != nil && len(b.Commit) == sha256.Size && c.mayTry() {
		s, err := c.newSession(from, true)
		if err != nil {
			c.mu.Unlock()
			return
		}
		s.commit = b.Commit
		s.tried = c.tries[len(c.tries)-1]
		c.session = s
		c.mu.Unlock()
		if err := c.send(from.ID, body{Kind: kindKey, Pub: s.priv.PublicKey().Bytes()}); err != nil {
			s.end(err)
		}
		return
	}
	c.mu.Unlock()
	if s == nil {
		// Not from the device we are pairing with: say no to a request,
		// ignore the rest.
		if b.Kind == kindAsk {
			c.send(from.ID, body{Kind: kindNo})
		}
		return
	}

	switch {
	case b.Kind == kindNo:
		s.end(ErrRefused)
	case b.Kind == kindKey && !s.Asked && s.key == nil:
		mine := s.priv.PublicKey().Bytes()
		if err := s.agree(mine, b.Pub, b.Pub); err != nil {
			s.stop(ErrTampered)
			return
		}
		if err := c.send(from.ID, body{Kind: kindOpen, Pub: mine}); err != nil {
			s.end(err)
			return
		}
		close(s.ready)
	case b.Kind == kindOpen && s.Asked && s.key == nil:
		if subtle.ConstantTimeCompare(commitment(b.Pub), s.commit) != 1 {
			s.stop(ErrTampered)
			return
		}
		if err := s.agree(b.Pub, s.priv.PublicKey().Bytes(), b.Pub); err != nil {
			s.stop(ErrTampered)
			return
		}
		close(s.ready)
		c.giveBack(s.tried)
		go c.Asked(s)
	case b.Kind == kindLink && s.Asked && s.key != nil:
		link, err := s.unseal(b.Box)
		if err != nil {
			s.stop(ErrTampered)
			return
		}
		select {
		case s.link <- link:
		default: // a second link is not wanted
		}
	default:
		// Out of turn.
		s.stop(ErrTampered)
	}
}

// mayTry reports whether a new request may be taken, and if so counts it.
// Only one session runs at a time, so a meeting point cannot dodge the
// count by leaving attempts open. Called with c.mu held.
func (c *Client) mayTry() bool {
	now := c.clock()
	if now.Before(c.paused) {
		return false
	}
	kept := c.tries[:0]
	for _, t := range c.tries {
		if now.Sub(t) < triesWindow {
			kept = append(kept, t)
		}
	}
	c.tries = kept
	if len(c.tries) >= triesMax {
		c.paused = now.Add(triesPause)
		c.tries = nil
		return false
	}
	c.tries = append(c.tries, now)
	return true
}

// giveBack takes back the count of a request whose owner is now asked.
func (c *Client) giveBack(tried time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, t := range c.tries {
		if t.Equal(tried) {
			c.tries = append(c.tries[:i], c.tries[i+1:]...)
			return
		}
	}
}

func commitment(pub []byte) []byte {
	h := sha256.New()
	h.Write([]byte("yonder nearby commitment v1"))
	h.Write(pub)
	return h.Sum(nil)
}

// agree works out the secret with the other device's public key, and from
// it the link key and the code. asker and answerer are the two public keys
// in that order, the same on both devices.
func (s *Session) agree(asker, answerer, theirs []byte) error {
	pub, err := ecdh.X25519().NewPublicKey(theirs)
	if err != nil {
		return err
	}
	secret, err := s.priv.ECDH(pub)
	if err != nil {
		return err
	}
	info := "yonder nearby v1" + string(asker) + string(answerer)
	out, err := hkdf.Key(sha256.New, secret, nil, info, 40)
	if err != nil {
		return err
	}
	s.Code = fmt.Sprintf("%0*d", codeDigits, binary.BigEndian.Uint64(out[32:])%codeValues)
	s.key = out[:32]
	return nil
}

func (s *Session) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func (s *Session) unseal(box []byte) (string, error) {
	aead, err := s.aead()
	if err != nil {
		return "", err
	}
	link, err := aead.Open(nil, nil, box, []byte(kindLink))
	return string(link), err
}

// SendLink gives the other device the pairing link, encrypted so that only
// it can read it. For the device that asked, once its owner has said the
// codes match.
func (s *Session) SendLink(link string) error {
	select {
	case <-s.done:
		return s.err
	default:
	}
	aead, err := s.aead()
	if err != nil {
		return err
	}
	return s.client.send(s.Peer.ID, body{Kind: kindLink, Box: aead.Seal(nil, nil, []byte(link), []byte(kindLink))})
}

// Link waits for the pairing link from the device that asked. Use it only
// once this device's owner has said the codes match.
func (s *Session) Link(ctx context.Context) (string, error) {
	select {
	case link := <-s.link:
		return link, nil
	case <-s.done:
		return "", s.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Done is closed when the session is over, for any reason.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err is why the session ended, or nil if this device closed it.
func (s *Session) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// Close calls the pairing off: unless the session was already over, the
// other device is told this one said no.
func (s *Session) Close() { s.stop(nil) }

// Finish ends a session whose work is done, without a word to the other
// device: on the answerer once it has the link, on the asker once the
// pairing it opened has been used.
func (s *Session) Finish() { s.end(nil) }

// stop ends the session from this side and tells the other device.
func (s *Session) stop(err error) {
	if s.end(err) {
		s.client.send(s.Peer.ID, body{Kind: kindNo})
	}
}

// end ends the session without telling the other device, and reports
// whether this call was the one that ended it.
func (s *Session) end(err error) (ended bool) {
	s.once.Do(func() {
		ended = true
		s.err = err
		s.timer.Stop()
		close(s.done)
		c := s.client
		c.mu.Lock()
		if c.session == s {
			c.session = nil
		}
		c.mu.Unlock()
	})
	return ended
}
