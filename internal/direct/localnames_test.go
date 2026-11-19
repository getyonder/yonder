//go:build !js

package direct

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// mdnsPacket makes an mDNS packet. Each record is "name type value":
// type A with an IPv4 address, AAAA with an IPv6 address, or TXT.
func mdnsPacket(t *testing.T, response bool, records ...string) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: response, Authoritative: response})
	b.EnableCompression()
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		f := strings.Fields(r)
		h := dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(f[0] + "."), Class: dnsmessage.ClassINET, TTL: 120}
		var err error
		switch f[1] {
		case "A":
			err = b.AResource(h, dnsmessage.AResource{A: [4]byte(net.ParseIP(f[2]).To4())})
		case "AAAA":
			err = b.AAAAResource(h, dnsmessage.AAAAResource{AAAA: [16]byte(net.ParseIP(f[2]).To16())})
		case "TXT":
			err = b.TXTResource(h, dnsmessage.TXTResource{TXT: []string{f[2]}})
		default:
			t.Fatalf("record type %q", f[1])
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	packet, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

func TestLocalAnswers(t *testing.T) {
	const a, b = "0a1b2c3d-aaaa.local", "0a1b2c3d-bbbb.local"
	asked := []string{a, b}
	for _, c := range []struct {
		name    string
		packet  []byte
		want    map[string]string
		wantNil bool
	}{
		{name: "an address for a name asked for",
			packet: mdnsPacket(t, true, a+" A 192.168.1.20"),
			want:   map[string]string{a: "192.168.1.20"}},
		{name: "both names in one packet",
			packet: mdnsPacket(t, true, a+" A 192.168.1.20", b+" A 10.0.0.7"),
			want:   map[string]string{a: "192.168.1.20", b: "10.0.0.7"}},
		{name: "the name in capitals",
			packet: mdnsPacket(t, true, strings.ToUpper(a)+" A 192.168.1.20"),
			want:   map[string]string{a: "192.168.1.20"}},
		{name: "a link-local address",
			packet: mdnsPacket(t, true, a+" A 169.254.3.4"),
			want:   map[string]string{a: "169.254.3.4"}},
		{name: "other records before the address",
			packet: mdnsPacket(t, true, "printer.local A 192.168.1.9", a+" AAAA fd00::1", a+" TXT x", a+" A 192.168.1.20"),
			want:   map[string]string{a: "192.168.1.20"}},
		{name: "a name not asked for",
			packet: mdnsPacket(t, true, "printer.local A 192.168.1.9"),
			want:   map[string]string{}},
		{name: "a public address",
			packet: mdnsPacket(t, true, a+" A 203.0.113.7"),
			want:   map[string]string{}},
		{name: "an address of Tailscale",
			packet: mdnsPacket(t, true, a+" A 100.101.102.103"),
			want:   map[string]string{}},
		{name: "the loopback",
			packet: mdnsPacket(t, true, a+" A 127.0.0.1"),
			want:   map[string]string{}},
		{name: "a question, not an answer",
			packet: mdnsPacket(t, false, a+" A 192.168.1.20"), wantNil: true},
		{name: "no packet", packet: nil, wantNil: true},
		{name: "not DNS", packet: []byte("not a DNS packet at all"), wantNil: true},
	} {
		got := localAnswers(c.packet, asked)
		if c.wantNil {
			if got != nil {
				t.Errorf("%s: got %v, want nothing", c.name, got)
			}
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for name, ip := range c.want {
			if got[name] == nil || got[name].String() != ip {
				t.Errorf("%s: %s = %v, want %s", c.name, name, got[name], ip)
			}
		}
	}
}

// A packet is from anyone on the network: one that is cut off at any
// place gives what was whole before the cut, and no panic.
func TestLocalAnswersCutPacket(t *testing.T) {
	const a, b = "0a1b2c3d-aaaa.local", "0a1b2c3d-bbbb.local"
	whole := mdnsPacket(t, true, a+" A 192.168.1.20", b+" A 192.168.1.21")
	for n := 0; n < len(whole); n++ {
		for name, ip := range localAnswers(whole[:n], []string{a, b}) {
			if want := map[string]string{a: "192.168.1.20", b: "192.168.1.21"}[name]; ip.String() != want {
				t.Fatalf("cut at %d: %s = %v, want %s", n, name, ip, want)
			}
		}
	}
}

func FuzzLocalAnswers(f *testing.F) {
	const a = "0a1b2c3d-aaaa.local"
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true})
	b.StartAnswers()
	b.AResource(dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(a + "."), Class: dnsmessage.ClassINET}, dnsmessage.AResource{A: [4]byte{192, 168, 1, 20}})
	seed, _ := b.Finish()
	f.Add(seed)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, packet []byte) {
		for name, ip := range localAnswers(packet, []string{a}) {
			if name != a || ip.To4() == nil || !(ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
				t.Fatalf("got %s = %v", name, ip)
			}
		}
	})
}

func TestWithLocalNames(t *testing.T) {
	const (
		a = "0a1b2c3d-aaaa.local"
		b = "0a1b2c3d-bbbb.local"
	)
	sdp := strings.Join([]string{
		"v=0",
		"a=candidate:1 1 udp 2113937151 " + a + " 49862 typ host generation 0",
		"a=candidate:2 1 UDP 2113937150 " + strings.ToUpper(b) + " 49863 typ host",
		"a=candidate:3 1 udp 2113937151 " + a + " 49864 typ host",
		"a=candidate:4 1 udp 1677729535 203.0.113.7 50000 typ srflx raddr 0.0.0.0 rport 0",
		"a=candidate:5 1 tcp 1518280447 " + a + " 9 typ host tcptype active",
		"a=candidate:6 1 udp 2113937151 192.168.1.30 49865 typ host",
		"",
	}, "\r\n")

	var asked []string
	got := withLocalNames(sdp, func(names []string) map[string]net.IP {
		asked = names
		return map[string]net.IP{a: net.IPv4(192, 168, 1, 20)}
	})
	if len(asked) != 2 || asked[0] != a || asked[1] != b {
		t.Errorf("asked for %v, want each name one time, in small letters", asked)
	}
	want := strings.Join([]string{
		"v=0",
		"a=candidate:1 1 udp 2113937151 192.168.1.20 49862 typ host generation 0",
		// No answer for b: the line stays.
		"a=candidate:2 1 UDP 2113937150 " + strings.ToUpper(b) + " 49863 typ host",
		"a=candidate:3 1 udp 2113937151 192.168.1.20 49864 typ host",
		"a=candidate:4 1 udp 1677729535 203.0.113.7 50000 typ srflx raddr 0.0.0.0 rport 0",
		// Only UDP candidates are changed.
		"a=candidate:5 1 tcp 1518280447 " + a + " 9 typ host tcptype active",
		"a=candidate:6 1 udp 2113937151 192.168.1.30 49865 typ host",
		"",
	}, "\r\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWithLocalNamesNothingToAsk(t *testing.T) {
	sdp := "v=0\r\na=candidate:6 1 udp 2113937151 192.168.1.30 49865 typ host\r\n"
	got := withLocalNames(sdp, func([]string) map[string]net.IP {
		t.Error("asked the network for a description with no .local name")
		return nil
	})
	if got != sdp {
		t.Errorf("the description changed: %q", got)
	}
}

func TestWithLocalNamesAtMost(t *testing.T) {
	var sdp strings.Builder
	for i := 0; i < maxLocalNames+4; i++ {
		fmt.Fprintf(&sdp, "a=candidate:%d 1 udp 2113937151 name-%d.local 49862 typ host\r\n", i, i)
	}
	var asked int
	withLocalNames(sdp.String(), func(names []string) map[string]net.IP {
		asked = len(names)
		return nil
	})
	if asked != maxLocalNames {
		t.Errorf("asked for %d names, want %d", asked, maxLocalNames)
	}
}
