//go:build js

package direct

// The browser's build: pion is a cover over the browser's RTCPeerConnection
// here. The browser has no settings for most of what pion.go sets: which
// interfaces it uses, how long it checks, mDNS, the limits of DTLS and
// SCTP and its logging are the browser's own.

import (
	"context"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// receiverCanWait: in a browser every message is its own event, handed to
// a new goroutine, and a channel cannot be told to stop. A callback that
// waited would let a later message pass it. So what arrives is kept until
// it is read, without a limit.
const receiverCanWait = false

const (
	// gatherQuiet: a browser does not always say that it found all its
	// addresses. The description is taken as complete when no new address
	// came for this long, once one from STUN is there (plan, section 11a).
	gatherQuiet = 1500 * time.Millisecond
	// gatherWait: with no address from STUN after this long, the
	// description goes with the addresses there are.
	gatherWait = 5 * time.Second
)

// newPeerConnection makes a peer connection. The limit is kept by the
// Peer's own timer alone.
func newPeerConnection(cfg Config, _ time.Duration) (*webrtc.PeerConnection, error) {
	return webrtc.NewAPI().NewPeerConnection(webrtc.Configuration{ICEServers: cfg.servers()})
}

// gathering must be called before the local description is set. The
// function it returns waits until the description has its addresses, by
// the rule above.
func gathering(pc *webrtc.PeerConnection, stun bool) func(ctx context.Context, done <-chan struct{}) error {
	var (
		mu       sync.Mutex
		any      bool // an address is there
		enough   bool // one of the kind waited for is there
		quiet    *time.Timer
		complete = make(chan struct{})
		once     sync.Once
	)
	finish := func() { once.Do(func() { close(complete) }) }
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		defer func() { recover() }() // the wait below then ends by its timer
		if c == nil {
			finish()
			return
		}
		mu.Lock()
		defer mu.Unlock()
		any = true
		if !stun || c.Typ == webrtc.ICECandidateTypeSrflx {
			enough = true
		}
		if !enough {
			return
		}
		if quiet != nil {
			quiet.Stop()
		}
		quiet = time.AfterFunc(gatherQuiet, finish)
	})
	return func(ctx context.Context, done <-chan struct{}) error {
		late := time.NewTimer(gatherWait)
		defer late.Stop()
		for {
			select {
			case <-complete:
				return nil
			case <-late.C:
				mu.Lock()
				ok := any
				mu.Unlock()
				if ok {
					return nil
				}
			case <-done:
				return ErrLost
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// prepare makes the browser hand messages over as bytes at once. Its
// other form, a Blob, is read in a second step, and messages could then
// arrive out of order.
func prepare(dc *webrtc.DataChannel) {
	dc.JSValue().Set("binaryType", "arraybuffer")
}
