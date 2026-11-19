package firewall

import (
	"net"

	"yonder/internal/ownroute"
)

// diverted reports whether the system sends traffic for one of this
// computer's own local networks out by another interface. A VPN does that
// when it takes the network's route: Tailscale on Linux, when it accepts
// a subnet route for the network that the computer is in. Devices on the
// network then get no answer from a socket that is not tied to its
// interface (package ownroute).
//
// source gives the address that the system would send from to reach ip.
// For each network of list, it is asked about another address of that
// network; an answer that is no address of the network's interface means
// the traffic goes elsewhere.
func diverted(list []ownroute.Interface, source func(ip net.IP) net.IP) bool {
	for _, ifc := range list {
		for _, n := range ifc.Nets {
			other := another(n)
			if other == nil {
				continue
			}
			from := source(other)
			if from == nil {
				continue
			}
			own := false
			for _, m := range ifc.Nets {
				own = own || m.IP.Equal(from)
			}
			if !own {
				return true
			}
		}
	}
	return false
}

// another gives an address in the private IPv4 network of n that is not
// n's own; nil if n is no such network or has no second address.
func another(n *net.IPNet) net.IP {
	ip := n.IP.To4()
	if ones, bits := n.Mask.Size(); ip == nil || !ip.IsPrivate() || bits != 32 || ones > 30 {
		return nil
	}
	base := ip.Mask(n.Mask)
	other := net.IPv4(base[0], base[1], base[2], base[3]+1).To4()
	if other.Equal(ip) {
		other[3]++
	}
	return other
}

// sourceFor asks the system which address it would send from to reach
// ip: a UDP socket is connected to ip, which sends no packet, and its own
// address is read. nil when the system has no way to ip.
func sourceFor(ip net.IP) net.IP {
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: ip, Port: DiscoveryPort})
	if err != nil {
		return nil
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP
}
