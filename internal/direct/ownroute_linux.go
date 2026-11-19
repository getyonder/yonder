package direct

import (
	"context"
	"net"
	"syscall"

	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"
	"golang.org/x/sys/unix"
)

// ownRoute is the network that pion opens its sockets on. Each socket for
// an address of this device is tied to the interface that has the address,
// so what it sends leaves by that interface.
//
// Without this, a routing rule of the system can take a packet for the
// local network away: Tailscale does so on Linux when a device of the
// tailnet advertises the subnet that this device is itself in. The checks
// to a device on the same network then go into the tunnel and get no
// answer, and no path forms on that network.
type ownRoute struct{ *stdnet.Net }

func newOwnRoute() (transport.Net, error) {
	n, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	return ownRoute{n}, nil
}

func (n ownRoute) ListenUDP(network string, addr *net.UDPAddr) (transport.UDPConn, error) {
	name := interfaceOf(addr)
	if name == "" {
		return n.Net.ListenUDP(network, addr)
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		// A failure leaves the socket as the system makes it.
		c.Control(func(fd uintptr) { unix.BindToDevice(int(fd), name) })
		return nil
	}}
	pc, err := lc.ListenPacket(context.Background(), network, addr.String())
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}

// interfaceOf gives the name of the interface that has the address, or ""
// for no address, the loopback, or an address of no interface.
func interfaceOf(addr *net.UDPAddr) string {
	if addr == nil || addr.IP == nil || addr.IP.IsUnspecified() || addr.IP.IsLoopback() {
		return ""
	}
	ifaces, _ := net.Interfaces()
	for _, ifi := range ifaces {
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(addr.IP) {
				return ifi.Name
			}
		}
	}
	return ""
}
