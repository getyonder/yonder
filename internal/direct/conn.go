package direct

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// chunk is the most one data channel message carries: the size
	// LocalSend's web version uses, and safe in every browser.
	chunk = 16 << 10
	// Write stops while more than highWater is waiting to be sent, and
	// goes on once that has fallen to lowWater.
	highWater = 1 << 20
	lowWater  = 256 << 10
	// maxMessage is the largest message taken from the other end. A
	// Yonder sends chunk at most; anything longer ends the connection.
	maxMessage = 2 * chunk
	// readLimit is how much may wait to be read before the receiving side
	// stops taking messages (where it can: see receiverCanWait).
	readLimit = 1 << 20
)

// errRemoteClosed is what Write gives once the other end closed.
var errRemoteClosed = fmt.Errorf("direct: the other end closed the connection: %w", io.ErrClosedPipe)

// channel is what a conn needs of a data channel; *webrtc.DataChannel in
// both builds, and a stand-in in the tests.
type channel interface {
	Send([]byte) error
	BufferedAmount() uint64
	Close() error
}

// conn makes one data channel a net.Conn: an ordered, reliable stream of
// bytes. Whoever owns the channel feeds it through receive, low, ended and
// fail, from the channel's callbacks.
type conn struct {
	ch            channel
	local, remote addr
	forget        func() // tells the Peer that this conn closed; may be nil

	opened   chan struct{} // closed when the channel is open
	openOnce sync.Once

	mu       sync.Mutex
	in       [][]byte // messages not yet read; in[0] may be partly read
	size     int      // bytes in in
	rerr     error    // what Read gives once in is empty: io.EOF or a failure
	werr     error    // what Write gives, when the channel is no more
	readable chan struct{}
	room     chan struct{} // receive waits here for Read to make room

	lowc      chan struct{} // the amount waiting to be sent fell to lowWater
	wsem      chan struct{} // one Write at a time
	ended     chan struct{} // closed when the channel is no more
	endOnce   sync.Once
	done      chan struct{} // closed by Close
	closeOnce sync.Once
	closeErr  error

	rd, wd deadline

	// For the tests: how often Write stopped for back-pressure, and the
	// most that was waiting to be sent when it looked.
	waits       atomic.Int64
	maxBuffered atomic.Uint64
}

func newConn(ch channel, id int) *conn {
	return &conn{
		ch:       ch,
		local:    addr{"local", id},
		remote:   addr{"remote", id},
		opened:   make(chan struct{}),
		readable: make(chan struct{}, 1),
		room:     make(chan struct{}, 1),
		lowc:     make(chan struct{}, 1),
		wsem:     make(chan struct{}, 1),
		ended:    make(chan struct{}),
		done:     make(chan struct{}),
		rd:       makeDeadline(),
		wd:       makeDeadline(),
	}
}

// signal leaves a token in a channel of size one, if none is there.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (c *conn) open() { c.openOnce.Do(func() { close(c.opened) }) }

// receive takes one message from the channel. Where the receiver can wait
// it does not return while readLimit bytes are unread, which holds the
// channel's reading back and so, in the end, the sender's Write.
func (c *conn) receive(b []byte, text bool) {
	if text || len(b) > maxMessage {
		c.fail(errors.New("direct: the other end sent a message that is not allowed"))
		c.ch.Close()
		return
	}
	if len(b) == 0 {
		return
	}
	c.mu.Lock()
	if c.rerr != nil || isClosed(c.done) {
		c.mu.Unlock()
		return
	}
	c.in = append(c.in, b)
	c.size += len(b)
	full := c.size >= readLimit
	c.mu.Unlock()
	signal(c.readable)

	for receiverCanWait && full {
		select {
		case <-c.room:
		case <-c.done:
			return
		case <-c.ended:
			return
		}
		c.mu.Lock()
		full = c.size >= readLimit
		c.mu.Unlock()
	}
}

// low is the channel's "buffered amount low" event.
func (c *conn) low() { signal(c.lowc) }

// end is the one place a conn stops: Read gives rerr after what is still
// unread, Write gives werr.
func (c *conn) end(rerr, werr error) {
	c.endOnce.Do(func() {
		c.mu.Lock()
		c.rerr, c.werr = rerr, werr
		c.mu.Unlock()
		close(c.ended)
	})
}

// closedByPeer is the channel's close event: the other end closed it, or
// this end did.
func (c *conn) closedByPeer() { c.end(io.EOF, errRemoteClosed) }

// fail ends the conn with an error, for Read and Write alike.
func (c *conn) fail(err error) { c.end(err, err) }

func (c *conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		switch {
		case isClosed(c.done):
			return 0, net.ErrClosed
		case isClosed(c.rd.wait()):
			return 0, os.ErrDeadlineExceeded
		}
		c.mu.Lock()
		if len(c.in) > 0 {
			n := copy(p, c.in[0])
			if n == len(c.in[0]) {
				c.in[0] = nil
				c.in = c.in[1:]
			} else {
				c.in[0] = c.in[0][n:]
			}
			c.size -= n
			more := len(c.in) > 0
			c.mu.Unlock()
			signal(c.room)
			if more {
				signal(c.readable) // for another Read that waits
			}
			return n, nil
		}
		err := c.rerr
		c.mu.Unlock()
		if err != nil {
			return 0, err
		}
		select {
		case <-c.readable:
		case <-c.ended:
		case <-c.done:
		case <-c.rd.wait():
		}
	}
}

func (c *conn) Write(p []byte) (int, error) {
	select {
	case c.wsem <- struct{}{}:
		defer func() { <-c.wsem }()
	case <-c.done:
		return 0, net.ErrClosed
	case <-c.wd.wait():
		return 0, os.ErrDeadlineExceeded
	}
	n := 0
	for len(p) > 0 {
		if err := c.waitToSend(); err != nil {
			return n, err
		}
		m := min(len(p), chunk)
		if err := c.ch.Send(p[:m]); err != nil {
			if werr := c.writeErr(); werr != nil {
				return n, werr
			}
			return n, err
		}
		n += m
		p = p[m:]
	}
	return n, nil
}

// writeErr says why Write cannot go on, or nil when it can.
func (c *conn) writeErr() error {
	switch {
	case isClosed(c.done):
		return net.ErrClosed
	case isClosed(c.ended):
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.werr
	case isClosed(c.wd.wait()):
		return os.ErrDeadlineExceeded
	}
	return nil
}

// waitToSend returns when one more message may be sent: at once while no
// more than highWater is waiting, otherwise when the channel reports that
// the amount fell to lowWater.
func (c *conn) waitToSend() error {
	if err := c.writeErr(); err != nil {
		return err
	}
	b := c.ch.BufferedAmount()
	for {
		old := c.maxBuffered.Load()
		if b <= old || c.maxBuffered.CompareAndSwap(old, b) {
			break
		}
	}
	if b <= highWater {
		return nil
	}
	c.waits.Add(1)
	for {
		select {
		case <-c.lowc:
		case <-c.done:
		case <-c.ended:
		case <-c.wd.wait():
		}
		if err := c.writeErr(); err != nil {
			return err
		}
		// A token can be an old one, from an earlier fall, so look.
		if c.ch.BufferedAmount() <= lowWater {
			return nil
		}
	}
}

// Close closes the channel. What was written before is still sent; a Read
// or Write that waits, at either end, returns.
func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.closeErr = c.ch.Close()
		if c.forget != nil {
			c.forget()
		}
	})
	return c.closeErr
}

func (c *conn) LocalAddr() net.Addr  { return c.local }
func (c *conn) RemoteAddr() net.Addr { return c.remote }

func (c *conn) SetDeadline(t time.Time) error {
	c.rd.set(t)
	c.wd.set(t)
	return nil
}

func (c *conn) SetReadDeadline(t time.Time) error {
	c.rd.set(t)
	return nil
}

func (c *conn) SetWriteDeadline(t time.Time) error {
	c.wd.set(t)
	return nil
}

// addr is the address of one end of a conn. A data channel has no address
// of its own, and the real ones (see Peer.Path) are not for the rest of
// Yonder to keep, so this only says which end and which connection, in the
// "host:port" form that net.SplitHostPort takes.
type addr struct {
	end string // "local" or "remote"
	id  int    // counts the connections of one Peer
}

func (a addr) Network() string { return "direct" }
func (a addr) String() string  { return a.end + ".direct:" + strconv.Itoa(a.id) }

// deadline is a point in time that can be moved, as net.Pipe's is: wait
// gives a channel that is closed once the time is reached.
type deadline struct {
	mu     *sync.Mutex
	timer  *time.Timer
	cancel chan struct{}
}

func makeDeadline() deadline {
	return deadline{mu: new(sync.Mutex), cancel: make(chan struct{})}
}

// set moves the deadline; the zero time means none.
func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel // the timer ran: wait until it closed the channel
	}
	d.timer = nil
	closed := isClosed(d.cancel)
	if t.IsZero() {
		if closed {
			d.cancel = make(chan struct{})
		}
		return
	}
	if dur := time.Until(t); dur > 0 {
		if closed {
			d.cancel = make(chan struct{})
		}
		cancel := d.cancel
		d.timer = time.AfterFunc(dur, func() { close(cancel) })
		return
	}
	if !closed {
		close(d.cancel)
	}
}

func (d *deadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}
