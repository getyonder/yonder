package app

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"yonder/internal/direct"
	"yonder/internal/localsend"
	"yonder/internal/nettest"
	"yonder/internal/pairing"
	"yonder/internal/tunnel"
	"yonder/internal/tunnel/tunneltest"
)

// newNode starts a Node with its own folders, on a loopback port, without
// local network discovery, so tests share nothing and announce nothing.
func newNode(t *testing.T, name string) *Node {
	t.Helper()
	return newNodeIn(t, t.TempDir(), name)
}

// newNodeIn is newNode with the device's own folder given, so a test can
// start the same device again.
func newNodeIn(t *testing.T, dir, name string) *Node {
	t.Helper()
	return startNode(t, Config{Dir: dir, SaveDir: t.TempDir(), Name: name, Listen: "127.0.0.1:0"})
}

// startNode is newNode with all of the Config given. Its data channels use
// only the loopback addresses and no STUN server, so the tests stay offline.
func startNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	n, err := Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	n.dc.cfg = direct.Config{Loopback: true, MDNS: n.dc.cfg.MDNS}
	t.Cleanup(n.Close)
	return n
}

// With its port taken, as by the stock LocalSend app, a Node takes the
// next one up, and failing that any free one.
func TestPortTaken(t *testing.T) {
	// A port to take whose neighbour is free.
	var taken, next net.Listener
	for range 20 {
		var err error
		if taken, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		if next, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(taken.Addr().(*net.TCPAddr).Port+1))); err == nil {
			break
		}
		taken.Close()
	}
	if next == nil {
		t.Skip("no two free ports side by side")
	}
	defer taken.Close()
	port := taken.Addr().(*net.TCPAddr).Port
	start := func() int {
		n, err := Start(Config{Dir: t.TempDir(), SaveDir: t.TempDir(), Name: "n", Listen: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))})
		if err != nil {
			t.Fatal(err)
		}
		defer n.Close()
		return n.Info.Port
	}

	if got := start(); got == port || got == port+1 {
		t.Errorf("with both ports taken it says it is on %d", got)
	}
	next.Close()
	if got := start(); got != port+1 {
		t.Errorf("it is on port %d, want the next one up, %d", got, port+1)
	}
}

func openTunnel(t *testing.T, ctx context.Context, n *Node) {
	t.Helper()
	if err := n.OpenTunnel(ctx); err != nil {
		t.Fatalf("%s: open tunnel: %v", n.Info.Alias, err)
	}
}

func TestPairingNeedsTheTunnel(t *testing.T) {
	n := newNode(t, "alone")
	if _, err := n.Pair(context.Background(), nil); err == nil {
		t.Fatal("pairing opened without a tunnel")
	}
	if _, err := n.Join(context.Background(), pairing.Link("tcWhatever")); !errors.Is(err, ErrTunnelDown) {
		t.Fatalf("joined without a tunnel: err %v, want ErrTunnelDown", err)
	}
}

// The whole of pairing, through the public relays: two devices pair by
// link, send a file each way's worth of checks, a stranger holding the
// address is kept out, and unpairing cuts the device off.
func TestPairSendAndUnpair(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	a, b, stranger := newNode(t, "A"), newNode(t, "B"), newNode(t, "stranger")
	for _, n := range []*Node{a, b, stranger} {
		openTunnel(t, ctx, n)
	}

	// Pair: A shows a link, B opens it, A's owner says yes.
	var asked string
	p, err := a.Pair(ctx, func(_ context.Context, joiner pairing.Device) bool {
		asked = joiner.Name
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	hostSeenByB, err := b.Join(ctx, p.Link)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	joinerSeenByA, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if asked != "B" || joinerSeenByA.Name != "B" || hostSeenByB.Name != "A" {
		t.Fatalf("asked about %q; A paired with %q, B with %q", asked, joinerSeenByA.Name, hostSeenByB.Name)
	}
	if d, ok := a.FindPaired("b"); !ok || d != joinerSeenByA {
		t.Fatal("A did not save B")
	}

	// Send B to A through the tunnel; A can tell it is from a paired device.
	var from string
	a.Receiver.Accept = func(ctx context.Context, o localsend.Offer) []string {
		if d, ok := a.PairedSender(ctx); ok {
			from = d.Name
		}
		return []string{o.Files[0].ID}
	}
	data := bytes.Repeat([]byte("yonder "), 100_000)
	path := filepath.Join(t.TempDir(), "far.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if sent, err := b.SendPaired(ctx, hostSeenByB, []string{path}); err != nil || sent != 1 {
		t.Fatalf("send: sent %d, err %v", sent, err)
	}
	got, err := os.ReadFile(filepath.Join(a.Receiver.SaveDir, "far.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("received file differs (err %v)", err)
	}
	if from != "B" {
		t.Fatalf("A saw the offer as from %q, want paired device B", from)
	}
	if !a.Online(ctx, joinerSeenByA) {
		t.Fatal("A sees B as offline")
	}

	// A restarts, as it would after a reboot. B's kept connection is now
	// stale, and B must notice and reconnect by itself.
	a.mu.Lock()
	a.tun.Close()
	a.tun = nil
	err = a.openTunnel(ctx, tunnel.Start)
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// With a send under way B leaves its connection alone, stale or not:
	// closing it would end the send.
	fp := hostSeenByB.Fingerprint
	b.mu.Lock()
	kept := b.clients[fp]
	b.sending[fp]++
	b.mu.Unlock()
	on := b.Online(ctx, hostSeenByB)
	b.mu.Lock()
	same := b.clients[fp] == kept
	b.sending[fp]--
	b.mu.Unlock()
	if !on || !same {
		t.Fatalf("during a send: online %v, connection kept %v", on, same)
	}
	if sent, err := b.SendPaired(ctx, hostSeenByB, []string{path}); err != nil || sent != 1 {
		t.Fatalf("send after A restarted: sent %d, err %v", sent, err)
	}

	// The link is dead once the pairing is over.
	short, stop := context.WithTimeout(ctx, 15*time.Second)
	if _, err := stranger.Join(short, p.Link); !errors.Is(err, ErrNotReached) {
		t.Fatalf("the pairing link a second time: err %v, want ErrNotReached", err)
	}
	stop()

	// A stranger who somehow holds A's address is not let in.
	if stranger.Online(ctx, hostSeenByB) {
		t.Fatal("a device that never paired reached A")
	}

	// Unpairing cuts B off, and A keeps working for nobody else's sake.
	if err := a.Unpair(ctx, joinerSeenByA.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if devices, _ := a.Paired(); len(devices) != 0 {
		t.Fatalf("A still lists %d device(s)", len(devices))
	}
	short, stop = context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	if sent, err := b.SendPaired(short, hostSeenByB, []string{path}); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("B sending to A after being unpaired: sent %d, err %v; want ErrUnreachable", sent, err)
	}
}

// A device says where it is now with each question it asks, but is only
// believed through the tunnel, where who is asking is certain. On the local
// network anyone could say it, even from the address that is A's in the
// tunnel.
func TestAddressIsOnlyHeardThroughTheTunnel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a, b := newNode(t, "A"), newNode(t, "B")
	was := tunneltest.Addr(t)
	if err := b.store.Add(pairing.Device{Name: "A", Fingerprint: a.Info.Fingerprint, Addr: was, ClientID: a.selfID}); err != nil {
		t.Fatal(err)
	}
	// Every sender's address is A's to the tunnel that stands in here.
	b.peerID = func(net.Addr, net.Addr) tunnel.ClientID { return a.selfID }
	listed := useRelays(t, "relay2.example")
	moved, err := was.On(listed)
	if err != nil {
		t.Fatal(err)
	}
	sender := *a.sender
	sender.AskHeader = http.Header{addrHeader: {string(moved)}}
	if _, err := sender.Ask(ctx, localsend.Peer{Host: "127.0.0.1", Info: b.Info}); err != nil {
		t.Fatalf("A asks B on the local network: %v", err)
	}
	if d, ok := b.FindPaired("A"); !ok || d.Addr != was {
		t.Errorf("B took a new address for A from the local network (paired %v)", ok)
	}

	// The same question through the tunnel's listener is believed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go b.serve(ln, true)
	info := b.Info
	info.Port = ln.Addr().(*net.TCPAddr).Port
	if _, err := sender.Ask(ctx, localsend.Peer{Host: "127.0.0.1", Info: info}); err != nil {
		t.Fatalf("A asks B through the tunnel: %v", err)
	}
	if d, ok := b.FindPaired("A"); !ok || d.Addr != moved {
		t.Errorf("B did not take A's new address from the tunnel (paired %v)", ok)
	}
}

// useRelays puts relay in the list of relays for the test, and returns it.
func useRelays(t *testing.T, relay string) string {
	t.Helper()
	was := tunnel.Relays()
	tunnel.UseRelays([]string{relay})
	t.Cleanup(func() { tunnel.UseRelays(was) })
	return relay
}

// After the relay changes, each device is at a new address, and each holds
// the other's old one. They find each other again without being paired
// again: a device that does not answer at its old address is tried on the
// relay of today, and a device that reaches another tells it where it is.
func TestPairedDevicesFindEachOtherAfterTheRelayChanges(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	a, b, aSeenByB, bSeenByA := pairedNodes(t, ctx)
	old := func(n *Node, d pairing.Device) pairing.Device { return holdOld(t, n, d) }

	// A holds B's old address: it finds B on today's relay and remembers.
	stale := old(a, bSeenByA)
	if !a.Online(ctx, stale) {
		t.Fatal("A does not find B, whose address it holds from before the relay changed")
	}
	if holds(a, bSeenByA) != bSeenByA.Addr {
		t.Error("A found B but did not store the address it found it at")
	}

	// B holds A's old address. A reaches B, and that tells B where A is.
	old(b, aSeenByB)
	if !a.Online(ctx, bSeenByA) {
		t.Fatal("A no longer reaches B")
	}
	if holds(b, aSeenByB) != aSeenByB.Addr {
		t.Error("A reached B, but B still holds A's old address")
	}
	if !b.Online(ctx, aSeenByB) {
		t.Error("B does not reach A at the address A told it")
	}
}

// pairedNodes is two Nodes, A and B, with their tunnels open, paired with
// each other, and each as the other holds it.
func pairedNodes(t *testing.T, ctx context.Context) (a, b *Node, aSeenByB, bSeenByA pairing.Device) {
	t.Helper()
	a, b = newNode(t, "A"), newNode(t, "B")
	aSeenByB, bSeenByA = pair(t, ctx, a, b)
	return a, b, aSeenByB, bSeenByA
}

// pair opens the tunnels of a and b and pairs them by a link from a.
func pair(t *testing.T, ctx context.Context, a, b *Node) (aSeenByB, bSeenByA pairing.Device) {
	t.Helper()
	openTunnel(t, ctx, a)
	openTunnel(t, ctx, b)
	p, err := a.Pair(ctx, func(context.Context, pairing.Device) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if aSeenByB, err = b.Join(ctx, p.Link); err != nil {
		t.Fatalf("join: %v", err)
	}
	if bSeenByA, err = p.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	return aSeenByB, bSeenByA
}

// A device that changes its name starts again under the new one. The next
// time a paired device asks whether it is there, the answer carries the
// new name, and the paired device stores it and says its list changed.
func TestNewNameReachesAPairedDevice(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	a, dir := newNode(t, "A"), t.TempDir()
	b := newNodeIn(t, dir, "B")
	var updated atomic.Int32
	a.Updated = func() { updated.Add(1) }
	_, bSeenByA := pair(t, ctx, a, b)
	if !a.Online(ctx, bSeenByA) {
		t.Fatal("A does not reach B before the name changes")
	}
	before := updated.Load()

	b.Close()
	b = newNodeIn(t, dir, "B renamed")
	openTunnel(t, ctx, b)
	if !a.Online(ctx, bSeenByA) {
		t.Fatal("A does not reach B after B started again under a new name")
	}
	got, ok := a.FindPaired("B renamed")
	if !ok || got.Fingerprint != bSeenByA.Fingerprint {
		devices, _ := a.Paired()
		t.Fatalf("A does not hold B under its new name: %+v", devices)
	}
	if _, ok := a.FindPaired("B"); ok {
		t.Error("A still holds a device under B's old name")
	}
	if updated.Load() == before {
		t.Error("A stored the new name but did not say its list changed")
	}
}

// holdOld makes n hold the address d had on a relay that is gone.
func holdOld(t *testing.T, n *Node, d pairing.Device) pairing.Device {
	t.Helper()
	addr, err := d.Addr.On("gone.invalid")
	if err != nil {
		t.Fatal(err)
	}
	// Not by Readdress, which takes no address off the list.
	d.Addr = addr
	if err := n.store.Add(d); err != nil {
		t.Fatalf("%s: store an old address for %s: %v", n.Info.Alias, d.Name, err)
	}
	n.readdressed(d.Fingerprint)
	return d
}

// holds is the address n holds for d.
func holds(n *Node, d pairing.Device) tunnel.Addr {
	got, _ := n.FindPaired(d.Name)
	return got.Addr
}

// A device that has moved is looked for on every relay in the list, a few
// at a time, not only on the main one, and where it answers is remembered.
// The same real relay under two spellings counts as two relays, so this
// needs no second server.
func TestMovedDeviceIsFoundAnywhereOnTheList(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	a, _, _, bSeenByA := pairedNodes(t, ctx)
	stale := holdOld(t, a, bSeenByA)

	// B's relay is in the list only under its other spelling, after a main
	// relay and another that do not answer.
	main := tunnel.Relay
	other := main + ":443"
	t.Cleanup(func() {
		tunnel.Relay = main
		tunnel.UseRelays(nil)
	})
	tunnel.Relay = "down.invalid"
	tunnel.UseRelays([]string{"down-too.invalid", other})
	want, err := bSeenByA.Addr.On(other)
	if err != nil {
		t.Fatal(err)
	}

	if !a.Online(ctx, stale) {
		t.Fatal("A does not find B on the list")
	}
	if got := holds(a, bSeenByA); got != want {
		t.Errorf("A found B but holds %s, want B's address on %s", got, other)
	}
	found := bSeenByA
	found.Addr = want
	if !a.Online(ctx, found) {
		t.Error("A does not reach B at the address it found")
	}
}

// With the site out of reach and no copy kept, a device starts on the main
// relay, as before there was a list. Unpairing restarts the tunnel on the
// same home without asking the site again.
func TestStartWithoutTheListAndRestart(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	gone := httptest.NewServer(nil)
	gone.Close()
	var asked atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		http.NotFound(w, r)
	}))
	defer site.Close()
	old := tunnel.ListURL
	t.Cleanup(func() {
		tunnel.ListURL = old
		tunnel.UseRelays(nil)
	})
	tunnel.ListURL = gone.URL + "/relays.json"

	a := newNode(t, "A")
	began := time.Now()
	openTunnel(t, ctx, a)
	t.Logf("the tunnel opened in %v", time.Since(began))
	if !slices.Equal(tunnel.Relays(), []string{tunnel.Relay}) || len(a.tun.Addr().Elsewhere()) != 0 {
		t.Errorf("without the list: relays %v, address elsewhere too %v", tunnel.Relays(), a.tun.Addr().Elsewhere())
	}
	if _, _, kept := a.store.RelayList(); kept {
		t.Error("a copy of a list never fetched was kept")
	}

	tunnel.ListURL = site.URL + "/relays.json"
	addr, region := a.tun.Addr(), string(a.keys.Region)
	if err := a.Unpair(ctx, "no such device"); err != nil {
		t.Fatal(err)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("the restart asked the site for the list %d times", n)
	}
	if a.tun.Addr() != addr || string(a.keys.Region) != region {
		t.Error("the restart changed the address or the home")
	}
}

// A connection that sends no request, or sends it very slowly, is cut off,
// on the local network, through the tunnel and on a pairing address; one
// that is sending a file is not.
func TestServersLimitIdleConnections(t *testing.T) {
	n := newNode(t, "n")
	for what, srv := range map[string]*http.Server{"the receiver": n.srv, "a pairing": newServer(http.NotFoundHandler())} {
		if srv.ReadHeaderTimeout <= 0 || srv.IdleTimeout <= 0 {
			t.Errorf("%s: header time limit %v, idle time limit %v; want both", what, srv.ReadHeaderTimeout, srv.IdleTimeout)
		}
		if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
			t.Errorf("%s: a request may last %v reading, %v writing; want no limit", what, srv.ReadTimeout, srv.WriteTimeout)
		}
	}
}

// A pairing link nobody has used ends at its time limit.
func TestPairingLinkExpires(t *testing.T) {
	closed := false
	p := &Pairing{ended: make(chan pairEnd, 1), expires: time.Now(), asking: func() bool { return false }, close: func() { closed = true }}
	if _, err := p.Wait(context.Background()); !errors.Is(err, ErrLinkEnded) || !closed {
		t.Fatalf("err %v, closed %v; want ErrLinkEnded and the link closed", err, closed)
	}
	// While the owner is being asked, the time limit waits for the answer.
	bo := pairing.Device{Name: "Bo"}
	p = &Pairing{ended: make(chan pairEnd, 1), expires: time.Now(), close: func() {}}
	p.asking = func() bool { p.ended <- pairEnd{joiner: bo}; return true }
	if got, err := p.Wait(context.Background()); err != nil || got != bo {
		t.Fatalf("asked at the time limit: %+v, %v; want Bo paired", got, err)
	}
	if linkLife != 10*time.Minute {
		t.Errorf("a link lasts %v, want ten minutes", linkLife)
	}
}

// A device whose home relay goes silent while it runs opens its tunnel
// again after some checks, and is then on a relay that answers. The same
// real relay under two spellings counts as two relays.
func TestHomeMovesWhileRunning(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	n := newNode(t, "mover")
	var updated atomic.Int32
	n.Updated = func() { updated.Add(1) }
	if err := n.OpenTunnel(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := n.self()
	if err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	n.homeAnswers = func(context.Context, *tunnel.Server) bool { return false }
	n.mu.Unlock()

	// With one relay there is nowhere to go, so nothing is done.
	for range homeMissesFirst {
		if moved, err := n.CheckHome(ctx); moved || err != nil {
			t.Fatalf("one relay in the list: moved %v, err %v", moved, err)
		}
	}
	if now, _ := n.self(); now.Addr != before.Addr {
		t.Fatal("one relay in the list: the address changed")
	}

	// The main relay is gone; the list has it and the real one's other
	// spelling.
	main := tunnel.Relay
	other := main + ":443"
	t.Cleanup(func() {
		tunnel.Relay = main
		tunnel.UseRelays(nil)
	})
	tunnel.Relay = "down.invalid"
	tunnel.UseRelays([]string{other})
	want, err := before.Addr.On(other)
	if err != nil {
		t.Fatal(err)
	}
	for i := range homeMissesFirst {
		moved, err := n.CheckHome(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if last := i == homeMissesFirst-1; moved != last {
			t.Fatalf("silent check %d: moved %v", i+1, moved)
		}
	}
	now, err := n.self()
	if err != nil {
		t.Fatal(err)
	}
	if now.Addr != want {
		t.Errorf("after the move the address is not on %s", other)
	}
	if updated.Load() != 1 {
		t.Errorf("Updated was called %d times, want 1", updated.Load())
	}

	// There it answers, and it stays.
	n.mu.Lock()
	n.homeAnswers = nil
	n.mu.Unlock()
	if moved, err := n.CheckHome(ctx); moved || err != nil {
		t.Fatalf("on the new home: moved %v, err %v", moved, err)
	}
	n.mu.Lock()
	misses := n.homeMisses
	n.mu.Unlock()
	if misses != 0 {
		t.Errorf("the new home did not answer: %d misses", misses)
	}
}
