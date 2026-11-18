package proxyline

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	good := map[string]string{
		"PROXY TCP4 203.0.113.7 127.0.0.1 51234 443\r\n":   "203.0.113.7:51234",
		"PROXY TCP6 2001:db8::1 2001:db8::2 51234 443\r\n": "[2001:db8::1]:51234",
	}
	for line, want := range good {
		addr, err := Parse(line)
		if err != nil {
			t.Errorf("%q: %v", line, err)
		} else if addr.String() != want {
			t.Errorf("%q: got %s, want %s", line, addr, want)
		}
	}
	bad := []string{
		"",
		"GET / HTTP/1.1\r\n",
		"PROXY UNKNOWN\r\n",
		"PROXY TCP4 203.0.113.7 127.0.0.1 51234 443\n",
		"PROXY TCP4 not-an-address 127.0.0.1 51234 443\r\n",
		"PROXY TCP4 203.0.113.7 127.0.0.1 99999 443\r\n",
		"PROXY TCP4 203.0.113.7 127.0.0.1 51234\r\n",
	}
	for _, line := range bad {
		if addr, err := Parse(line); err == nil {
			t.Errorf("%q: accepted as %s", line, addr)
		}
	}
}

// A connection with a line is handed out under the visitor's address with
// the bytes after the line untouched; one without a line is never handed
// out and does not hold up the next.
func TestListen(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := Listen(inner)
	defer l.Close()

	bad, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	bad.Write([]byte("GET / HTTP/1.1\r\n"))

	good, err := net.Dial("tcp", inner.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer good.Close()
	good.Write([]byte("PROXY TCP4 203.0.113.7 127.0.0.1 51234 443\r\nhello"))

	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.RemoteAddr().String(); got != "203.0.113.7:51234" {
		t.Errorf("remote address %s, want 203.0.113.7:51234", got)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Errorf("after the line: %q, %v", buf, err)
	}

	// The connection without a line was closed by the listener.
	bad.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := bad.Read(buf); err != io.EOF {
		t.Errorf("connection without a line: %v, want it closed", err)
	}
}
