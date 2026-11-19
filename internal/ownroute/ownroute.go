// Package ownroute keeps a socket on its own network interface, so that
// what it sends leaves by that interface and what arrives there gets its
// answers back the same way.
//
// Without this, a routing rule of the system can take traffic for the
// local network away: Tailscale does so on Linux when a device of the
// tailnet advertises the subnet that this device is itself in. Packets
// for a device on the same network then go into the tunnel and get no
// answer. A socket that is tied to its interface is not affected: the
// system passes over a route that names another interface.
//
// Only Linux has the tie (ownroute_linux.go). On other systems every
// function here changes nothing.
package ownroute

import (
	"net"
	"net/netip"
	"strings"
)

// Interface is one network interface of this computer.
type Interface struct {
	Name  string
	Index int
	// Nets are its addresses, each with its network.
	Nets []*net.IPNet
}

// skipInterfaces is the one list of network interfaces that lead to no
// device on a local network: those of Docker, bridges, virtual machines,
// VPNs and Tailscale, by the start of their names in lower case.
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

// UsableInterface says whether an interface can lead to a device on a
// local network.
func UsableInterface(name string) bool {
	name = strings.ToLower(name)
	for _, p := range skipInterfaces {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// UsableIP says whether an address can be that of a device on a local
// network or on the internet: not one of Tailscale's.
func UsableIP(ip net.IP) bool {
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

// Dialer gives the dialer for a connection to host, an IP address. It is
// tied to the interface that InterfaceFor gives, if there is one.
func Dialer(host string) *net.Dialer {
	return &net.Dialer{Control: Control(InterfaceFor(host))}
}

// interfaceOf gives the name of the interface in list that has the
// address, or "" for no address, the loopback, or an address of no
// interface.
func interfaceOf(list []Interface, ip net.IP) string {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() {
		return ""
	}
	for _, ifc := range list {
		for _, n := range ifc.Nets {
			if n.IP.Equal(ip) {
				return ifc.Name
			}
		}
	}
	return ""
}

// interfaceFor gives the name of the interface in list whose network
// holds host, the address of another device. It gives "" when no
// interface or more than one fits, for the loopback, for an address of
// this computer, for an address that is not private or link-local, and
// for one that names its interface itself ("fe80::1%eth0").
func interfaceFor(list []Interface, host string) string {
	a, err := netip.ParseAddr(host)
	if err != nil || a.Zone() != "" {
		return ""
	}
	a = a.Unmap()
	if a.IsLoopback() || !(a.IsPrivate() || a.IsLinkLocalUnicast()) {
		return ""
	}
	ip := net.IP(a.AsSlice())
	found := ""
	for _, ifc := range list {
		for _, n := range ifc.Nets {
			if n.IP.Equal(ip) {
				return ""
			}
			if n.Contains(ip) && found != ifc.Name {
				if found != "" {
					return ""
				}
				found = ifc.Name
			}
		}
	}
	return found
}

// local gives the interfaces of list that can lead to a device on a local
// network: those with a usable name and a private IPv4 address.
func local(list []Interface) []Interface {
	var out []Interface
	for _, ifc := range list {
		if !UsableInterface(ifc.Name) {
			continue
		}
		for _, n := range ifc.Nets {
			if ip := n.IP.To4(); ip != nil && ip.IsPrivate() {
				out = append(out, ifc)
				break
			}
		}
	}
	return out
}
