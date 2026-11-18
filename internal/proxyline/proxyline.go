// Package proxyline reads the one line a proxy puts in front of a
// connection to say who the visitor really is (the PROXY protocol, version
// 1, as nginx's "proxy_protocol on" sends it). Yonder's server needs it
// because nginx passes connections on unopened, so without the line every
// visitor looks like 127.0.0.1.
package proxyline

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

// headerWait is how long a connection may take to send its line. nginx
// sends it the moment it connects.
const headerWait = 5 * time.Second

// maxLine is the longest line the protocol allows, with its line ending.
const maxLine = 107

// Listen wraps a listener whose every connection must begin with a PROXY
// line. The connections it hands out report the visitor's address as their
// RemoteAddr. A connection without a valid line is closed and never handed
// out, so the wrapped listener must only be reachable by the proxy.
func Listen(inner net.Listener) net.Listener {
	l := &listener{Listener: inner, conns: make(chan net.Conn), done: make(chan struct{})}
	go l.accept()
	return l
}

type listener struct {
	net.Listener
	conns chan net.Conn
	done  chan struct{}
	err   error // why accept stopped; set before done is closed
}

// accept takes connections and reads each one's line on its own goroutine,
// so a slow one holds up nobody else.
func (l *listener) accept() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			l.err = err
			close(l.done)
			return
		}
		go func() {
			addr, err := readLine(c)
			if err != nil {
				c.Close()
				return
			}
			select {
			case l.conns <- &conn{Conn: c, remote: addr}:
			case <-l.done:
				c.Close()
			}
		}()
	}
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, l.err
	}
}

type conn struct {
	net.Conn
	remote net.Addr
}

func (c *conn) RemoteAddr() net.Addr { return c.remote }

// readLine reads the line one byte at a time, so nothing after it (the
// start of the TLS handshake) is taken from the connection.
func readLine(c net.Conn) (net.Addr, error) {
	c.SetReadDeadline(time.Now().Add(headerWait))
	defer c.SetReadDeadline(time.Time{})
	var line []byte
	b := make([]byte, 1)
	for len(line) < maxLine {
		if _, err := c.Read(b); err != nil {
			return nil, err
		}
		line = append(line, b[0])
		if b[0] == '\n' {
			return Parse(string(line))
		}
	}
	return nil, errors.New("proxy line too long")
}

// Parse reads one PROXY line, "PROXY TCP4 <from> <to> <port> <port>\r\n",
// and returns the visitor's address.
func Parse(line string) (net.Addr, error) {
	rest, ok := strings.CutSuffix(line, "\r\n")
	if !ok {
		return nil, errors.New("proxy line has no line ending")
	}
	f := strings.Split(rest, " ")
	if len(f) != 6 || f[0] != "PROXY" || (f[1] != "TCP4" && f[1] != "TCP6") {
		return nil, fmt.Errorf("not a proxy line: %q", rest)
	}
	ap, err := netip.ParseAddrPort(net.JoinHostPort(f[2], f[4]))
	if err != nil {
		return nil, fmt.Errorf("proxy line: %w", err)
	}
	return net.TCPAddrFromAddrPort(ap), nil
}
