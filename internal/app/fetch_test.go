package app

// Sending over whichever connection is direct, with the tunnel stood in
// for, so the tests stay offline: each "tunnel" connection is a loopback
// one to a listener that the other Node serves as it serves its tunnel, and
// the paths are set by hand. TestSendOverTheOtherConnection in
// direct_test.go is the same send through the real relay.

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yonder/internal/localsend"
	"yonder/internal/pairing"
	"yonder/internal/tunnel"
	"yonder/internal/tunnel/tunneltest"
)

// until waits, at most wait, for cond to hold. It is the one wait of these
// tests for something that another goroutine does.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v for %s", wait, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// ways is the two paths between a Node and the other device, set by hand:
// out on the Node's own connection, in on the other device's.
type ways struct {
	mu      sync.Mutex
	out, in tunnel.Path
}

func (w *ways) set(out, in tunnel.Path) {
	w.mu.Lock()
	w.out, w.in = out, in
	w.mu.Unlock()
}

func (w *ways) path(_ context.Context, _ pairing.Device, outgoing bool) tunnel.Path {
	w.mu.Lock()
	defer w.mu.Unlock()
	if outgoing {
		return w.out
	}
	return w.in
}

// wired is a Node whose tunnel is stood in for.
type wired struct {
	*Node
	t    *testing.T
	ways *ways

	mu    sync.Mutex
	from  map[int]tunnel.ClientID // who is behind each stand-in listener, by its port
	to    map[tunnel.Addr]int     // the port of each paired device's stand-in listener
	heard map[string][]string     // by device name: all that passed on that device's connection to this one
	// What its person was asked, and how its transfers that came in ended.
	offers   []localsend.Offer
	finished chan bool
	guards   int // how many admitted offers came with a guard
}

// loopback is a tunnel connection stood in for by one to a local port.
type loopback struct{ port int }

func (l loopback) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", l.port))
}
func (loopback) Path(context.Context) tunnel.Path { return tunnel.PathUnknown }
func (loopback) Close() error                     { return nil }

// newWired starts a Node that takes every offer the rule admits, as the
// window does when its person says yes, with the guard the rule gives.
func newWired(t *testing.T, name string) *wired {
	t.Helper()
	return wire(t, newNode(t, name))
}

// wire stands in for the tunnel of a Node, as newWired says.
func wire(t *testing.T, n *Node) *wired {
	t.Helper()
	w := &wired{Node: n, t: t, ways: &ways{}, from: map[int]tunnel.ClientID{}, to: map[tunnel.Addr]int{},
		heard: map[string][]string{}, finished: make(chan bool, 8)}
	w.rule = pathRule{path: w.ways.path, wait: wait, every: time.Millisecond, guards: time.Millisecond}
	w.listening = func() bool { return true }
	w.connect = func(addr tunnel.Addr, _ *tunnel.Keys) (link, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		port, ok := w.to[addr]
		if !ok {
			return nil, errors.New("no such device")
		}
		return loopback{port}, nil
	}
	w.peerID = func(local, _ net.Addr) tunnel.ClientID {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.from[local.(*net.TCPAddr).Port]
	}
	var guarded func()
	w.Receiver.Accept = func(ctx context.Context, o localsend.Offer) []string {
		ok, guard := w.AdmitOffer(ctx, o)
		if !ok {
			return nil
		}
		w.mu.Lock()
		w.offers = append(w.offers, o)
		if guard != nil {
			w.guards++
		}
		w.mu.Unlock()
		if guard != nil {
			gctx, stop := context.WithCancel(context.Background())
			guarded = stop
			t.Cleanup(stop)
			go guard(gctx, w.Receiver.Cancel)
		}
		var ids []string
		for _, f := range o.Files {
			ids = append(ids, f.ID)
		}
		return ids
	}
	w.Receiver.Finished = func(complete bool) {
		if guarded != nil {
			guarded()
		}
		w.finished <- complete
	}
	return w
}

// asked is how many offers the Node's person was asked about.
func (w *wired) asked() []localsend.Offer {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]localsend.Offer(nil), w.offers...)
}

// said is all that passed on a device's connection to this Node, the
// requests and the answers, as one text.
func (w *wired) said(by string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.heard[by], "\n")
}

// connectTo gives w a stand-in tunnel connection to other, and makes other
// hold w as a paired device behind it. It returns other as w holds it.
func (w *wired) connectTo(other *wired) pairing.Device {
	w.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		w.t.Fatal(err)
	}
	// Served as the Node serves its tunnel, with all that passes noted.
	name := w.Info.Alias
	srv := &http.Server{ConnContext: other.srv.ConnContext, Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		answer := &noted{ResponseWriter: rw}
		other.srv.Handler.ServeHTTP(answer, r)
		entry := fmt.Sprintf("%s %s\n%v\n%s\n-> %d\n%v\n%s", r.Method, r.URL, r.Header, body, answer.status, rw.Header(), answer.body.Bytes())
		other.mu.Lock()
		other.heard[name] = append(other.heard[name], entry)
		other.mu.Unlock()
	})}
	go srv.Serve(tls.NewListener(tunnelListener{ln}, other.tlsCfg))
	w.t.Cleanup(func() { srv.Close() })

	addr := tunneltest.Addr(w.t)
	port := ln.Addr().(*net.TCPAddr).Port
	w.mu.Lock()
	w.to[addr] = port
	w.mu.Unlock()
	other.mu.Lock()
	other.from[port] = w.selfID
	other.mu.Unlock()
	d := pairing.Device{Name: other.Info.Alias, Fingerprint: other.Info.Fingerprint, Addr: addr, ClientID: other.selfID}
	if err := w.store.Add(d); err != nil {
		w.t.Fatal(err)
	}
	return d
}

// noted keeps what a handler answered.
type noted struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (n *noted) WriteHeader(status int) {
	n.status = status
	n.ResponseWriter.WriteHeader(status)
}

func (n *noted) Write(b []byte) (int, error) {
	if n.status == 0 {
		n.status = http.StatusOK
	}
	if n.body.Len() < 1<<16 { // enough to see a list of files; not a whole file
		n.body.Write(b)
	}
	return n.ResponseWriter.Write(b)
}

// wiredPair is two Nodes, A and B, paired, with a stand-in connection each
// way, and each as the other holds it.
func wiredPair(t *testing.T) (a, b *wired, bSeenByA, aSeenByB pairing.Device) {
	t.Helper()
	a, b = newWired(t, "A"), newWired(t, "B")
	return a, b, a.connectTo(b), b.connectTo(a)
}

// file writes a file to send, whose name is the secret that must not pass
// through the relay.
func file(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), secretName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const secretName = "secret-name.bin"

// arrived fails unless B saved the file whole.
func arrived(t *testing.T, b *wired, data []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(b.Receiver.SaveDir, secretName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("the file arrived as %d bytes, want the %d sent", len(got), len(data))
	}
}

const (
	straight = tunnel.PathDirect
	relayed  = tunnel.PathRelayed
)

// The sender's own connection is direct: the files are uploaded, as before,
// whatever the other connection is.
func TestOwnDirectConnectionUploads(t *testing.T) {
	for name, theirs := range map[string]tunnel.Path{"only the sender's is direct": relayed, "both are direct": straight} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			defer cancel()
			a, b, toB, _ := wiredPair(t)
			a.ways.set(straight, theirs)
			b.ways.set(theirs, straight)
			data := []byte("uploaded")
			if sent, err := a.SendPaired(ctx, toB, []string{file(t, data)}); err != nil || sent != 1 {
				t.Fatalf("sent %d, err %v", sent, err)
			}
			arrived(t, b, data)
			if offers := b.asked(); len(offers) != 1 || offers[0].Fetch != "" {
				t.Fatalf("B was asked about %d offers, the first marked %v; want one ordinary offer", len(offers), len(offers) > 0 && offers[0].Fetch != "")
			}
			if said := a.said("B"); strings.Contains(said, "/prepare-download") || strings.Contains(said, "/download") {
				t.Fatalf("B fetched from A though A's connection was direct:\n%s", said)
			}
		})
	}
}

// The receiver of an upload does not guard the path: its way back to the
// sender goes to the relay in the middle of a file, and the file arrives all
// the same, because the sender's connection, which carries it, is direct.
func TestReceiverOfAnUploadDoesNotGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB, _ := wiredPair(t)
	a.ways.set(straight, straight)
	b.ways.set(straight, straight)
	released := make(chan struct{})
	close(released)
	b.Receiver.Progress = held(func() { b.ways.set(relayed, relayed) }, released)
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, big)}); err != nil || sent != 1 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
	if complete := <-b.finished; !complete {
		t.Fatal("B did not call the transfer complete")
	}
	arrived(t, b, big)
	b.mu.Lock()
	guards := b.guards
	b.mu.Unlock()
	if guards != 0 {
		t.Fatal("B was given a guard for an uploaded offer")
	}
}

// Only the receiver's connection is direct: it fetches the files over that
// one, and nothing about them passes on the sender's, which is relayed.
func TestOnlyTheOtherConnectionDirectIsFetched(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB, toA := wiredPair(t)
	a.ways.set(relayed, straight)
	b.ways.set(straight, relayed)
	// While the file is on the move: B's connection counts as busy, and A
	// reports the path of the connection that carries the file.
	var busy int
	var shownRelayed bool
	b.Receiver.Received = func(string, localsend.FileMeta) {
		b.Node.mu.Lock()
		busy = b.sending[toA.Fingerprint]
		b.Node.mu.Unlock()
		shownRelayed = a.SendRelayed(ctx, toB)
	}
	data := bytes.Repeat([]byte("fetched "), 50_000)
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, data)}); err != nil || sent != 1 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
	arrived(t, b, data)
	if complete := <-b.finished; !complete {
		t.Fatal("B did not call the transfer complete")
	}
	offers := b.asked()
	if len(offers) != 1 || offers[0].Fetch == "" || len(offers[0].Files) != 1 || offers[0].Files[0].FileName != secretName {
		t.Fatalf("B's person was asked about %+v, want one marked offer of the file", offers)
	}
	if busy != 1 {
		t.Errorf("B's connection to A counted %d transfers during the fetch, want 1", busy)
	}
	if shownRelayed {
		t.Error("A showed the send as relayed, though the files went over B's direct connection")
	}
	until(t, "B's connection to be free again", func() bool {
		b.Node.mu.Lock()
		defer b.Node.mu.Unlock()
		return b.sending[toA.Fingerprint] == 0 && !b.looking[toA.Fingerprint]
	})

	// A's connection to B carried the look, the empty offer and the yes,
	// and nothing about the file; B's connection to A carried the list.
	relay, way := b.said("A"), a.said("B")
	for _, want := range []string{"Yonder-Fetch:[look]", "/prepare-upload", "Yonder-Fetch:[" + offers[0].Fetch + "]"} {
		if !strings.Contains(relay, want) {
			t.Errorf("A's connection did not carry %q:\n%s", want, relay)
		}
	}
	for _, secret := range []string{secretName, "400000", "fetched"} { // the name, the size, the content
		if strings.Contains(relay, secret) {
			t.Errorf("A's relayed connection carried %q:\n%s", secret, relay)
		}
	}
	for _, want := range []string{"/prepare-download", secretName, "/download", "/fetched"} {
		if !strings.Contains(way, want) {
			t.Errorf("B's connection did not carry %q:\n%s", want, way)
		}
	}
}

// Neither connection becomes direct: after the one wait the send fails, and
// no offer was made.
func TestNeitherConnectionDirectSendsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB, _ := wiredPair(t)
	a.ways.set(relayed, relayed)
	b.ways.set(relayed, relayed)
	a.rule.wait = 50 * time.Millisecond
	began := time.Now()
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, []byte("x"))}); !errors.Is(err, ErrNoDirectPath) || sent != 0 {
		t.Fatalf("sent %d, err %v, want ErrNoDirectPath", sent, err)
	}
	if took := time.Since(began); took < a.rule.wait {
		t.Fatalf("the send gave up after %v, before its wait of %v was over", took, a.rule.wait)
	}
	if said := b.said("A"); strings.Contains(said, "prepare-upload") {
		t.Fatalf("A offered files with no direct path:\n%s", said)
	}
	if len(b.asked()) != 0 {
		t.Fatal("B's person was asked")
	}
}

// The sender sees the receiver's connection as direct and the receiver does
// not: the receiver refuses before it asks for the list or its person, and
// the sender reports no direct path, not a "no".
func TestReceiverThatSeesNoDirectPathRefuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB, _ := wiredPair(t)
	a.ways.set(relayed, straight)
	b.ways.set(relayed, relayed)
	b.rule.wait = 50 * time.Millisecond
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, []byte("x"))}); !errors.Is(err, ErrNoDirectPath) || sent != 0 {
		t.Fatalf("sent %d, err %v, want ErrNoDirectPath", sent, err)
	}
	if len(b.asked()) != 0 {
		t.Fatal("B's person was asked")
	}
	if said := a.said("B"); strings.Contains(said, "/prepare-download") || strings.Contains(said, "/download") {
		t.Fatalf("B asked A for the files:\n%s", said)
	}
	// B can take an offer again at once.
	a.ways.set(straight, relayed)
	b.ways.set(relayed, straight)
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, []byte("x"))}); err != nil || sent != 1 {
		t.Fatalf("an upload after the refusal: sent %d, err %v", sent, err)
	}
}

// A receiver from before 0.1.18 does not say that it can fetch: it gets no
// marks, and a send to it works only over the sender's own connection.
func TestOlderReceiverGetsNoMarks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB, _ := wiredPair(t)
	b.Receiver.Source = nil // as before 0.1.18
	a.ways.set(relayed, straight)
	b.ways.set(straight, relayed)
	a.rule.wait = 50 * time.Millisecond
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, []byte("x"))}); !errors.Is(err, ErrNoDirectPath) || sent != 0 {
		t.Fatalf("sent %d, err %v, want ErrNoDirectPath", sent, err)
	}
	a.ways.set(straight, relayed)
	b.ways.set(relayed, straight)
	data := []byte("as before")
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, data)}); err != nil || sent != 1 {
		t.Fatalf("an upload: sent %d, err %v", sent, err)
	}
	arrived(t, b, data)
	if said := b.said("A"); strings.Contains(said, localsend.FetchHeader) {
		t.Fatalf("A sent a mark to a receiver that cannot fetch:\n%s", said)
	}
}

// A sender that sends no marks, as one from before 0.1.18, or the
// terminal's send, which cannot be reached itself: the new receiver takes
// its upload as before, and it offers nothing when its own connection is
// relayed.
func TestSenderWithoutMarksIsServedAsBefore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB, _ := wiredPair(t)
	a.listening = func() bool { return false }
	a.ways.set(straight, relayed)
	b.ways.set(relayed, straight)
	data := []byte("as before")
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, data)}); err != nil || sent != 1 {
		t.Fatalf("an upload: sent %d, err %v", sent, err)
	}
	arrived(t, b, data)
	a.ways.set(relayed, straight)
	b.ways.set(straight, relayed)
	a.rule.wait = 50 * time.Millisecond
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, []byte("x"))}); !errors.Is(err, ErrNoDirectPath) || sent != 0 {
		t.Fatalf("sent %d, err %v, want ErrNoDirectPath", sent, err)
	}
	if said := b.said("A"); strings.Contains(said, "look") || strings.Count(said, "prepare-upload") != 1 {
		t.Fatalf("A sent a mark though it cannot be reached, or offered twice:\n%s", said)
	}
	if len(b.asked()) != 1 {
		t.Fatalf("B's person was asked %d times, want once", len(b.asked()))
	}
}

func TestFetchedOfferDeclined(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB, _ := wiredPair(t)
	a.ways.set(relayed, straight)
	b.ways.set(straight, relayed)
	b.Receiver.Accept = func(context.Context, localsend.Offer) []string { return nil }
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, []byte("x"))}); !errors.Is(err, localsend.ErrDeclined) || sent != 0 {
		t.Fatalf("sent %d, err %v, want declined", sent, err)
	}
	if said := a.said("B"); strings.Contains(said, "/download") {
		t.Fatalf("B fetched a file it declined:\n%s", said)
	}
}

// big is a file large enough to be stopped in the middle.
var big = bytes.Repeat([]byte("0123456789abcdef"), 1<<19)

// held returns a progress report that, with the first bytes on the move,
// calls then and holds the transfer there until release is closed, so that
// what then sets off is certain to find the file in the middle. It gives up
// holding after wait.
func held(then func(), release <-chan struct{}) func(localsend.Progress) {
	var once sync.Once
	return func(p localsend.Progress) {
		if p.Done == 0 {
			return
		}
		once.Do(func() {
			then()
			select {
			case <-release:
			case <-time.After(wait):
			}
		})
	}
}

// The path of the connection that carries a fetch goes to the relay in the
// middle of a file: each end stops the transfer on its own look, and
// nothing is kept.
func TestFetchStopsWhenItsPathDrops(t *testing.T) {
	t.Run("seen by the receiver", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		a, b, toB, _ := wiredPair(t)
		a.ways.set(relayed, straight)
		b.ways.set(straight, relayed)
		stopped := make(chan struct{})
		b.Receiver.Progress = held(func() { b.ways.set(relayed, relayed) }, stopped)
		finished := b.Receiver.Finished
		b.Receiver.Finished = func(complete bool) {
			finished(complete)
			close(stopped)
		}
		if sent, err := a.SendPaired(ctx, toB, []string{file(t, big)}); !errors.Is(err, localsend.ErrFetchStopped) || sent != 0 {
			t.Fatalf("sent %d, err %v, want the receiver to stop it", sent, err)
		}
		if complete := <-b.finished; complete {
			t.Fatal("B called the transfer complete")
		}
		nothingKept(t, b)
	})
	t.Run("seen by the sender", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		a, b, toB, _ := wiredPair(t)
		a.ways.set(relayed, straight)
		b.ways.set(straight, relayed)
		returned := make(chan struct{})
		pctx := localsend.WithProgress(ctx, held(func() { a.ways.set(relayed, relayed) }, returned))
		sent, err := a.SendPaired(pctx, toB, []string{file(t, big)})
		close(returned)
		if !errors.Is(err, ErrNoDirectPath) || sent != 0 {
			t.Fatalf("sent %d, err %v, want ErrNoDirectPath", sent, err)
		}
		if complete := <-b.finished; complete {
			t.Fatal("B called the transfer complete")
		}
		nothingKept(t, b)
	})
}

// nothingKept fails if B's save folder holds anything, a part of a file
// included, once the stopped file has been let go: a transfer is finished
// for its person before the file on the move is cleared away.
func nothingKept(t *testing.T, b *wired) {
	t.Helper()
	until(t, "B's save folder to be empty", func() bool {
		left, err := os.ReadDir(b.Receiver.SaveDir)
		if err != nil {
			t.Fatal(err)
		}
		return len(left) == 0
	})
}

// Either person cancels in the middle of a fetched file: both ends stop.
func TestFetchCancelledFromEitherEnd(t *testing.T) {
	t.Run("by the sender", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		a, b, toB, _ := wiredPair(t)
		a.ways.set(relayed, straight)
		b.ways.set(straight, relayed)
		sctx, stop := context.WithCancel(ctx)
		defer stop()
		returned := make(chan struct{})
		pctx := localsend.WithProgress(sctx, held(stop, returned))
		sent, err := a.SendPaired(pctx, toB, []string{file(t, big)})
		close(returned)
		if !errors.Is(err, context.Canceled) || sent != 0 {
			t.Fatalf("sent %d, err %v, want it cancelled", sent, err)
		}
		if complete := <-b.finished; complete {
			t.Fatal("B called the transfer complete")
		}
		nothingKept(t, b)
	})
	t.Run("by the receiver", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		a, b, toB, _ := wiredPair(t)
		a.ways.set(relayed, straight)
		b.ways.set(straight, relayed)
		gone := make(chan struct{})
		close(gone) // the cancel is all there is to wait for
		b.Receiver.Progress = held(b.Receiver.Cancel, gone)
		if sent, err := a.SendPaired(ctx, toB, []string{file(t, big)}); !errors.Is(err, localsend.ErrFetchStopped) || sent != 0 {
			t.Fatalf("sent %d, err %v, want the receiver to stop it", sent, err)
		}
		if complete := <-b.finished; complete {
			t.Fatal("B called the transfer complete")
		}
		nothingKept(t, b)
	})
}

// What A lends to B is B's alone: another paired device that asks with the
// right id, and a device on the local network, get 404, and so does B once
// its connection is no longer direct.
func TestLentFilesAreOnlyForTheirReceiver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB, _ := wiredPair(t)
	c := newWired(t, "C")
	aSeenByC := c.connectTo(a)
	a.ways.set(relayed, straight)
	b.ways.set(straight, relayed)

	// get asks A for the file of the offer, through a port of A's.
	get := func(port int, mark string) int {
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // the test knows who is there
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				return &c.sender.Cert, nil
			},
		}}}
		defer client.CloseIdleConnections()
		statuses := 0
		for _, request := range []string{"POST /api/localsend/v2/prepare-download?sessionId=", "GET /api/localsend/v2/download?fileId=0&sessionId="} {
			method, path, _ := strings.Cut(request, " ")
			req, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("https://127.0.0.1:%d%s%s", port, path, mark), nil)
			if err != nil {
				t.Error(err)
				return 0
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Error(err)
				return 0
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				return resp.StatusCode
			}
			statuses = resp.StatusCode
		}
		return statuses
	}
	c.mu.Lock()
	tunnelPort := c.to[aSeenByC.Addr]
	c.mu.Unlock()
	var fromC, fromNetwork int
	accept := b.Receiver.Accept
	b.Receiver.Accept = func(ctx context.Context, o localsend.Offer) []string {
		// The loan is open, and its list has been given to B.
		fromC = get(tunnelPort, o.Fetch)
		fromNetwork = get(a.Info.Port, o.Fetch)
		return accept(ctx, o)
	}
	data := []byte("for B only")
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, data)}); err != nil || sent != 1 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
	arrived(t, b, data)
	if fromC != http.StatusNotFound || fromNetwork != http.StatusNotFound {
		t.Fatalf("another paired device got HTTP %d and the local network HTTP %d, want 404 for both", fromC, fromNetwork)
	}

	// The same send again, but B's connection is no longer direct, as A
	// sees it, when B comes for the file. A's guard is kept from looking,
	// so that it is the request that is refused; then B's closing word is
	// refused too, and A's send ends on B's silence.
	<-b.finished
	a.rule.guards = time.Hour
	a.lender.IdleTime = 50 * time.Millisecond
	b.Receiver.Accept = func(ctx context.Context, o localsend.Offer) []string {
		a.ways.set(relayed, relayed)
		return accept(ctx, o)
	}
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, data)}); !errors.Is(err, localsend.ErrFetchQuiet) || sent != 0 {
		t.Fatalf("with B's connection relayed: sent %d, err %v, want nothing sent", sent, err)
	}
	if said := a.said("B"); !strings.Contains(said, "/download?fileId=0") || !strings.Contains(said, "-> 404") {
		t.Fatalf("B's request over a relayed connection was not refused:\n%s", said)
	}
}

// A paired device that has started again says so with its next question,
// and the connection kept to it from before, which is dead, is replaced at
// once and not at the next send.
func TestConnectionIsRenewedWhenTheDeviceStartsAgain(t *testing.T) {
	a, b, bSeenByA, aSeenByB := wiredPair(t)
	ctx := context.Background()
	var mu sync.Mutex
	made := 0
	connect := a.connect
	a.connect = func(addr tunnel.Addr, keys *tunnel.Keys) (link, error) {
		mu.Lock()
		made++
		mu.Unlock()
		return connect(addr, keys)
	}
	connections := func() int {
		mu.Lock()
		defer mu.Unlock()
		return made
	}
	if !a.Online(ctx, bSeenByA) || !b.Online(ctx, aSeenByB) || !b.Online(ctx, aSeenByB) {
		t.Fatal("the two devices do not reach each other")
	}
	if got := connections(); got != 1 {
		t.Fatalf("A made %d connections to B while B stayed up, want the one", got)
	}
	// B starts again: the same device, a new run.
	b.started = "another run"
	if !b.Online(ctx, aSeenByB) {
		t.Fatal("B does not reach A")
	}
	until(t, "A to make a new connection to B", func() bool { return connections() == 2 })
}
