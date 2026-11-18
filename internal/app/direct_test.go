package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yonder/internal/localsend"
	"yonder/internal/nettest"
	"yonder/internal/pairing"
	"yonder/internal/tunnel"
)

// wait is the longest any step of a test here may take.
const wait = 10 * time.Second

// fakePath is a path a test sets by hand, in place of asking the tunnel,
// and a count of how often it was asked.
type fakePath struct {
	mu    sync.Mutex
	now   tunnel.Path
	asked int
	// after, if set, is called with the count each time the path is asked,
	// with the lock held, so a test can change the path at a known moment.
	after func(asked int)
}

func (f *fakePath) path(context.Context, pairing.Device, bool) tunnel.Path {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	if f.after != nil {
		f.after(f.asked)
	}
	return f.now
}

// rule is the rule over a fake path, with its waits cut short so tests are
// quick; the long wait stays long, so a slow machine cannot run it out.
func (f *fakePath) rule() pathRule {
	return pathRule{path: f.path, wait: wait, every: time.Millisecond, guards: time.Millisecond}
}

func TestOnlyWithABrowserMayTheRelayBeUsed(t *testing.T) {
	for _, c := range []struct {
		mine, theirs string
		want         bool
	}{
		{"desktop", "desktop", false},
		{"desktop", "mobile", false},
		{"desktop", "", false}, // a version from before the rule, or not yet asked
		{"desktop", TypeWeb, true},
		{TypeWeb, "desktop", true},
		{TypeWeb, "", true},
		{TypeWeb, TypeWeb, true},
	} {
		if got := mayRelay(c.mine, c.theirs); got != c.want {
			t.Errorf("mayRelay(%q, %q) = %v, want %v", c.mine, c.theirs, got, c.want)
		}
	}
}

func TestAwaitDirect(t *testing.T) {
	t.Run("direct already", func(t *testing.T) {
		f := &fakePath{now: tunnel.PathDirect}
		if !f.rule().awaitDirect(context.Background(), pairing.Device{}, true) {
			t.Fatal("a direct path was not taken as one")
		}
	})
	t.Run("relayed, then direct", func(t *testing.T) {
		f := &fakePath{now: tunnel.PathRelayed}
		f.after = func(asked int) {
			if asked == 3 {
				f.now = tunnel.PathDirect
			}
		}
		if !f.rule().awaitDirect(context.Background(), pairing.Device{}, true) {
			t.Fatal("a path that became direct was not waited for")
		}
	})
	for name, path := range map[string]tunnel.Path{"relayed": tunnel.PathRelayed, "unknown": tunnel.PathUnknown} {
		t.Run("never direct: "+name, func(t *testing.T) {
			f := &fakePath{now: path}
			r := f.rule()
			r.wait = 20 * time.Millisecond
			if r.awaitDirect(context.Background(), pairing.Device{}, true) {
				t.Fatalf("a path that stayed %s was taken as direct", name)
			}
		})
	}
	t.Run("a look that gets no answer", func(t *testing.T) {
		// The first look hangs until its ctx ends, as the tunnel's does
		// when the answer is lost; the path is direct at the next.
		var asked atomic.Int32
		r := pathRule{wait: wait, every: time.Millisecond, path: func(ctx context.Context, _ pairing.Device, _ bool) tunnel.Path {
			if asked.Add(1) == 1 {
				<-ctx.Done()
				return tunnel.PathUnknown
			}
			return tunnel.PathDirect
		}}
		if !r.awaitDirect(context.Background(), pairing.Device{}, true) {
			t.Fatal("one look without an answer took up the whole wait")
		}
	})
	t.Run("given up", func(t *testing.T) {
		f := &fakePath{now: tunnel.PathRelayed}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if f.rule().awaitDirect(ctx, pairing.Device{}, true) {
			t.Fatal("a wait that was called off reported a direct path")
		}
	})
}

// guarded runs the guard over f until the test ends, and returns a channel
// that is closed if the guard stops the transfer.
func guarded(t *testing.T, f *fakePath) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dropped := make(chan struct{})
	go f.rule().guard(ctx, pairing.Device{}, true, func() { close(dropped) })
	return dropped
}

// askedAgain waits until the guard has looked at the path n more times.
func (f *fakePath) askedAgain(t *testing.T, n int) {
	t.Helper()
	f.mu.Lock()
	target := f.asked + n
	f.mu.Unlock()
	deadline := time.Now().Add(wait)
	for {
		f.mu.Lock()
		asked := f.asked
		f.mu.Unlock()
		if asked >= target {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the guard looked at the path %d times, want %d", asked, target)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGuardStopsATransferThatDropsToTheRelay(t *testing.T) {
	f := &fakePath{now: tunnel.PathDirect}
	dropped := guarded(t, f)
	f.askedAgain(t, 5)
	select {
	case <-dropped:
		t.Fatal("a transfer on a direct path was stopped")
	default:
	}
	f.mu.Lock()
	f.now = tunnel.PathRelayed
	f.mu.Unlock()
	select {
	case <-dropped:
	case <-time.After(wait):
		t.Fatal("a transfer that dropped to the relay was not stopped")
	}
}

func TestGuardLeavesAnUnknownPathAlone(t *testing.T) {
	f := &fakePath{now: tunnel.PathUnknown}
	dropped := guarded(t, f)
	f.askedAgain(t, 10)
	select {
	case <-dropped:
		t.Fatal("a transfer whose path could not be learned was stopped")
	default:
	}
}

// One look at the relay between direct ones is a path changing over, and
// is not enough to stop a transfer.
func TestGuardForgivesOneRelayedLook(t *testing.T) {
	f := &fakePath{now: tunnel.PathDirect}
	f.after = func(asked int) {
		if asked%2 == 0 {
			f.now = tunnel.PathRelayed
		} else {
			f.now = tunnel.PathDirect
		}
	}
	dropped := guarded(t, f)
	f.askedAgain(t, 20)
	select {
	case <-dropped:
		t.Fatal("a transfer was stopped though it was never on the relay twice in a row")
	default:
	}
}

// The rule over the real relay: two apps on one machine, which find a
// direct path, send as before; with either side's path forced to "relayed"
// nothing is sent; and to a device that says it is a browser the relay is
// allowed.
func TestNoRelayBetweenApps(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	a, b := newNode(t, "A"), newNode(t, "B")
	openTunnel(t, ctx, a)
	openTunnel(t, ctx, b)
	p, err := a.Pair(ctx, func(context.Context, pairing.Device) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	toA, err := b.Join(ctx, p.Link)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := p.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// A takes offers as the window does: only those the rule admits.
	var admitted, refused atomic.Int32
	a.Receiver.Accept = func(ctx context.Context, o localsend.Offer) []string {
		if ok, _ := a.AdmitOffer(ctx, o); !ok {
			refused.Add(1)
			return nil
		}
		admitted.Add(1)
		return []string{o.Files[0].ID}
	}
	path := filepath.Join(t.TempDir(), "far.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("yonder "), 10_000), 0o644); err != nil {
		t.Fatal(err)
	}
	relayed := func(context.Context, pairing.Device, bool) tunnel.Path { return tunnel.PathRelayed }
	real := b.rule

	// As they are: a direct path forms and the file goes.
	if sent, err := b.SendPaired(ctx, toA, []string{path}); err != nil || sent != 1 {
		t.Fatalf("send over a direct path: sent %d, err %v", sent, err)
	}

	// The sender finds no direct path: it gives up without offering.
	b.rule = pathRule{path: relayed, wait: 50 * time.Millisecond, every: time.Millisecond, guards: guardEvery}
	if sent, err := b.SendPaired(ctx, toA, []string{path}); !errors.Is(err, ErrNoDirectPath) || sent != 0 {
		t.Fatalf("send with the sender's path relayed: sent %d, err %v, want ErrNoDirectPath", sent, err)
	}
	if got := admitted.Load() + refused.Load(); got != 1 {
		t.Fatalf("A was offered files %d times, want once", got)
	}

	// A browser at the other end: the relay is allowed.
	b.met(toA.Fingerprint, TypeWeb)
	b.mu.Lock()
	b.sending[toA.Fingerprint]++ // so the send does not ask A what it is again
	b.mu.Unlock()
	sent, err := b.SendPaired(ctx, toA, []string{path})
	b.mu.Lock()
	b.sending[toA.Fingerprint]--
	b.mu.Unlock()
	if err != nil || sent != 1 {
		t.Fatalf("send to a browser with the path relayed: sent %d, err %v", sent, err)
	}

	// The receiver finds no direct path (as with a sender from before the
	// rule): it refuses the offer.
	b.rule = real
	a.rule = pathRule{path: relayed, wait: 50 * time.Millisecond, every: time.Millisecond, guards: guardEvery}
	before := refused.Load()
	if sent, err := b.SendPaired(ctx, toA, []string{path}); !errors.Is(err, localsend.ErrDeclined) || sent != 0 {
		t.Fatalf("send with the receiver's path relayed: sent %d, err %v, want declined", sent, err)
	}
	if refused.Load() != before+1 {
		t.Fatal("A did not refuse the offer")
	}
}

// A send through the real relay where only the receiver's connection may
// carry the files: A's own path is forced to "relayed", and everything else
// is as it is, so B really looks, A's tunnel really sees B's connection go
// direct, and B really fetches. How long A's tunnel takes to show B's
// connection as direct is reported, not judged: it depends on the network.
func TestSendOverTheOtherConnection(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	a, b, _, toB := pairedNodes(t, ctx)
	var marked atomic.Int32
	b.Receiver.Accept = func(ctx context.Context, o localsend.Offer) []string {
		if ok, _ := b.AdmitOffer(ctx, o); !ok {
			return nil
		}
		if o.Fetch != "" {
			marked.Add(1)
		}
		return []string{o.Files[0].ID}
	}
	real := a.rule.path
	var began atomic.Int64 // when the send began, in nanoseconds
	var shown atomic.Int64 // nanoseconds from the start of a send to the first direct look at B's connection
	a.rule.path = func(ctx context.Context, d pairing.Device, outgoing bool) tunnel.Path {
		if outgoing {
			return tunnel.PathRelayed
		}
		path := real(ctx, d, false)
		if path == tunnel.PathDirect {
			shown.CompareAndSwap(0, time.Now().UnixNano()-began.Load())
		}
		return path
	}
	data := bytes.Repeat([]byte("yonder "), 100_000)
	path := filepath.Join(t.TempDir(), "far.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, send := range []string{"the first send, B not yet connected to A", "the second send"} {
		shown.Store(0)
		start := time.Now()
		began.Store(start.UnixNano())
		sent, err := a.SendPaired(ctx, toB, []string{path})
		if err != nil || sent != 1 {
			t.Fatalf("%s: sent %d, err %v", send, sent, err)
		}
		t.Logf("%s: A's tunnel showed B's connection as direct after %v; the send took %v",
			send, time.Duration(shown.Load()).Round(10*time.Millisecond), time.Since(start).Round(10*time.Millisecond))
	}
	if got := marked.Load(); got != 2 {
		t.Fatalf("B fetched %d of the 2 sends, want both", got)
	}
	for _, name := range []string{"far.bin", "far (1).bin"} {
		got, err := os.ReadFile(filepath.Join(b.Receiver.SaveDir, name))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s arrived as %d bytes, err %v, want the %d sent", name, len(got), err, len(data))
		}
	}
}
