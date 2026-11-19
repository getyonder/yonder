package ownroute

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// patience is the most that a step of these tests waits for another
// process or goroutine. All of them are on this computer.
const patience = 10 * time.Second

// ownNetwork gives an interface of this computer that gets a tied
// listener, with its private IPv4 address and the network of that.
func ownNetwork(t *testing.T) (name string, ipn *net.IPNet) {
	t.Helper()
	for _, ifc := range Local() {
		for _, n := range ifc.Nets {
			if ip := n.IP.To4(); ip != nil && ip.IsPrivate() {
				return ifc.Name, n
			}
		}
	}
	t.Skip("this computer has no interface of a local network")
	return "", nil
}

// another gives an address in the network of ipn that is not ipn's own.
func another(ipn *net.IPNet) net.IP {
	ip := ipn.IP.Mask(ipn.Mask).To4()
	other := net.IPv4(ip[0], ip[1], ip[2], ip[3]+1).To4()
	if other.Equal(ipn.IP.To4()) {
		other[3]++
	}
	return other
}

// boundTo gives the interface that a socket is tied to, "" for none.
func boundTo(t *testing.T, conn any) string {
	t.Helper()
	sc, ok := conn.(syscall.Conn)
	if !ok {
		t.Fatalf("the socket is a %T", conn)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if cerr := raw.Control(func(fd uintptr) {
		name, err = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
	}); cerr != nil || err != nil {
		t.Fatal(cerr, err)
	}
	return name
}

func TestSystemInterfaces(t *testing.T) {
	name, ipn := ownNetwork(t)
	if got := InterfaceOf(ipn.IP); got != name {
		t.Errorf("InterfaceOf(%s) = %q, want %q", ipn.IP, got, name)
	}
	if got := InterfaceFor(another(ipn).String()); got != name {
		t.Errorf("InterfaceFor(%s) = %q, want %q", another(ipn), got, name)
	}
	if got := InterfaceFor(ipn.IP.String()); got != "" {
		t.Errorf("InterfaceFor(this computer's %s) = %q, want none", ipn.IP, got)
	}
}

func TestControlTiesSocket(t *testing.T) {
	if Control("") != nil {
		t.Error("Control gives a function for no interface")
	}
	name, ipn := ownNetwork(t)
	lc := net.ListenConfig{Control: Control(name)}
	pc, err := lc.ListenPacket(context.Background(), "udp4", net.JoinHostPort(ipn.IP.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if got := boundTo(t, pc); got != name {
		t.Errorf("the socket at %s is tied to %q, want %q", ipn.IP, got, name)
	}
}

// A dialer for a device on a network of this computer is tied to that
// network's interface; one for the loopback is not. UDP, so that no
// packet is sent and nothing has to answer.
func TestDialerTiesSocket(t *testing.T) {
	name, ipn := ownNetwork(t)
	for host, want := range map[string]string{another(ipn).String(): name, "127.0.0.1": ""} {
		c, err := Dialer(host).DialContext(context.Background(), "udp4", net.JoinHostPort(host, "9"))
		if err != nil {
			t.Fatal(err)
		}
		if got := boundTo(t, c); got != want {
			t.Errorf("the socket to %s is tied to %q, want %q", host, got, want)
		}
		c.Close()
	}
}

// answer writes name to each connection that ln gets.
func answer(ln net.Listener, name string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		fmt.Fprintln(c, name)
		c.Close()
	}
}

// ask connects with dialer and gives the line that the listener wrote.
func ask(dialer *net.Dialer, addr string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	c, err := dialer.DialContext(ctx, "tcp4", addr)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(patience))
	b := make([]byte, 16)
	n, err := c.Read(b)
	return strings.TrimSpace(string(b[:n])), err
}

// pair opens a listener for every interface and a tied one beside it.
func pair(t *testing.T, name string) (port int) {
	t.Helper()
	every, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { every.Close() })
	port = every.Addr().(*net.TCPAddr).Port
	if _, err := Listen(name, port); err == nil {
		t.Fatal("a tied listener got the port before Share")
	}
	if err := Share(every); err != nil {
		t.Fatal(err)
	}
	tied, err := Listen(name, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tied.Close() })
	if got := boundTo(t, tied); got != name {
		t.Fatalf("the tied listener is tied to %q, want %q", got, name)
	}
	go answer(every, "every")
	go answer(tied, "tied")
	return port
}

// Beside a listener for every interface, a tied listener takes the same
// port and gets what comes for its interface's address; the loopback
// stays with the first. A plain listener is still refused the port.
func TestTiedListenerBesideTheFirst(t *testing.T) {
	name, ipn := ownNetwork(t)
	port := pair(t, name)
	if ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port)); err == nil {
		ln.Close()
		t.Error("a second plain listener got the port")
	}
	for host, want := range map[string]string{ipn.IP.String(): "tied", "127.0.0.1": "every"} {
		got, err := ask(&net.Dialer{}, fmt.Sprintf("%s:%d", host, port))
		if err != nil || got != want {
			t.Errorf("a connection to %s got %q, %v; want the listener %q", host, got, err, want)
		}
	}
}

// With a route that takes the local network's traffic away, as Tailscale
// makes it, a tied listener still gets a connection from the network and
// a tied dialer still reaches a device there. Making such a route needs a
// network of this test's own: testdata/route.sh makes one and runs this
// test in it. Without it the test is passed over.
func TestUnderRouteThatDiverts(t *testing.T) {
	peer, pid := os.Getenv("YONDER_ROUTE_PEER"), os.Getenv("YONDER_ROUTE_PEER_PID")
	if peer == "" || pid == "" {
		t.Skip("run by internal/ownroute/testdata/route.sh, which makes the network and the route")
	}
	name, ipn := ownNetwork(t)
	inPeer := func(arg ...string) *exec.Cmd {
		return exec.Command("nsenter", append([]string{"-t", pid, "-n"}, arg...)...)
	}

	// Coming in: the other device connects to this one.
	port := pair(t, name)
	out, err := inPeer("timeout", "5", "bash", "-c", fmt.Sprintf("cat < /dev/tcp/%s/%d", ipn.IP, port)).CombinedOutput()
	if got := strings.TrimSpace(string(out)); err != nil || got != "tied" {
		t.Errorf("a connection from the network got %q, %v; want the listener \"tied\"", got, err)
	}

	// Going out: this device connects to the other.
	serve := inPeer("python3", "-c", `
import socket
s = socket.create_server(("0.0.0.0", 5001))
while True:
    c, _ = s.accept()
    c.sendall(b"peer\n")
    c.close()
`)
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serve.Process.Kill(); serve.Wait() })
	addr := net.JoinHostPort(peer, "5001")
	var got string
	for deadline := time.Now().Add(patience); ; time.Sleep(20 * time.Millisecond) {
		// Refused until the other device listens.
		if got, err = ask(Dialer(peer), addr); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil || got != "peer" {
		t.Errorf("the tied dialer got %q, %v; want \"peer\"", got, err)
	}
	// For the record, not a condition: what a plain socket gets.
	plain := &net.Dialer{Timeout: time.Second}
	got, err = ask(plain, addr)
	t.Logf("a plain dialer under the route: %q, %v", got, err)
}
