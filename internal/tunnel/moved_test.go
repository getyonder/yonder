package tunnel_test

import (
	"slices"
	"testing"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"

	"yonder/internal/tunnel"
	"yonder/internal/tunnel/tunneltest"
)

// When the relay changes, a device's new address is its old one pointing at
// the new relay, so a device that holds the old one can work it out.
func TestMovedAddress(t *testing.T) {
	old := tunnel.Relay
	defer func() { tunnel.Relay = old }()
	tunnel.Relay = "relay.example.com"
	addr := tunneltest.Addr(t)
	if !addr.Valid() {
		t.Fatal("a fresh address is not valid")
	}
	if moved := addr.Elsewhere(); len(moved) != 0 {
		t.Error("an address on the only relay is said to be elsewhere too")
	}

	tunnel.Relay = "elsewhere.example.com:8443"
	moved := addr.Elsewhere()
	if len(moved) != 1 || moved[0] == addr || len(moved[0].Elsewhere()) != 0 {
		t.Fatalf("after the relay changed: moved to %d places, not all on the relay", len(moved))
	}
	// Nothing but the relay differs: pointed back, it is the old address.
	if back, err := moved[0].On("relay.example.com"); err != nil || back != addr {
		t.Errorf("pointed back at the first relay it is a different address (err %v)", err)
	}

	if moved := tunnel.Addr("not an address").Elsewhere(); len(moved) != 0 {
		t.Error("something that is not an address is said to have moved")
	}
	if tunnel.Addr("not an address").Valid() {
		t.Error("something that is not an address is said to be valid")
	}
	tunnel.Relay = ""
	if moved := addr.Elsewhere(); len(moved) != 0 {
		t.Error("with no relay of our own, an address is said to have moved to it")
	}
}

// With several relays, the same device is looked for on each relay in the
// list but the one its address is on. The same relay under two
// spellings counts as two.
func TestElsewhereOnTheList(t *testing.T) {
	t.Cleanup(func() { tunnel.UseRelays(nil) })
	other := tunnel.Relay + ":443"
	tunnel.UseRelays([]string{other, "relay-3.example.com"})
	addr := tunneltest.Addr(t) // on tunnel.Relay
	onOther, err := addr.On(other)
	if err != nil {
		t.Fatal(err)
	}
	gone, err := addr.On("gone.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got := addr.Elsewhere(); len(got) != 2 || got[0] != onOther {
		t.Errorf("from the main relay: %d addresses, first on %s %v", len(got), other, len(got) > 0 && got[0] == onOther)
	}
	if got := gone.Elsewhere(); len(got) != 3 || !slices.Contains(got, addr) || !slices.Contains(got, onOther) {
		t.Errorf("from a relay not in the list: %d addresses", len(got))
	}
}

// An address from a device that is not yet trusted is dialled only on a
// relay in the list. The relay's name is not enough: an address can say at
// which IP address its relay is, and which certificate it shows.
func TestListed(t *testing.T) {
	t.Cleanup(func() { tunnel.UseRelays(nil) })
	tunnel.UseRelays([]string{"relay-2.example.com"})
	addr := tunneltest.Addr(t) // on tunnel.Relay
	second, err := addr.On("relay-2.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !addr.Listed() || !second.Listed() {
		t.Error("an address on a relay in the list is said not to be")
	}
	other, err := addr.On("relay.attacker.example")
	if err != nil {
		t.Fatal(err)
	}
	if other.Listed() {
		t.Error("an address on a relay that is not in the list is said to be on one")
	}
	for what, change := range map[string]func(*tailcfg.DERPNode){
		"at an IP address of its own":   func(n *tailcfg.DERPNode) { n.IPv4 = "203.0.113.7" },
		"at an IPv6 address of its own": func(n *tailcfg.DERPNode) { n.IPv6 = "2001:db8::7" },
		"with a certificate of its own": func(n *tailcfg.DERPNode) { n.CertName = "relay.attacker.example" },
		"with no check of certificates": func(n *tailcfg.DERPNode) { n.InsecureForTests = true },
	} {
		info, err := tailcat.ParseAddr(tailcat.Addr(addr))
		if err != nil {
			t.Fatal(err)
		}
		change(info.Region[0].Nodes[0])
		forged := tunnel.Addr(info.Addr())
		if forged == addr {
			t.Fatalf("the address %s is the plain one: the test changes nothing", what)
		}
		if forged.Listed() {
			t.Errorf("an address that names Yonder's relay %s is said to be on the list", what)
		}
		if addr.MovedTo(forged) {
			t.Errorf("a device is believed to have moved to an address that names Yonder's relay %s", what)
		}
	}
	info, err := tailcat.ParseAddr(tailcat.Addr(addr))
	if err != nil {
		t.Fatal(err)
	}
	info.Region = append(info.Region, &tailcfg.DERPRegion{RegionID: 901, Nodes: []*tailcfg.DERPNode{{HostName: "relay.attacker.example"}}})
	if tunnel.Addr(info.Addr()).Listed() {
		t.Error("an address with a second relay that is not in the list is said to be on the list")
	}
	if tunnel.Addr("not an address").Listed() {
		t.Error("something that is not an address is said to be on the list")
	}
}
