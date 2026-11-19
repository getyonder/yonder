package firewall

import (
	"net"
	"testing"

	"yonder/internal/ownroute"
)

func ifc(t *testing.T, name string, cidrs ...string) ownroute.Interface {
	t.Helper()
	out := ownroute.Interface{Name: name}
	for _, c := range cidrs {
		ip, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		out.Nets = append(out.Nets, &net.IPNet{IP: ip, Mask: n.Mask})
	}
	return out
}

func TestDiverted(t *testing.T) {
	wifi := ifc(t, "wlan0", "192.168.1.5/24", "fd00::5/64")
	cable := ifc(t, "eth0", "10.0.0.1/24", "10.0.0.9/24")
	// from gives a look-up that answers by the address asked about, and
	// notes each question.
	var asked []string
	from := func(answers map[string]string) func(net.IP) net.IP {
		asked = nil
		return func(ip net.IP) net.IP {
			asked = append(asked, ip.String())
			return net.ParseIP(answers[ip.String()])
		}
	}
	for name, c := range map[string]struct {
		list    []ownroute.Interface
		answers map[string]string
		want    bool
	}{
		"no interfaces":                   {nil, nil, false},
		"the network's own address":       {[]ownroute.Interface{wifi}, map[string]string{"192.168.1.1": "192.168.1.5"}, false},
		"a VPN's address":                 {[]ownroute.Interface{wifi}, map[string]string{"192.168.1.1": "100.101.102.103"}, true},
		"the interface's second address":  {[]ownroute.Interface{cable}, map[string]string{"10.0.0.2": "10.0.0.9"}, false},
		"the second network is diverted":  {[]ownroute.Interface{wifi, cable}, map[string]string{"192.168.1.1": "192.168.1.5", "10.0.0.2": "100.101.102.103"}, true},
		"the system has no way":           {[]ownroute.Interface{wifi}, nil, false},
		"another interface's own address": {[]ownroute.Interface{wifi, cable}, map[string]string{"192.168.1.1": "10.0.0.1", "10.0.0.2": "10.0.0.1"}, true},
	} {
		if got := diverted(c.list, from(c.answers)); got != c.want {
			t.Errorf("%s: diverted = %v, want %v (asked about %v)", name, got, c.want, asked)
		}
	}
}

func TestAnother(t *testing.T) {
	for cidr, want := range map[string]string{
		"192.168.1.5/24":  "192.168.1.1",
		"192.168.1.1/24":  "192.168.1.2",
		"10.0.0.1/8":      "10.0.0.2",
		"172.16.5.2/30":   "172.16.5.1",
		"192.168.1.5/32":  "", // no second address
		"192.168.1.4/31":  "",
		"203.0.113.7/24":  "", // not private
		"fd00::5/64":      "", // not IPv4
		"100.64.0.5/10":   "",
		"169.254.10.1/16": "",
	} {
		ip, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		got := another(&net.IPNet{IP: ip, Mask: n.Mask})
		if (got == nil && want != "") || (got != nil && got.String() != want) {
			t.Errorf("another(%s) = %v, want %q", cidr, got, want)
		}
	}
}

// The look-up sends nothing and gives an address of this computer: for
// the loopback, the loopback's.
func TestSourceFor(t *testing.T) {
	if got := sourceFor(net.IPv4(127, 0, 0, 9)); got == nil || !got.IsLoopback() {
		t.Errorf("sourceFor(127.0.0.9) = %v, want a loopback address", got)
	}
}
