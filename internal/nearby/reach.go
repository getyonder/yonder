package nearby

import (
	"context"
	"encoding/hex"
	"io"
	"time"
)

// The reach check. A device cannot test its own firewall, and one behind a
// firewall hears nothing from its network, so it asks a device it met here
// to try:
//
//	asker    -> reach:   these are my addresses on the local network
//	answerer -> reached: I could connect to you there, or I could not
//
// The answerer says yes only if what answered at an address proved to be
// the asker (see Client.Reach), so the answer tells nobody anything about
// another machine.

const (
	kindReach   = "reach"
	kindReached = "reached"

	// maxReachAddrs is the most addresses one request may name.
	maxReachAddrs = 4
	// reachEvery is how often one device may have a check made for it.
	reachEvery = 20 * time.Second
)

// AskReach asks a nearby device whether it can connect to this one at
// addrs ("192.168.1.20:53317"). It waits for the answer until ctx ends: an
// older Yonder, or one in a browser, never answers.
func (c *Client) AskReach(ctx context.Context, peerID string, addrs []string) (bool, error) {
	nonce := make([]byte, 8)
	if _, err := io.ReadFull(c.rand(), nonce); err != nil {
		return false, err
	}
	n := hex.EncodeToString(nonce)
	key, answer := peerID+"|"+n, make(chan bool, 1)
	c.mu.Lock()
	if c.asks == nil {
		c.asks = map[string]chan bool{}
	}
	c.asks[key] = answer
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.asks, key)
		c.mu.Unlock()
	}()
	if err := c.send(peerID, body{Kind: kindReach, Addrs: addrs, N: n}); err != nil {
		return false, err
	}
	select {
	case ok := <-answer:
		return ok, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// reach deals with one message of the reach check.
func (c *Client) reach(ctx context.Context, from Peer, b body) {
	switch b.Kind {
	case kindReach:
		if c.Reach == nil || len(b.Addrs) == 0 || len(b.Addrs) > maxReachAddrs {
			return
		}
		c.mu.Lock()
		if c.reaching || time.Since(c.reached[from.ID]) < reachEvery {
			c.mu.Unlock()
			return
		}
		if c.reached == nil {
			c.reached = map[string]time.Time{}
		}
		c.reaching, c.reached[from.ID] = true, time.Now()
		c.mu.Unlock()
		go func() {
			ok := c.Reach(ctx, from, b.Addrs)
			c.mu.Lock()
			c.reaching = false
			c.mu.Unlock()
			c.send(from.ID, body{Kind: kindReached, N: b.N, OK: ok})
		}()
	case kindReached:
		c.mu.Lock()
		answer := c.asks[from.ID+"|"+b.N]
		c.mu.Unlock()
		if answer != nil {
			select {
			case answer <- b.OK:
			default:
			}
		}
	}
}
