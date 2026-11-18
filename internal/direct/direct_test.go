package direct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Several megabytes go each way at one time, and arrive as they were sent.
func TestBytesArriveInOrderBothWays(t *testing.T) {
	a, b := connect(t)
	ca, cb := pipe(t, a, b)
	const size = 6 << 20

	type end struct {
		c    *conn
		seed uint64
	}
	errs := make(chan error, 4)
	for _, e := range []struct{ from, to end }{{end{ca, 1}, end{cb, 1}}, {end{cb, 2}, end{ca, 2}}} {
		data := noise(e.from.seed, size)
		go func() {
			_, err := e.from.c.Write(data)
			errs <- err
		}()
		go func() {
			sum, err := readSum(e.to.c, size)
			if err == nil && sum != sha256.Sum256(data) {
				err = errors.New("the bytes that arrived are not the bytes sent")
			}
			errs <- err
		}()
	}
	for range 4 {
		if err := within(t, "the transfer", func() error { return <-errs }); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []*conn{ca, cb} {
		if m := c.maxBuffered.Load(); m > highWater+chunk {
			t.Errorf("%d bytes waited to be sent, above the limit", m)
		}
	}
}

// A reader that does not read stops the writer, with no more than the
// limit waiting to be sent; when it reads, everything arrives. In a
// browser the writer stops only for a time: see receiverCanWait.
func TestASlowReaderHoldsTheWriterBack(t *testing.T) {
	a, b := connect(t)
	ca, cb := pipe(t, a, b)
	const size = 8 << 20
	data := noise(3, size)

	wrote := make(chan error, 1)
	go func() {
		_, err := ca.Write(data)
		wrote <- err
	}()
	// The reader's hook: it starts only once the writer has stopped.
	until(t, "the writer to stop", func() bool { return ca.waits.Load() > 0 })
	if receiverCanWait {
		select {
		case err := <-wrote:
			t.Fatalf("Write returned with nothing read: %v", err)
		default:
		}
	}
	if m := ca.maxBuffered.Load(); m <= highWater {
		t.Errorf("the writer stopped at %d bytes, not above the limit", m)
	}

	sum := within(t, "the bytes", func() [32]byte {
		s, err := readSum(cb, size)
		if err != nil {
			t.Error(err)
		}
		return s
	})
	if sum != sha256.Sum256(data) {
		t.Error("the bytes that arrived are not the bytes sent")
	}
	if err := within(t, "Write", func() error { return <-wrote }); err != nil {
		t.Fatal(err)
	}
	if m := ca.maxBuffered.Load(); m > highWater+chunk {
		t.Errorf("%d bytes waited to be sent, above the limit", m)
	}
	t.Logf("the writer stopped %d times; at most %d bytes waited", ca.waits.Load(), ca.maxBuffered.Load())
}

// Close at one end ends a Read and a Write that wait at the other,
// whichever end opened the connection.
func TestCloseUnblocksTheOtherEnd(t *testing.T) {
	a, b := connect(t)
	for _, closer := range []string{"the end that dialled", "the end that accepted"} {
		t.Run(closer, func(t *testing.T) {
			near, far := pipe(t, a, b)
			if closer == "the end that accepted" {
				near, far = far, near
			}
			rd, wr := make(chan result, 1), make(chan result, 1)
			go func() { n, err := far.Read(make([]byte, 1)); rd <- result{n, err} }()
			// near reads nothing, so far's Write stops in time. It writes
			// until it fails: in a browser it does not stop for good.
			go func() {
				for {
					if n, err := far.Write(make([]byte, 1<<20)); err != nil {
						wr <- result{n, err}
						return
					}
				}
			}()
			until(t, "Write to stop", func() bool { return far.waits.Load() > 0 })

			if err := near.Close(); err != nil {
				t.Fatal(err)
			}
			// io.EOF, or in a browser an error: bytes of far's Write
			// were still waiting to be sent, and are lost.
			if r := within(t, "Read", func() result { return <-rd }); r.err == nil || (receiverCanWait && r.err != io.EOF) {
				t.Errorf("Read = %d, %v, want io.EOF", r.n, r.err)
			}
			if r := within(t, "Write", func() result { return <-wr }); r.err == nil {
				t.Error("Write returned no error")
			}
		})
	}
}

// What was written before Close arrives, and then the end.
func TestCloseSendsWhatWasWritten(t *testing.T) {
	a, b := connect(t)
	ca, cb := pipe(t, a, b)
	data := noise(4, 900<<10)
	if _, err := ca.Write(data); err != nil {
		t.Fatal(err)
	}
	ca.Close()
	got := within(t, "the bytes", func() []byte { b, _ := io.ReadAll(cb); return b })
	if !bytes.Equal(got, data) {
		t.Fatalf("%d of %d bytes arrived before the end", len(got), len(data))
	}
}

func TestDeadlinesOnAChannel(t *testing.T) {
	a, b := connect(t)
	ca, cb := pipe(t, a, b)

	ca.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	r := within(t, "the read deadline", func() result { n, err := ca.Read(make([]byte, 1)); return result{n, err} })
	if !errors.Is(r.err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read = %v, want a timeout", r.err)
	}

	if !receiverCanWait {
		return // a Write stops for good only where the reader can hold it
	}
	// cb reads nothing, so the Write stops and its deadline ends it.
	ca.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	r = within(t, "the write deadline", func() result { n, err := ca.Write(make([]byte, 16<<20)); return result{n, err} })
	if !errors.Is(r.err, os.ErrDeadlineExceeded) {
		t.Fatalf("Write = %v, want a timeout", r.err)
	}
	if r.n == 0 || r.n%chunk != 0 {
		t.Errorf("Write reports %d bytes sent", r.n)
	}

	// The conn is still good: the bytes sent arrive, and it takes more.
	ca.SetDeadline(time.Time{})
	sent := r.n
	go ca.Write([]byte("more"))
	got := within(t, "the bytes", func() []byte {
		b := make([]byte, sent+4)
		io.ReadFull(cb, b)
		return b
	})
	if string(got[sent:]) != "more" {
		t.Errorf("after the deadline the stream ends in %q", got[sent:])
	}
}

func TestSeveralConnsOverOnePeer(t *testing.T) {
	a, b := connect(t)
	const conns, size = 8, 512 << 10

	// b sends back what it gets, on each connection.
	go func() {
		for {
			c, err := b.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	errs := make(chan error, conns)
	addrs := map[string]bool{}
	var mu sync.Mutex
	for i := range conns {
		go func() {
			errs <- func() error {
				c, err := a.Dial(ctx)
				if err != nil {
					return err
				}
				defer c.Close()
				mu.Lock()
				addrs[c.RemoteAddr().String()] = true
				mu.Unlock()
				data := noise(uint64(10+i), size)
				go c.Write(data)
				sum, err := readSum(c, size)
				if err != nil {
					return err
				}
				if sum != sha256.Sum256(data) {
					return fmt.Errorf("connection %d got another connection's bytes", i)
				}
				return nil
			}()
		}()
	}
	for range conns {
		if err := within(t, "the connections", func() error { return <-errs }); err != nil {
			t.Fatal(err)
		}
	}
	if len(addrs) != conns {
		t.Errorf("%d addresses for %d connections", len(addrs), conns)
	}
}

func TestPathSaysTheKindOfAddress(t *testing.T) {
	a, b := connect(t)
	for _, p := range []*Peer{a, b} {
		// A browser hides its local address, and the other end then
		// learns it during the checks: "prflx".
		path, ok := p.Path()
		if !ok || path.Local != "host" || (path.Remote != "host" && path.Remote != "prflx") {
			t.Errorf("Path = %+v, %v", path, ok)
		}
	}
}

func TestADescriptionThatIsNotValidIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	a, offer, err := Offer(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, answer, err := Answer(ctx, local, offer)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if len(offer) > MaxDescription/4 {
		t.Errorf("an offer over loopback has %d bytes; the limit is %d", len(offer), MaxDescription)
	}

	long := bytes.Replace(offer, []byte(`"sdp":"`), []byte(`"sdp":"`+strings.Repeat(" ", MaxDescription)), 1)
	for name, bad := range map[string][]byte{
		"empty":                nil,
		"not JSON":             []byte("v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\n"),
		"no description in it": []byte(`{"type":"offer","sdp":"hello"}`),
		"lines that are wrong": []byte(`{"type":"offer","sdp":"v=0\r\na=fingerprint:sha-256 nothing\r\nm=what\r\n"}`),
		"an answer":            answer,
		"too long":             long,
	} {
		if p, _, err := Answer(ctx, local, bad); !errors.Is(err, ErrDescription) {
			t.Errorf("Answer to an offer that is %s: %v", name, err)
			if p != nil {
				p.Close()
			}
		} else if strings.Contains(err.Error(), "fingerprint") || strings.Contains(err.Error(), "hello") {
			t.Errorf("the error quotes the description: %v", err)
		}
	}
	for name, bad := range map[string][]byte{
		"empty":    nil,
		"not JSON": []byte("{"),
		"an offer": offer,
		"too long": bytes.Replace(long, []byte(`"offer"`), []byte(`"answer"`), 1),
	} {
		if err := a.SetAnswer(bad); !errors.Is(err, ErrDescription) {
			t.Errorf("SetAnswer with one that is %s: %v", name, err)
		}
	}

	// After all that the right answer still connects.
	if err := a.SetAnswer(answer); err != nil {
		t.Fatal(err)
	}
	if err := a.Connected(ctx); err != nil {
		t.Fatal(err)
	}
}

// An answer whose fingerprint is not the answering end's certificate: the
// addresses are right and the checks pass, but no connection forms.
func TestAWrongFingerprintDoesNotConnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	a, offer, err := Offer(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, answer, err := Answer(ctx, local, offer)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	const mark = "a=fingerprint:sha-256 "
	i := bytes.Index(answer, []byte(mark))
	if i < 0 {
		t.Fatal("no fingerprint in the answer")
	}
	i += len(mark)
	forged := bytes.Clone(answer)
	forged[i] = map[bool]byte{true: '1', false: '0'}[forged[i] == '0']
	if err := a.SetAnswer(forged); err != nil {
		t.Fatal(err)
	}
	err = within(t, "the Peer to end", func() error { return a.Connected(ctx) })
	if !errors.Is(err, ErrLost) && !errors.Is(err, ErrTimeout) {
		t.Fatalf("Connected = %v", err)
	}
}

func TestATURNAddressIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	for _, addr := range []string{"turn:turn.example.org:3478", "turns:turn.example.org:5349", "TURN:turn.example.org"} {
		cfg := Config{STUN: []string{"stun:stun.example.org:3478", addr}, Loopback: true}
		if _, _, err := Offer(ctx, cfg); !errors.Is(err, ErrTURN) {
			t.Errorf("Offer with %s: %v", addr, err)
		}
		if _, _, err := Answer(ctx, cfg, []byte(`{}`)); !errors.Is(err, ErrDescription) && !errors.Is(err, ErrTURN) {
			t.Errorf("Answer with %s: %v", addr, err)
		}
	}
	if _, _, err := Offer(ctx, Config{STUN: []string{"stun.example.org:3478"}}); err == nil {
		t.Error("Offer took an address with no scheme")
	}
}

// The other end never answers: the Peer ends by itself, and every call
// that waits on it returns.
func TestConnectTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	a, _, err := Offer(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	long := context.Background() // the Peer's own limit must end the waits
	calls := map[string]func() error{
		"Connected": func() error { return a.Connected(long) },
		"Dial":      func() error { _, err := a.Dial(long); return err },
		"Accept":    func() error { _, err := a.Accept(); return err },
	}
	errs := map[string]chan error{}
	for name, call := range calls {
		ch := make(chan error, 1)
		errs[name] = ch
		go func() { ch <- call() }()
	}
	for name, ch := range errs {
		if err := within(t, name, func() error { return <-ch }); !errors.Is(err, ErrTimeout) {
			t.Errorf("%s = %v, want ErrTimeout", name, err)
		}
	}
	within(t, "Done", func() bool { <-a.Done(); return true })
	if !errors.Is(a.Err(), ErrTimeout) {
		t.Errorf("Err = %v", a.Err())
	}
}

// Dial gives up with its own context while the Peer still waits.
func TestDialKeepsToItsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	a, _, err := Offer(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	err = within(t, "Dial", func() error { _, err := a.Dial(short); return err })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial = %v", err)
	}
}

// Closing one Peer ends the other, and the conns of both.
func TestClosingAPeerEndsBothEnds(t *testing.T) {
	a, b := connect(t)
	ca, cb := pipe(t, a, b)
	rd := make(chan result, 1)
	go func() { n, err := cb.Read(make([]byte, 1)); rd <- result{n, err} }()

	a.Close()
	if _, err := ca.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Read at the end that closed = %v", err)
	}
	if r := within(t, "Read at the other end", func() result { return <-rd }); r.err == nil {
		t.Error("Read at the other end gave no error")
	}
	within(t, "the other Peer to end", func() bool { <-b.Done(); return true })
	if !errors.Is(b.Err(), ErrLost) {
		t.Errorf("the other Peer's Err = %v", b.Err())
	}
	if _, err := b.Accept(); !errors.Is(err, ErrLost) {
		t.Errorf("Accept = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	if _, err := a.Dial(ctx); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Dial on a closed Peer = %v", err)
	}
}

// A panic in a callback becomes the error of the Peer and of its conns.
func TestAPanicInACallbackEndsThePeerWithAnError(t *testing.T) {
	a, b := connect(t)
	ca, _ := pipe(t, a, b)
	func() {
		defer a.guard("a test")
		panic("boom")
	}()
	if err := a.Err(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Err = %v", err)
	}
	for what, call := range map[string]func() (int, error){
		"Read":  func() (int, error) { return ca.Read(make([]byte, 1)) },
		"Write": func() (int, error) { return ca.Write([]byte("x")) },
	} {
		if _, err := call(); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%s = %v", what, err)
		}
	}
}
