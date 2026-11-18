package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"yonder/internal/localsend"
	"yonder/internal/pairing"
	"yonder/internal/tunnel"
)

// This file holds one rule: two Yonder apps never move files through the
// relay to each other. The relay only helps them find a direct path; if
// none forms, the transfer does not happen, and one that loses its direct
// path is stopped. A transfer with a web browser goes over a data channel
// (channel.go) if one forms, and is refused if not (relayFallBack); sendWeb
// makes that choice. Sending (SendPaired) and receiving
// (AdmitOffer and source) both ask here.
//
// Two paired apps each keep their own connection to the other, and each
// connection finds, or fails to find, its own direct path. So a send looks
// at both: the files go over the sender's connection if that one is direct
// (an upload, as LocalSend does it), and otherwise over the receiver's if
// that one is (the receiver fetches them; see localsend's fetch.go). Then
// nothing about the files goes through the relay, not even their names:
// only that a send is wanted, and the yes or no.

// ErrNoDirectPath means a transfer between two apps was not made, or was
// stopped, because it would have gone through the relay.
var ErrNoDirectPath = errors.New("the two networks would not connect directly, and Yonder does not pass files between apps through its server")

// TypeWeb is the LocalSend device type of Yonder in a browser, which is how
// a device tells a browser from an app: every device gives its type when
// asked who it is and with every offer. Anything else counts as an app,
// which covers versions from before the rule.
const TypeWeb = "web"

// What two apps say to each other in localsend.FetchHeader, beside what the
// localsend package says there itself.
const (
	// fetchCan, on the answer to a question: this device can fetch the
	// files of a send. Versions before 0.1.18 do not say it, and are sent
	// to only over the sender's connection.
	fetchCan = "1"
	// fetchLook, on a question: the asker is about to send, so look for a
	// direct path to it now.
	fetchLook = "look"
)

// mayRelay is the rule: only a transfer with a browser at one end or the
// other may use the relay. The arguments are the two devices' types.
func mayRelay(mine, theirs string) bool { return mine == TypeWeb || theirs == TypeWeb }

const (
	// directWait is how long a transfer waits for a direct path, which
	// two devices that can have one find within a few seconds of the
	// first traffic between them.
	directWait = 10 * time.Second
	// directEvery is how often the path is looked at while waiting.
	directEvery = time.Second
	// guardEvery is how often a transfer under way has its path looked
	// at, and guardStrikes how many times in a row it must be on the
	// relay to be stopped: once can be a path changing over.
	guardEvery   = 3 * time.Second
	guardStrikes = 2
)

// pathRule is the rule's timing, and where it learns a path; tests replace
// both.
type pathRule struct {
	path                func(ctx context.Context, d pairing.Device, outgoing bool) tunnel.Path
	wait, every, guards time.Duration
}

// awaitEither waits for a direct path to a device on either of the two
// connections, both looked at over the one wait, and reports which is
// direct: this device's own (outgoing) or the other device's. This device's
// own wins when both are.
func (r pathRule) awaitEither(ctx context.Context, d pairing.Device) (outgoing, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, r.wait)
	defer cancel()
	// Each connection has its own looks, so that one slow to say does not
	// keep the other from being seen.
	ours, theirs := make(chan bool, 1), make(chan bool, 1)
	go func() { ours <- r.awaitDirect(ctx, d, true) }()
	go func() { theirs <- r.awaitDirect(ctx, d, false) }()
	select {
	case ok := <-ours:
		if ok {
			return true, true
		}
		return false, <-theirs
	case ok := <-theirs:
		if !ok {
			return true, <-ours
		}
		// Theirs is direct, and is seen at once where ours takes an
		// answer from the other device: one more look at ours.
		lctx, stop := context.WithTimeout(ctx, r.every)
		defer stop()
		return r.path(lctx, d, true) == tunnel.PathDirect, true
	}
}

// awaitDirect waits for the path to a device to be direct, and reports
// whether it became so in time.
func (r pathRule) awaitDirect(ctx context.Context, d pairing.Device, outgoing bool) bool {
	ctx, cancel := context.WithTimeout(ctx, r.wait)
	defer cancel()
	tick := time.NewTicker(r.every)
	defer tick.Stop()
	for {
		// Each look has its own limit: the tunnel's answer to one can be
		// lost, and then nothing ends that look but its ctx. Without a
		// limit, one lost answer took up the whole wait (seen 2026-11-16).
		lctx, stop := context.WithTimeout(ctx, 2*r.every)
		path := r.path(lctx, d, outgoing)
		stop()
		if path == tunnel.PathDirect {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

// guard watches a transfer's path until ctx ends, and calls dropped, once,
// if the path has gone to the relay and stays there. A path that cannot be
// learned is not held against the transfer: a busy connection can be slow
// to say.
func (r pathRule) guard(ctx context.Context, d pairing.Device, outgoing bool, dropped func()) {
	tick := time.NewTicker(r.guards)
	defer tick.Stop()
	strikes := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		actx, cancel := context.WithTimeout(ctx, r.guards)
		path := r.path(actx, d, outgoing)
		cancel()
		switch path {
		case tunnel.PathRelayed:
			if strikes++; strikes >= guardStrikes && ctx.Err() == nil {
				dropped()
				return
			}
		case tunnel.PathDirect:
			strikes = 0
		}
	}
}

// path is the way traffic with a paired device is travelling. outgoing says
// who connected: true when this device reached out to it, false when it
// reached in to us.
func (n *Node) path(ctx context.Context, d pairing.Device, outgoing bool) tunnel.Path {
	n.mu.Lock()
	tun, cl := n.tun, n.clients[d.Fingerprint]
	n.mu.Unlock()
	switch {
	case outgoing && cl != nil:
		return cl.Path(ctx)
	case !outgoing && tun != nil:
		return tun.Path(d.ClientID)
	}
	return tunnel.PathUnknown
}

// met records whether transfers with a paired device may use the relay,
// from what kind of device it says it is.
func (n *Node) met(fingerprint, deviceType string) {
	n.mu.Lock()
	n.web[fingerprint] = mayRelay(n.Info.DeviceType, deviceType)
	n.mu.Unlock()
}

// relayOK reports whether a transfer with a paired device may use the
// relay. Until the device has said what it is, only a browser's may.
func (n *Node) relayOK(d pairing.Device) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.web[d.Fingerprint] || mayRelay(n.Info.DeviceType, "")
}

// AdmitOffer applies the rule to an offer, given the ctx passed to
// Receiver.Accept: an offer that came through the tunnel from another app
// is admitted only once the path is direct, which it gets a few seconds to
// become. Offers from the local network and from browsers are admitted as
// they are.
//
// An uploaded offer has no guard at this end (from 0.1.22): the files come
// over the sender's connection, which the sender's guard watches, and this
// device's way back to the sender carries only acknowledgements, so its
// going to the relay says nothing about the files.
//
// An offer whose files this device fetches was held to the rule before the
// list of its files was asked for (see source). For such an offer a guard
// is returned to run while the files arrive: it watches this device's own
// connection, which is the one the files arrive on, until its ctx ends, and
// calls dropped if the path goes to the relay, when the transfer is to be
// stopped.
func (n *Node) AdmitOffer(ctx context.Context, o localsend.Offer) (ok bool, guard func(ctx context.Context, dropped func())) {
	if d, channel, paired := n.channelSender(ctx); channel {
		// A data channel carries uploads between a browser and a device
		// paired with it, and nothing else.
		if !paired || o.Fetch != "" {
			return false, nil
		}
		n.met(d.Fingerprint, o.Sender.DeviceType)
		ok := n.relayOK(d)
		n.log.logf("receive from %q: path: data channel, admitted: %v", localsend.Clip(d.Name), ok)
		return ok, nil
	}
	d, tunnelled := n.tunnelSender(ctx)
	if !tunnelled {
		return true, nil
	}
	if o.Fetch == "" {
		n.met(d.Fingerprint, o.Sender.DeviceType)
		if n.relayOK(d) {
			// A browser is at one end, and the files would come through
			// the relay.
			if !n.dc.relay {
				n.log.logf("receive from %q: path: relay, refused, because %s", localsend.Clip(d.Name), saidWhy(ctx))
				return false, nil
			}
			n.log.logf("receive from %q: path: relay, because %s", localsend.Clip(d.Name), saidWhy(ctx))
			return true, nil
		}
		return n.rule.awaitDirect(ctx, d, false), nil
	}
	return true, func(ctx context.Context, dropped func()) {
		n.rule.guard(ctx, d, true, func() {
			n.log.logf("receive from %q: the path went to the relay, stopping", localsend.Clip(d.Name))
			dropped()
		})
	}
}

// relayedKey holds, in a request's ctx, what the request said in
// relayedHeader.
type relayedKey struct{}

// saidWhy is why the sender of an offer that came through the relay says no
// data channel carries it, for the send log.
func saidWhy(ctx context.Context) string {
	said, _ := ctx.Value(relayedKey{}).(string)
	switch text, known := whyText[said]; {
	case known:
		return "the sender says " + text
	case said == "":
		return "the sender did not try a data channel (its Yonder is older)"
	}
	return "the sender gives a reason not known to this version"
}

// sendWeb is SendPaired with a browser at one end, and the one place that
// chooses between a data channel and the relay. A channel gets the wait to
// form. If it does, every connection of the send is one through it, and a
// channel that fails then fails the send, as a lost connection does: the
// path is not changed in the middle. If none forms, the send is refused
// with ErrNoChannel or ErrOldDevice and the send log says why; where the
// rule allows the relay, it goes through the tunnel, which for a browser is
// the relay, and the send log and the offer say why.
func (n *Node) sendWeb(ctx context.Context, sender *localsend.Sender, to localsend.Peer, d pairing.Device, paths []string) (int, error) {
	name := localsend.Clip(d.Name)
	start := time.Now()
	tellWay(ctx, WayConnecting)
	p, why, err := n.openChannel(ctx, sender, to)
	if p == nil {
		if cerr := ctx.Err(); cerr != nil {
			return 0, cerr
		}
		if !n.dc.relay {
			n.log.logf("send to %q: path: none, refused, because %s, after %v: %v", name, whyText[why], since(start), err)
			if why == whyOld {
				return 0, ErrOldDevice
			}
			return 0, ErrNoChannel
		}
		n.log.logf("send to %q: path: relay, because %s, after %v: %v", name, whyText[why], since(start), err)
		sender.OfferHeader.Set(relayedHeader, why)
		tellWay(ctx, WayRelay)
		return sender.Send(ctx, to, paths)
	}
	defer p.Close()
	n.mu.Lock()
	n.chans.sending[d.Fingerprint]++
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		n.chans.sending[d.Fingerprint]--
		n.mu.Unlock()
	}()
	n.log.logf("send to %q: path: data channel after %v (%s)", name, since(start), pathText(p))
	tellWay(ctx, WayChannel)
	over := *sender
	over.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) { return p.Dial(ctx) }
	return over.Send(ctx, to, paths)
}

// sendDirect is SendPaired to another app: the files go over whichever of
// the two connections is direct, or not at all.
func (n *Node) sendDirect(ctx context.Context, sender *localsend.Sender, to localsend.Peer, d pairing.Device, paths []string) (int, error) {
	n.mu.Lock()
	either := n.fetches[d.Fingerprint]
	n.mu.Unlock()
	// A device that is not reachable itself cannot be fetched from: the
	// terminal's send, which opens no tunnel.
	either = either && n.listening()
	outgoing, direct := true, false
	start := time.Now()
	if either {
		// The other device looks for a direct path on its connection
		// while this one looks on its own. The question may go through
		// the relay: it says nothing about the files.
		go func() {
			lctx, cancel := context.WithTimeout(ctx, freshWait)
			defer cancel()
			sender.Question(lctx, to, http.Header{localsend.FetchHeader: {fetchLook}})
		}()
		outgoing, direct = n.rule.awaitEither(ctx, d)
	} else {
		direct = n.rule.awaitDirect(ctx, d, true)
	}
	if !direct {
		n.log.logf("send to %q: no direct path after %v (it can fetch: %v)", localsend.Clip(d.Name), since(start), either)
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		return 0, ErrNoDirectPath
	}
	if outgoing {
		n.log.logf("send to %q: our connection is direct after %v, uploading", localsend.Clip(d.Name), since(start))
	} else {
		n.log.logf("send to %q: its connection is direct after %v, it fetches", localsend.Clip(d.Name), since(start))
	}
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	var dropped atomic.Bool
	go n.rule.guard(sctx, d, outgoing, func() {
		n.log.logf("send to %q: the path went to the relay, stopping", localsend.Clip(d.Name))
		dropped.Store(true)
		stop()
	})
	var sent int
	var err error
	if outgoing {
		sent, err = sender.Send(sctx, to, paths)
	} else {
		sent, err = n.lend(sctx, sender, to, d, paths)
	}
	// The receiver's own look at its connection did not agree with ours:
	// no direct path either.
	if dropped.Load() || errors.Is(err, localsend.ErrFetchRefused) {
		err = ErrNoDirectPath
	}
	return sent, err
}

// lend offers files for a paired device to fetch over its connection to
// this one. The list of files and the files are given only to that device,
// through the tunnel, and only while its connection is direct.
func (n *Node) lend(ctx context.Context, sender *localsend.Sender, to localsend.Peer, d pairing.Device, paths []string) (int, error) {
	n.mu.Lock()
	n.lending[d.Fingerprint]++
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		n.lending[d.Fingerprint]--
		n.mu.Unlock()
	}()
	return sender.Lend(ctx, to, paths, n.lender, func(r *http.Request) bool {
		from, tunnelled := n.tunnelSender(r.Context())
		return tunnelled && from.Fingerprint == d.Fingerprint && n.rule.path(r.Context(), d, false) == tunnel.PathDirect
	})
}

// look looks for a direct path to a paired device that has said it is about
// to send, so that the path is there if the files have to be fetched. It
// returns at once; one look runs at a time for a device.
func (n *Node) look(d pairing.Device) {
	n.mu.Lock()
	skip := n.looking[d.Fingerprint] || n.sending[d.Fingerprint] > 0 || n.web[d.Fingerprint]
	if !skip {
		n.looking[d.Fingerprint] = true
	}
	n.mu.Unlock()
	if skip {
		return
	}
	go func() {
		defer func() {
			n.mu.Lock()
			delete(n.looking, d.Fingerprint)
			n.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(n.ctx, freshWait+n.rule.wait)
		defer cancel()
		start := time.Now()
		if _, err := n.reach(ctx, d); err != nil {
			n.log.logf("look: %q did not answer after %v", localsend.Clip(d.Name), since(start))
			return
		}
		n.log.logf("look: %q answered after %v", localsend.Clip(d.Name), since(start))
		direct := n.rule.awaitDirect(ctx, d, true)
		n.log.logf("look: our connection to %q direct: %v, after %v", localsend.Clip(d.Name), direct, since(start))
	}()
}

// source is the Receiver's Source: the way to the sender of an offer whose
// files this device is to fetch. There is one only to a paired device that
// offered through the tunnel, and only once this device's own connection to
// it is direct; the look that the sender asked for has normally found that
// already.
func (n *Node) source(ctx context.Context, o localsend.Offer) (*localsend.Source, error) {
	d, tunnelled := n.tunnelSender(ctx)
	if !tunnelled {
		return nil, errors.New("files are fetched only from a paired device")
	}
	n.met(d.Fingerprint, o.Sender.DeviceType)
	start := time.Now()
	n.log.logf("fetch from %q: offered", localsend.Clip(d.Name))
	cl, err := n.reach(ctx, d)
	if err != nil {
		n.log.logf("fetch from %q: it did not answer after %v", localsend.Clip(d.Name), since(start))
		return nil, err
	}
	// The fetch keeps the connection busy, as a send does.
	done := n.busy(d)
	if !n.rule.awaitDirect(ctx, d, true) {
		n.log.logf("fetch from %q: no direct path after %v", localsend.Clip(d.Name), since(start))
		done()
		return nil, ErrNoDirectPath
	}
	n.log.logf("fetch from %q: our connection is direct after %v", localsend.Clip(d.Name), since(start))
	sender, to := n.through(cl, d)
	return &localsend.Source{Sender: sender, Peer: to, Close: done}, nil
}
