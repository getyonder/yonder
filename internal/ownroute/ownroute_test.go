package ownroute

import (
	"net"
	"testing"
)

func TestInterfacesLeftOut(t *testing.T) {
	for name, want := range map[string]bool{
		"eth0": true, "enp5s0": true, "wlan0": true, "en0": true, "Wi-Fi": true, "Ethernet": true,
		"docker0": false, "br-3f2a": false, "veth12ab": false, "virbr0": false,
		"vmnet8": false, "VMware Network Adapter VMnet1": false, "vEthernet (WSL)": false,
		"vboxnet0": false, "bridge100": false, "tailscale0": false, "Tailscale": false, "utun3": false,
	} {
		if UsableInterface(name) != want {
			t.Errorf("UsableInterface(%q) = %v", name, !want)
		}
	}
	for ip, want := range map[string]bool{
		"192.168.1.5": true, "203.0.113.7": true, "2001:db8::1": true,
		"100.64.0.1": false, "100.101.102.103": false, "fd7a:115c:a1e0::1": false,
	} {
		if UsableIP(net.ParseIP(ip)) != want {
			t.Errorf("UsableIP(%s) = %v", ip, !want)
		}
	}
}

// ifc makes an interface from its name and its addresses with their
// networks ("192.168.1.5/24").
func ifc(t *testing.T, name string, index int, cidrs ...string) Interface {
	t.Helper()
	out := Interface{Name: name, Index: index}
	for _, c := range cidrs {
		ip, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		out.Nets = append(out.Nets, &net.IPNet{IP: ip, Mask: n.Mask})
	}
	return out
}

// made is a computer on Wi-Fi with Tailscale, a virtual machine's network
// and a second network that lies over the first.
func made(t *testing.T) []Interface {
	return []Interface{
		ifc(t, "wlan0", 2, "192.168.1.5/24", "fd00::5/64", "fe80::5/64"),
		ifc(t, "tailscale0", 3, "100.101.102.103/32", "fe80::7/64"),
		ifc(t, "vmnet8", 4, "172.16.9.1/24"),
		ifc(t, "eth0", 5, "10.0.0.5/8"),
		ifc(t, "eth1", 6, "10.1.0.5/16", "10.1.0.6/16"),
	}
}

func TestInterfaceOf(t *testing.T) {
	list := made(t)
	for ip, want := range map[string]string{
		"192.168.1.5":     "wlan0",
		"fd00::5":         "wlan0",
		"100.101.102.103": "tailscale0",
		"10.1.0.6":        "eth1",
		"192.168.1.6":     "", // another device's
		"192.0.2.1":       "", // of no interface
		"127.0.0.1":       "",
		"::1":             "",
		"0.0.0.0":         "",
		"::":              "",
	} {
		if got := interfaceOf(list, net.ParseIP(ip)); got != want {
			t.Errorf("interfaceOf(%s) = %q, want %q", ip, got, want)
		}
	}
	if got := interfaceOf(list, nil); got != "" {
		t.Errorf("interfaceOf(no address) = %q, want none", got)
	}
}

func TestInterfaceFor(t *testing.T) {
	list := made(t)
	for host, want := range map[string]string{
		"192.168.1.20":        "wlan0",
		"::ffff:192.168.1.20": "wlan0",
		"fd00::20":            "wlan0",
		"172.16.9.20":         "vmnet8",
		"10.200.0.1":          "eth0",
		"10.1.0.20":           "", // in the networks of eth0 and of eth1
		"192.168.1.5":         "", // this computer's own
		"192.168.2.20":        "", // a private network this computer is not in
		"fe80::20":            "", // every interface has this network
		"fe80::20%wlan0":      "", // names its interface itself
		"100.101.102.103":     "", // not private
		"100.70.1.1":          "",
		"203.0.113.7":         "", // on the internet
		"2001:db8::1":         "",
		"127.0.0.1":           "",
		"::1":                 "",
		"mac.local":           "", // not an address
		"":                    "",
	} {
		if got := interfaceFor(list, host); got != want {
			t.Errorf("interfaceFor(%q) = %q, want %q", host, got, want)
		}
	}
	if got := interfaceFor(nil, "192.168.1.20"); got != "" {
		t.Errorf("interfaceFor with no interfaces = %q, want none", got)
	}
}

func TestLocal(t *testing.T) {
	var got []string
	for _, i := range local(append(made(t), ifc(t, "eth2", 7, "203.0.113.7/24"), ifc(t, "eth3", 8, "fd00:1::5/64"), ifc(t, "eth4", 9))) {
		got = append(got, i.Name)
	}
	// Not Tailscale's and not the virtual machine's, by their names; not
	// eth2, eth3 and eth4, which have no private IPv4 address.
	if want := []string{"wlan0", "eth0", "eth1"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("local = %v, want %v", got, want)
	}
}
