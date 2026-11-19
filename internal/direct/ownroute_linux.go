package direct

import (
	"context"
	"net"

	"github.com/pion/transport/v5"
	"github.com/pion/transport/v5/stdnet"

	"yonder/internal/ownroute"
)

// ownRoute is the network that pion opens its sockets on. Each socket for
// an address of this device is tied to the interface that has the address,
// so what it sends leaves by that interface (package ownroute says why).
type ownRoute struct{ *stdnet.Net }

func newOwnRoute() (transport.Net, error) {
	n, err := stdnet.NewNet()
	if err != nil {
		return nil, err
	}
	return ownRoute{n}, nil
}

func (n ownRoute) ListenUDP(network string, addr *net.UDPAddr) (transport.UDPConn, error) {
	name := ""
	if addr != nil {
		name = ownroute.InterfaceOf(addr.IP)
	}
	if name == "" {
		return n.Net.ListenUDP(network, addr)
	}
	lc := net.ListenConfig{Control: ownroute.Control(name)}
	pc, err := lc.ListenPacket(context.Background(), network, addr.String())
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}
