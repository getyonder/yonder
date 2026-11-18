//go:build !js

package direct

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
		if usableInterface(name) != want {
			t.Errorf("usableInterface(%q) = %v", name, !want)
		}
	}
	for ip, want := range map[string]bool{
		"192.168.1.5": true, "203.0.113.7": true, "2001:db8::1": true,
		"100.64.0.1": false, "100.101.102.103": false, "fd7a:115c:a1e0::1": false,
	} {
		if usableIP(net.ParseIP(ip)) != want {
			t.Errorf("usableIP(%s) = %v", ip, !want)
		}
	}
}
