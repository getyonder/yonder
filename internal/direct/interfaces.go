//go:build !js

package direct

import (
	"net"
	"net/netip"
	"strings"
)

// skipInterfaces is the one list of network interfaces that give no
// addresses: those of Docker, bridges, virtual machines, VPNs and
// Tailscale, by the start of their names in lower case. None of them
// leads to the other device by a direct path, and asking a STUN server
// from each made finding the addresses take seconds.
var skipInterfaces = []string{
	"docker", "br-", "veth", "cni", "flannel", "podman", // containers
	"virbr", "bridge", "vmnet", "vmware", "vboxnet", "virtualbox", "vethernet", // virtual machines
	"tailscale", "utun", "tun", "tap", "wg", "zt", // Tailscale and other VPNs
}

// skipNets are Tailscale's addresses, whatever the interface is called.
var skipNets = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// usableInterface says whether an interface's addresses are offered.
func usableInterface(name string) bool {
	name = strings.ToLower(name)
	for _, p := range skipInterfaces {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// usableIP says whether an address is offered, or tried when the other
// end offers it.
func usableIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	for _, n := range skipNets {
		if n.Contains(a) {
			return false
		}
	}
	return true
}
