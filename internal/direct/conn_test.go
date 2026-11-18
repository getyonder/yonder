package direct

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// fake is a data channel whose send buffer the test sets by hand.
type fake struct {
	mu       sync.Mutex
	sent     [][]byte
	buffered uint64
	looks    int // calls of BufferedAmount
	closed   bool
}

func (f *fake) Send(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("closed")
	}
	f.sent = append(f.sent, bytes.Clone(b))
	return nil
}

func (f *fake) BufferedAmount() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.looks++
	return f.buffered
}

func (f *fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fake) set(n uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buffered = n
}

func (f *fake) seen() (looks, messages int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.looks, len(f.sent)
}

func TestWriteCutsIntoMessagesOfAChunkAtMost(t *testing.T) {
	f := &fake{}
	c := newConn(f, 1)
	data := noise(1, 5*chunk+123)
	if n, err := c.Write(data); n != len(data) || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if len(f.sent) != 6 {
		t.Fatalf("%d messages, want 6", len(f.sent))
	}
	for i, m := range f.sent {
		if len(m) > chunk {
			t.Errorf("message %d has %d bytes", i, len(m))
		}
	}
	if !bytes.Equal(bytes.Join(f.sent, nil), data) {
		t.Error("the messages are not the bytes written, in order")
	}
}

// Write stops above highWater, and a fall that does not reach lowWater
// does not start it again.
func TestWriteWaitsFromHighWaterToLowWater(t *testing.T) {
	f := &fake{}
	c := newConn(f, 1)
	f.set(highWater) // at the limit is not above it
	if _, err := c.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}

	f.set(highWater + 1)
	done := make(chan result, 1)
	go func() {
		n, err := c.Write([]byte("b"))
		done <- result{n, err}
	}()
	until(t, "Write to stop", func() bool { return c.waits.Load() == 1 })

	// An event while the amount is still above lowWater: Write looks, and
	// goes on waiting.
	f.set(lowWater + 1)
	looks, _ := f.seen()
	c.low()
	until(t, "Write to look again", func() bool { l, _ := f.seen(); return l > looks })
	if _, sent := f.seen(); sent != 1 {
		t.Fatalf("Write sent above lowWater: %d messages", sent)
	}

	f.set(lowWater)
	c.low()
	if r := within(t, "Write to go on", func() result { return <-done }); r.n != 1 || r.err != nil {
		t.Fatalf("Write = %d, %v", r.n, r.err)
	}
}

func TestReadGivesTheMessagesAsOneStream(t *testing.T) {
	c := newConn(&fake{}, 1)
	go func() {
		c.receive([]byte("hel"), false)
		c.receive([]byte("lo wor"), false)
		c.receive([]byte("ld"), false)
		c.closedByPeer()
	}()
	got := within(t, "the stream", func() []byte { b, _ := io.ReadAll(c); return b })
	if string(got) != "hello world" {
		t.Fatalf("read %q", got)
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("Write after the other end closed: %v", err)
	}
}

func TestAMessageThatIsNotAllowedEndsTheConn(t *testing.T) {
	for name, send := range map[string]func(c *conn){
		"text":     func(c *conn) { c.receive([]byte("hi"), true) },
		"too long": func(c *conn) { c.receive(make([]byte, maxMessage+1), false) },
	} {
		f := &fake{}
		c := newConn(f, 1)
		send(c)
		if _, err := c.Read(make([]byte, 1)); err == nil || err == io.EOF {
			t.Errorf("%s: Read gave %v", name, err)
		}
		if !f.closed {
			t.Errorf("%s: the channel was not closed", name)
		}
	}
}

// The receiver holds the channel back while readLimit bytes are unread.
func TestReceiveWaitsForRead(t *testing.T) {
	if !receiverCanWait {
		t.Skip("this build cannot hold a channel back")
	}
	c := newConn(&fake{}, 1)
	for range readLimit/chunk - 1 {
		c.receive(make([]byte, chunk), false)
	}
	done := make(chan bool, 1)
	go func() {
		c.receive(make([]byte, chunk), false) // this one fills it
		done <- true
	}()
	until(t, "the last message", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.size == readLimit
	})
	select {
	case <-done:
		t.Fatal("receive returned with the limit reached")
	default:
	}
	if _, err := io.ReadFull(c, make([]byte, chunk)); err != nil {
		t.Fatal(err)
	}
	within(t, "receive to return", func() bool { return <-done })
}

func TestDeadlines(t *testing.T) {
	f := &fake{}
	c := newConn(f, 1)

	c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	r := within(t, "the read deadline", func() result {
		n, err := c.Read(make([]byte, 1))
		return result{n, err}
	})
	var ne net.Error
	if !errors.Is(r.err, os.ErrDeadlineExceeded) || !errors.As(r.err, &ne) || !ne.Timeout() {
		t.Fatalf("Read = %v, want a timeout", r.err)
	}
	// A deadline in the past fails a Read even when bytes are there, and
	// taking it away makes the conn good again.
	c.receive([]byte("x"), false)
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read after the deadline = %v", err)
	}
	c.SetReadDeadline(time.Time{})
	if n, err := c.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("Read with no deadline = %d, %v", n, err)
	}

	f.set(highWater + 1)
	c.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
	r = within(t, "the write deadline", func() result {
		n, err := c.Write([]byte("x"))
		return result{n, err}
	})
	if !errors.Is(r.err, os.ErrDeadlineExceeded) {
		t.Fatalf("Write = %v, want a timeout", r.err)
	}
	f.set(0)
	c.SetDeadline(time.Time{})
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("Write with no deadline = %v", err)
	}
}

func TestCloseUnblocksItsOwnReadAndWrite(t *testing.T) {
	f := &fake{}
	c := newConn(f, 1)
	f.set(highWater + 1)
	rd, wr := make(chan result, 1), make(chan result, 1)
	go func() { n, err := c.Read(make([]byte, 1)); rd <- result{n, err} }()
	go func() { n, err := c.Write([]byte("x")); wr <- result{n, err} }()
	until(t, "Write to stop", func() bool { return c.waits.Load() == 1 })
	c.Close()
	for what, ch := range map[string]chan result{"Read": rd, "Write": wr} {
		if r := within(t, what, func() result { return <-ch }); !errors.Is(r.err, net.ErrClosed) {
			t.Errorf("%s after Close = %v", what, r.err)
		}
	}
}

func TestAddrs(t *testing.T) {
	c := newConn(&fake{}, 7)
	for _, a := range []net.Addr{c.LocalAddr(), c.RemoteAddr()} {
		if _, port, err := net.SplitHostPort(a.String()); err != nil || port != "7" {
			t.Errorf("%q: port %q, %v", a, port, err)
		}
	}
	if c.LocalAddr().String() == c.RemoteAddr().String() {
		t.Error("both ends have one address")
	}
}
