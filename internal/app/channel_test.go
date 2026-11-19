package app

// Transfers with a browser at one end, over a data channel or, when none
// forms, through the relay. The tunnel is stood in for as in fetch_test.go,
// and the data channels are real ones over the loopback addresses.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"yonder/internal/direct"
	"yonder/internal/localsend"
	"yonder/internal/pairing"
	"yonder/internal/tunnel"
)

// end is one device of a channelPair, with its send log if it keeps one.
type end struct {
	*wired
	logDir string

	mu   sync.Mutex
	came []Way    // what OfferWay said of each offer it took
	by   []string // the fingerprint PairedSender gave for each
	told []Way    // the way each of its sends was told
	// connecting counts the sends that were told WayConnecting, and
	// early is whether one was told so after its way.
	connecting int
	early      bool
	// tunnelled is whether tunnelSender took any of those offers as come
	// through the tunnel itself.
	tunnelled bool
}

// newEnd starts a wired Node that is an app, or a browser, which keeps no
// send log. A data channel gets the whole of wait to form.
func newEnd(t *testing.T, name string, browser bool) *end {
	t.Helper()
	e := &end{}
	cfg := Config{Dir: t.TempDir(), SaveDir: t.TempDir(), Name: name, Listen: "127.0.0.1:0", Browser: browser}
	e.wired = wire(t, startNode(t, cfg))
	e.dc.wait = wait
	if !browser {
		e.logDir = cfg.Dir
	}
	accept := e.Receiver.Accept
	e.Receiver.Accept = func(ctx context.Context, o localsend.Offer) []string {
		ids := accept(ctx, o)
		d, _ := e.PairedSender(ctx)
		_, tunnelled := e.tunnelSender(ctx)
		e.mu.Lock()
		e.tunnelled = e.tunnelled || tunnelled
		e.came, e.by = append(e.came, e.OfferWay(ctx, o)), append(e.by, d.Fingerprint)
		e.mu.Unlock()
		return ids
	}
	return e
}

// sending is a ctx for a send that notes the way the send is told.
func (e *end) sending(ctx context.Context) context.Context {
	return WithWay(ctx, func(w Way) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if w == WayConnecting {
			e.connecting++
			e.early = e.early || len(e.told) >= e.connecting
			return
		}
		e.told = append(e.told, w)
	})
}

// trace is the end's send log so far.
func (e *end) trace(t *testing.T) string {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(e.logDir, TraceName))
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// traced waits for a line with want in the end's send log: the receiver
// writes some of its lines after the sender's send has returned.
func (e *end) traced(t *testing.T, want string) {
	t.Helper()
	if e.logDir == "" {
		return // a browser keeps no log
	}
	var got string
	defer func() {
		if t.Failed() {
			t.Logf("%s's send log:\n%s", e.Info.Alias, got)
		}
	}()
	until(t, "the line "+want+" in "+e.Info.Alias+"'s send log", func() bool {
		got = e.trace(t)
		return strings.Contains(got, want)
	})
}

func (e *end) noted() (ways []Way, from []string, told []Way) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Way(nil), e.came...), append([]string(nil), e.by...), append([]Way(nil), e.told...)
}

// channelPair is two paired devices, A which sends and B which receives,
// each an app or a browser, and B as A holds it.
func channelPair(t *testing.T, browserA, browserB bool) (a, b *end, toB pairing.Device) {
	t.Helper()
	a, b = newEnd(t, "A", browserA), newEnd(t, "B", browserB)
	toB = a.connectTo(b.wired)
	b.connectTo(a.wired)
	return a, b, toB
}

// released waits until an end holds none of the data channels it answered:
// each is closed when the sender closes its own after the send.
func released(t *testing.T, e *end) {
	t.Helper()
	until(t, e.Info.Alias+" to let go of the data channels it answered", func() bool {
		e.Node.mu.Lock()
		defer e.Node.mu.Unlock()
		for _, n := range e.chans.answering {
			if n != 0 {
				return false
			}
		}
		return len(e.chans.peers) == 0
	})
}

// overTunnel reports whether any part of an upload passed through A's
// stand-in tunnel connection to B.
func overTunnel(b *end) bool {
	said := b.said("A")
	return strings.Contains(said, "/prepare-upload") || strings.Contains(said, "/upload?")
}

// kinds is the two sends a browser takes part in that the tests run: it
// sends to an app, and an app sends to it.
var kinds = map[string]struct{ browserA, browserB bool }{
	"a browser sends to an app": {true, false},
	"an app sends to a browser": {false, true},
}

// A send with a browser at one end goes over a data channel: nothing of
// the upload passes through the tunnel, both ends say so to whoever shows
// the transfer, the send logs say so, and the receiver knows the sender as
// its paired device.
func TestSendWithABrowserGoesOverADataChannel(t *testing.T) {
	for name, kind := range kinds {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			a, b, toB := channelPair(t, kind.browserA, kind.browserB)
			if sent, err := a.SendPaired(a.sending(ctx), toB, []string{file(t, big)}); err != nil || sent != 1 {
				t.Fatalf("sent %d, err %v", sent, err)
			}
			arrived(t, b.wired, big)
			if overTunnel(b) {
				t.Fatalf("the upload passed through the tunnel:\n%s", b.said("A"))
			}
			if said := b.said("A"); !strings.Contains(said, "POST "+directPath) {
				t.Fatalf("A did not ask for a data channel through the tunnel:\n%s", said)
			}
			ways, from, _ := b.noted()
			_, _, told := a.noted()
			if len(ways) != 1 || ways[0] != WayChannel || len(told) != 1 || told[0] != WayChannel {
				t.Fatalf("B says the offer's way is %v and A was told %v; want one data channel each", ways, told)
			}
			a.mu.Lock()
			connecting, early := a.connecting, a.early
			a.mu.Unlock()
			if connecting != 1 || early {
				t.Fatalf("A was told that it connects %d times (after the way: %v); want once, before the way", connecting, early)
			}
			if from[0] != a.Info.Fingerprint {
				t.Fatalf("B took the offer as from %q, want its paired device A", from[0])
			}
			// What only the tunnel may carry (a new address, a lent file,
			// a fetch, a request for a channel) asks tunnelSender.
			if b.tunnelled {
				t.Fatal("B took an offer on a data channel as come through the tunnel itself")
			}
			a.traced(t, `send to "B": path: data channel after`)
			b.traced(t, `direct: data channel with "A" open after`)
			b.traced(t, `receive from "A": path: data channel, admitted: true`)
			released(t, b)
			b.traced(t, `direct: data channel with "A" closed after`)
		})
	}
}

// old makes an end answer as a Yonder from before data channels does.
func old(e *end) {
	e.srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hear(w, r)
		e.Receiver.ServeHTTP(w, r)
	})
}

// relaying lets ends fall back to the relay when no data channel forms,
// which a Yonder does not do by itself (relayFallBack): the path is kept
// and these tests hold it to what it did.
func relaying(ends ...*end) {
	for _, e := range ends {
		e.dc.relay = true
	}
}

// relayedBecause fails unless the send went whole through the tunnel, and
// both ends say so, with the reason, in every place they say it.
func relayedBecause(t *testing.T, a, b *end, word string) {
	t.Helper()
	arrived(t, b.wired, big)
	if !overTunnel(b) {
		t.Fatalf("the upload did not pass through the tunnel:\n%s", b.said("A"))
	}
	ways, _, _ := b.noted()
	_, _, told := a.noted()
	if len(ways) != 1 || ways[0] != WayRelay || len(told) != 1 || told[0] != WayRelay {
		t.Fatalf("B says the offer's way is %v and A was told %v; want the relay once each", ways, told)
	}
	a.traced(t, `send to "B": path: relay, because `+whyText[word])
	b.traced(t, `receive from "A": path: relay, because the sender says `+whyText[word])
}

// The other device answers 404 to the request, as a Yonder from before data
// channels does: the send goes through the relay, and the logs say why.
func TestOlderDeviceIsSentToThroughTheRelay(t *testing.T) {
	for name, kind := range kinds {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			a, b, toB := channelPair(t, kind.browserA, kind.browserB)
			relaying(a, b)
			old(b)
			if sent, err := a.SendPaired(a.sending(ctx), toB, []string{file(t, big)}); err != nil || sent != 1 {
				t.Fatalf("sent %d, err %v", sent, err)
			}
			arrived(t, b.wired, big)
			if !overTunnel(b) {
				t.Fatalf("the upload did not pass through the tunnel:\n%s", b.said("A"))
			}
			if _, _, told := a.noted(); len(told) != 1 || told[0] != WayRelay {
				t.Fatalf("A was told %v, want the relay", told)
			}
			a.traced(t, `send to "B": path: relay, because `+whyText[whyOld])
		})
	}
}

// A sender from before data channels does not ask for one: its upload comes
// through the relay as before, and the receiver's log says that it was not
// tried.
func TestOlderSenderIsReceivedFromThroughTheRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB := channelPair(t, true, false)
	relaying(a, b)
	cl, err := a.client(toB)
	if err != nil {
		t.Fatal(err)
	}
	sender, to := a.through(cl, toB)
	if sent, err := sender.Send(ctx, to, []string{file(t, big)}); err != nil || sent != 1 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
	arrived(t, b.wired, big)
	if ways, _, _ := b.noted(); len(ways) != 1 || ways[0] != WayRelay {
		t.Fatalf("B says the offer's way is %v, want the relay", ways)
	}
	b.traced(t, `receive from "A": path: relay, because the sender did not try a data channel`)
}

// No data channel forms within the wait, because the receiver's end of it
// is gone as soon as it has answered: the send goes through the relay, and
// both logs say that no channel formed in time.
func TestNoDataChannelInTimeFallsBackToTheRelay(t *testing.T) {
	for name, kind := range kinds {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			a, b, toB := channelPair(t, kind.browserA, kind.browserB)
			relaying(a, b)
			// Only this wait is short: it is the one the test lets pass.
			a.dc.wait = wait / 10
			b.dc.answer = func(ctx context.Context, cfg direct.Config, offer []byte) (*direct.Peer, []byte, error) {
				p, answer, err := direct.Answer(ctx, cfg, offer)
				if err == nil {
					p.Close()
				}
				return p, answer, err
			}
			if sent, err := a.SendPaired(a.sending(ctx), toB, []string{file(t, big)}); err != nil || sent != 1 {
				t.Fatalf("sent %d, err %v", sent, err)
			}
			relayedBecause(t, a, b, whyTime)
			b.traced(t, `direct: no data channel with "A" after`)
			released(t, b)
		})
	}
}

// The answer that reaches the sender carries a DTLS fingerprint that is not
// the receiver's: WebRTC makes no connection with it, so no channel is
// used, and nothing reaches the receiver on one. The send then goes the way
// it goes whenever no channel forms.
func TestWrongDTLSFingerprintMakesNoDataChannel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB := channelPair(t, false, true)
	relaying(a, b)
	a.dc.wait = wait / 10
	b.dc.answer = func(ctx context.Context, cfg direct.Config, offer []byte) (*direct.Peer, []byte, error) {
		p, answer, err := direct.Answer(ctx, cfg, offer)
		if err != nil {
			return p, answer, err
		}
		const mark = "a=fingerprint:sha-256 "
		i := bytes.Index(answer, []byte(mark))
		if i < 0 {
			t.Error("the answer has no fingerprint to change")
			return p, answer, err
		}
		// Two other hex digits in place of the first two.
		at := i + len(mark)
		wrong := []byte("00")
		if bytes.Equal(answer[at:at+2], wrong) {
			wrong = []byte("11")
		}
		copy(answer[at:], wrong)
		return p, answer, err
	}
	if sent, err := a.SendPaired(a.sending(ctx), toB, []string{file(t, big)}); err != nil || sent != 1 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
	arrived(t, b.wired, big)
	if !overTunnel(b) {
		t.Fatalf("the upload did not pass through the tunnel:\n%s", b.said("A"))
	}
	ways, _, _ := b.noted()
	_, _, told := a.noted()
	if len(ways) != 1 || ways[0] != WayRelay || len(told) != 1 || told[0] != WayRelay {
		t.Fatalf("B says the offer's way is %v and A was told %v; want the relay once each", ways, told)
	}
	a.traced(t, `send to "B": path: relay, because `)
	if log := a.trace(t); strings.Contains(log, "path: data channel") {
		t.Fatalf("A used a data channel whose fingerprint was wrong:\n%s", log)
	}
}

// As a Yonder is set, no data channel in time means no send: nothing of the
// files passes through the tunnel, the sender gets ErrNoChannel, and its log
// says why.
func TestNoDataChannelInTimeRefusesTheSend(t *testing.T) {
	for name, kind := range kinds {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			a, b, toB := channelPair(t, kind.browserA, kind.browserB)
			a.dc.wait = wait / 10
			b.dc.answer = func(ctx context.Context, cfg direct.Config, offer []byte) (*direct.Peer, []byte, error) {
				p, answer, err := direct.Answer(ctx, cfg, offer)
				if err == nil {
					p.Close()
				}
				return p, answer, err
			}
			if sent, err := a.SendPaired(a.sending(ctx), toB, []string{file(t, big)}); !errors.Is(err, ErrNoChannel) || sent != 0 {
				t.Fatalf("sent %d, err %v; want none and ErrNoChannel", sent, err)
			}
			if overTunnel(b) {
				t.Fatalf("the files were offered through the tunnel:\n%s", b.said("A"))
			}
			if _, _, told := a.noted(); len(told) != 0 {
				t.Fatalf("A was told the way %v of a send that was not made", told)
			}
			a.traced(t, `send to "B": path: none, refused, because `+whyText[whyTime])
			released(t, b)
		})
	}
}

// The same when the other device answers as a Yonder from before data
// channels: the error says that it is older.
func TestOlderDeviceIsNotSentTo(t *testing.T) {
	for name, kind := range kinds {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			a, b, toB := channelPair(t, kind.browserA, kind.browserB)
			old(b)
			if sent, err := a.SendPaired(a.sending(ctx), toB, []string{file(t, big)}); !errors.Is(err, ErrOldDevice) || sent != 0 {
				t.Fatalf("sent %d, err %v; want none and ErrOldDevice", sent, err)
			}
			if overTunnel(b) {
				t.Fatalf("the files were offered through the tunnel:\n%s", b.said("A"))
			}
			a.traced(t, `send to "B": path: none, refused, because `+whyText[whyOld])
		})
	}
}

// An upload that a browser's end sends through the tunnel all the same, as
// a Yonder from before data channels does, is refused by the receiver, and
// no file of it arrives.
func TestUploadThroughTheRelayIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB := channelPair(t, true, false)
	cl, err := a.client(toB)
	if err != nil {
		t.Fatal(err)
	}
	sender, to := a.through(cl, toB)
	if sent, err := sender.Send(ctx, to, []string{file(t, big)}); err == nil || sent != 0 {
		t.Fatalf("sent %d, err %v; want none and an error", sent, err)
	}
	if said := b.said("A"); strings.Contains(said, "/upload?") {
		t.Fatalf("a file was uploaded through the tunnel:\n%s", said)
	}
	b.traced(t, `receive from "A": path: relay, refused, because the sender did not try a data channel`)
}

// stranger is a device that is not one of the pair.
func stranger(t *testing.T) *Node {
	t.Helper()
	return newNode(t, "X")
}

// The channel is open, but the receiver shows a certificate inside it that
// is not the one it was paired with: the send stops there. It does not go
// on over the channel, and it does not change to the relay.
func TestWrongInnerCertificateStopsTheSend(t *testing.T) {
	for name, kind := range kinds {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			a, b, toB := channelPair(t, kind.browserA, kind.browserB)
			// Only what B serves on a data channel: its tunnel keeps its
			// own certificate.
			own, other := b.dc.serverTLS, stranger(t).tlsCfg.Certificates
			b.dc.serverTLS = func(d pairing.Device) *tls.Config {
				conf := own(d)
				conf.Certificates = other
				return conf
			}
			sent, err := a.SendPaired(a.sending(ctx), toB, []string{file(t, big)})
			if err == nil || sent != 0 || !strings.Contains(err.Error(), "certificate does not match") {
				t.Fatalf("sent %d, err %v; want the certificate refused", sent, err)
			}
			if _, _, told := a.noted(); len(told) != 1 || told[0] != WayChannel {
				t.Fatalf("A was told %v; the test needs the data channel to have opened", told)
			}
			if offers := b.asked(); len(offers) != 0 || overTunnel(b) {
				t.Fatalf("B was asked about %d offers, and through the tunnel passed:\n%s", len(offers), b.said("A"))
			}
			nothingKept(t, b.wired)
			a.traced(t, `send to "B": path: data channel after`)
		})
	}
}

// The channel is open, but the sender shows a certificate inside it that is
// not the paired device's: the receiver takes nothing from it. That the
// channel was asked for through the tunnel is not enough.
func TestDataChannelNeedsThePairedDevicesCertificate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB := channelPair(t, true, false)
	a.sender.Cert = stranger(t).sender.Cert
	sent, err := a.SendPaired(a.sending(ctx), toB, []string{file(t, big)})
	if err == nil || sent != 0 {
		t.Fatalf("sent %d, err %v; want the send refused", sent, err)
	}
	if _, _, told := a.noted(); len(told) != 1 || told[0] != WayChannel {
		t.Fatalf("A was told %v; the test needs the data channel to have opened", told)
	}
	if offers := b.asked(); len(offers) != 0 {
		t.Fatalf("B was asked about %d offers from a device with another certificate", len(offers))
	}
	nothingKept(t, b.wired)
}

// offer makes the description that a sender posts.
func offer(t *testing.T, ctx context.Context) []byte {
	t.Helper()
	p, desc, err := direct.Offer(ctx, direct.Config{Loopback: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return desc
}

// ask posts body to B's request for a data channel, as A through its
// stand-in tunnel connection, and gives the status.
func ask(t *testing.T, ctx context.Context, a *end, toB pairing.Device, body []byte) int {
	t.Helper()
	cl, err := a.client(toB)
	if err != nil {
		t.Fatal(err)
	}
	sender, to := a.through(cl, toB)
	status, _, err := sender.Post(ctx, to, directPath, body)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

// The request for a data channel is answered to a paired device through
// the tunnel, and to nobody else: not on the local network, not to a key
// that is not paired, not over a data channel, and not between two apps.
func TestDataChannelIsOnlyAskedForInTheTunnel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB := channelPair(t, true, false)

	if got := ask(t, ctx, a, toB, offer(t, ctx)); got != http.StatusOK {
		t.Fatalf("a paired device, through the tunnel: HTTP %d, want 200", got)
	}

	t.Run("the local network", func(t *testing.T) {
		status, _, err := a.sender.Post(ctx, localsend.Peer{Host: "127.0.0.1", Info: b.Info}, directPath, offer(t, ctx))
		if err != nil || status != http.StatusNotFound {
			t.Fatalf("HTTP %d, err %v; want 404", status, err)
		}
	})
	t.Run("a key that is not paired", func(t *testing.T) {
		c := newEnd(t, "C", true)
		toBFromC := c.connectTo(b.wired) // B does not hold C
		if got := ask(t, ctx, c, toBFromC, offer(t, ctx)); got != http.StatusNotFound {
			t.Fatalf("HTTP %d, want 404", got)
		}
	})
	t.Run("another method", func(t *testing.T) {
		cl, err := a.client(toB)
		if err != nil {
			t.Fatal(err)
		}
		c := &http.Client{Transport: &http.Transport{DialContext: cl.DialContext, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
		defer c.CloseIdleConnections()
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://device.yonder"+directPath, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("HTTP %d, want 404", resp.StatusCode)
		}
	})
	t.Run("two apps", func(t *testing.T) {
		a, b, toB := channelPair(t, false, false)
		b.met(a.Info.Fingerprint, a.Info.DeviceType)
		if got := ask(t, ctx, a, toB, offer(t, ctx)); got != http.StatusForbidden {
			t.Fatalf("HTTP %d, want 403", got)
		}
	})
}

// channelTo opens a data channel from A to B, and gives a sender whose
// connections go through it.
func channelTo(t *testing.T, ctx context.Context, a *end, toB pairing.Device) (*localsend.Sender, localsend.Peer) {
	t.Helper()
	cl, err := a.client(toB)
	if err != nil {
		t.Fatal(err)
	}
	sender, to := a.through(cl, toB)
	p, _, err := a.openChannel(ctx, sender, to)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	sender.Dial = func(ctx context.Context, _, _ string) (net.Conn, error) { return p.Dial(ctx) }
	return sender, to
}

// A data channel carries uploads between a browser and its paired device,
// and none of what only the tunnel may carry: no request for another
// channel, no new address for the device, no offer whose files are to be
// fetched, and no upload between two apps. (A lent file is given only to
// what tunnelSender knows, which TestSendWithABrowserGoesOverADataChannel
// shows a data channel is not.)
func TestDataChannelCarriesOnlyUploads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB := channelPair(t, true, false)
	over, to := channelTo(t, ctx, a, toB)

	// The channel works: B answers who it is on it.
	if info, err := over.Ask(ctx, to); err != nil || info.Fingerprint != b.Info.Fingerprint {
		t.Fatalf("a question over the channel: %+v, err %v", info, err)
	}

	t.Run("a request for a data channel", func(t *testing.T) {
		status, _, err := over.Post(ctx, to, directPath, offer(t, ctx))
		if err != nil || status != http.StatusNotFound {
			t.Fatalf("HTTP %d, err %v; want 404", status, err)
		}
	})
	t.Run("a new address", func(t *testing.T) {
		held, _ := b.paired(a.Info.Fingerprint)
		moved, err := held.Addr.On(useRelays(t, "relay2.example"))
		if err != nil {
			t.Fatal(err)
		}
		elsewhere := *over
		elsewhere.AskHeader = http.Header{addrHeader: {string(moved)}}
		if _, err := elsewhere.Ask(ctx, to); err != nil {
			t.Fatal(err)
		}
		if now, _ := b.paired(a.Info.Fingerprint); now.Addr != held.Addr {
			t.Fatal("B took a new address for A from a data channel")
		}
		// The same question through the tunnel is believed.
		cl, err := a.client(toB)
		if err != nil {
			t.Fatal(err)
		}
		tunnelled, to := a.through(cl, toB)
		tunnelled.AskHeader = elsewhere.AskHeader
		if _, err := tunnelled.Ask(ctx, to); err != nil {
			t.Fatal(err)
		}
		if now, _ := b.paired(a.Info.Fingerprint); now.Addr != moved {
			t.Fatal("B did not take A's new address from the tunnel, so the test shows nothing")
		}
	})
	t.Run("an offer to fetch", func(t *testing.T) {
		marked := *over
		marked.OfferHeader = http.Header{localsend.FetchHeader: {"a-mark"}}
		if sent, err := marked.Send(ctx, to, []string{file(t, big)}); err == nil || sent != 0 {
			t.Fatalf("sent %d, err %v; want the offer refused", sent, err)
		}
		if offers := b.asked(); len(offers) != 0 {
			t.Fatalf("B was asked about %d offers", len(offers))
		}
	})
	t.Run("two apps", func(t *testing.T) {
		// Two apps, and B has not yet heard what A is, so it answers the
		// request; the offer says what A is, and is refused.
		a, b, toB := channelPair(t, false, false)
		over, to := channelTo(t, ctx, a, toB)
		if sent, err := over.Send(ctx, to, []string{file(t, big)}); !errors.Is(err, localsend.ErrDeclined) || sent != 0 {
			t.Fatalf("sent %d, err %v; want declined", sent, err)
		}
		if offers := b.asked(); len(offers) != 0 {
			t.Fatalf("B was asked about %d offers", len(offers))
		}
		b.traced(t, `receive from "A": path: data channel, admitted: false`)
	})
}

// A device that is unpaired loses the data channels it has here, and what
// still arrives on one is not taken as from a paired device.
func TestUnpairedDeviceLosesItsDataChannel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB := channelPair(t, true, false)
	over, to := channelTo(t, ctx, a, toB)
	if _, err := over.Ask(ctx, to); err != nil {
		t.Fatal(err)
	}
	if err := b.Unpair(ctx, a.Info.Fingerprint); err != nil {
		t.Fatal(err)
	}
	released(t, b)
	if _, channel, ok := b.channelSender(context.WithValue(ctx, channelPeerKey{}, a.Info.Fingerprint)); !channel || ok {
		t.Fatal("a data channel of an unpaired device still counts as a paired device's")
	}
}

// The limits on the request: a description that is too long, more
// connections at a time than allowed, and more requests in the time than
// allowed are each refused, and the log says so.
func TestDataChannelRequestLimits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	t.Run("a description that is too long", func(t *testing.T) {
		a, b, toB := channelPair(t, true, false)
		if got := ask(t, ctx, a, toB, make([]byte, direct.MaxDescription+1)); got != http.StatusRequestEntityTooLarge {
			t.Fatalf("HTTP %d, want 413", got)
		}
		if got := ask(t, ctx, a, toB, []byte("not a description")); got != http.StatusBadRequest {
			t.Fatalf("what is not a description: HTTP %d, want 400", got)
		}
		b.traced(t, `refused: its description is too long`)
		released(t, b)
	})
	t.Run("too many at a time", func(t *testing.T) {
		a, b, toB := channelPair(t, true, false)
		b.dc.most = 2
		for i := range 3 {
			want := http.StatusOK
			if i == 2 {
				want = http.StatusTooManyRequests
			}
			// Each one answered stays, waiting for its sender to connect.
			if got := ask(t, ctx, a, toB, offer(t, ctx)); got != want {
				t.Fatalf("request %d: HTTP %d, want %d", i+1, got, want)
			}
		}
		b.traced(t, `refused: too many`)
	})
	t.Run("too many in the time", func(t *testing.T) {
		a, b, toB := channelPair(t, true, false)
		b.dc.asks = 2
		// What is refused before it is made counts too.
		for i, want := range []int{http.StatusBadRequest, http.StatusOK, http.StatusTooManyRequests} {
			body := offer(t, ctx)
			if i == 0 {
				body = []byte("not a description")
			}
			if got := ask(t, ctx, a, toB, body); got != want {
				t.Fatalf("request %d: HTTP %d, want %d", i+1, got, want)
			}
		}
		// Another device's requests are counted by themselves.
		c := newEnd(t, "C", true)
		toBFromC := c.connectTo(b.wired)
		b.connectTo(c.wired)
		if got := ask(t, ctx, c, toBFromC, offer(t, ctx)); got != http.StatusOK {
			t.Fatalf("another paired device: HTTP %d, want 200", got)
		}
	})
}

// relayedLink is a tunnel connection that says it goes through the relay.
type relayedLink struct{ link }

func (relayedLink) Path(context.Context) tunnel.Path { return tunnel.PathRelayed }

// While a send goes over a data channel it is not shown as relayed,
// whatever the tunnel's own path is.
func TestSendOverADataChannelIsNotRelayed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB := channelPair(t, false, true)
	connect := a.connect
	a.connect = func(addr tunnel.Addr, keys *tunnel.Keys) (link, error) {
		cl, err := connect(addr, keys)
		return relayedLink{cl}, err
	}
	var during, checked bool
	accept := b.Receiver.Accept
	b.Receiver.Accept = func(ctx context.Context, o localsend.Offer) []string {
		during, checked = a.SendRelayed(ctx, toB), true
		return accept(ctx, o)
	}
	if _, err := a.client(toB); err != nil {
		t.Fatal(err)
	}
	if !a.SendRelayed(ctx, toB) {
		t.Fatal("the test needs the tunnel to count as relayed")
	}
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, big)}); err != nil || sent != 1 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
	if !checked || during {
		t.Fatalf("during the send it was looked at: %v, and shown as relayed: %v", checked, during)
	}
}

// The app asks the local network for a browser's ".local" names: without
// that, an app behind a firewall and a browser on one network find no path.
func TestChannelRuleAsksForLocalNames(t *testing.T) {
	n := startNode(t, Config{Dir: t.TempDir(), SaveDir: t.TempDir(), Name: "Local Names", Listen: "127.0.0.1:0"})
	if !n.dc.cfg.MDNS {
		t.Error("the data channels do not ask for .local names")
	}
}
