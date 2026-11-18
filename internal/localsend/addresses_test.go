package localsend

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"

	"yonder/internal/identity"
)

func TestRegistryKeepsAndRanksEveryAddress(t *testing.T) {
	reg := &Registry{Self: "me"}
	info := Info{Alias: "Bo", Fingerprint: "bo"}
	hosts := func() []string {
		p, _ := reg.Find("Bo")
		return p.Hosts()
	}

	reg.Seen(info, "192.168.1.100")
	reg.Seen(info, "192.168.1.10") // as a router that rewrites addresses would show it
	if got := hosts(); len(got) != 2 {
		t.Fatalf("addresses kept: %v, want both", got)
	}
	reg.Reached("bo", "192.168.1.100", true)
	reg.Seen(info, "192.168.1.10")
	if got := hosts(); got[0] != "192.168.1.100" {
		t.Fatalf("after a later call from elsewhere: %v, want the address that was reached first", got)
	}

	reg.Seen(info, "fe80::1%eth0")
	reg.Reached("bo", "fe80::1%eth0", true)
	if got := hosts(); !reflect.DeepEqual(got, []string{"fe80::1%eth0", "192.168.1.100", "192.168.1.10"}) {
		t.Fatalf("ranked: %v, want IPv6 before IPv4 among reached addresses, then the rest", got)
	}
	reg.Reached("bo", "fe80::1%eth0", false)
	if got := hosts(); !reflect.DeepEqual(got, []string{"192.168.1.100", "192.168.1.10", "fe80::1%eth0"}) {
		t.Fatalf("ranked: %v, want the failed address last", got)
	}

	for i := range 2 * maxAddresses {
		reg.Seen(info, fmt.Sprintf("10.0.0.%d", i))
	}
	if got := hosts(); len(got) != maxAddresses || got[0] != "192.168.1.100" {
		t.Fatalf("after many addresses: %v, want %d with the reached one first", got, maxAddresses)
	}
	if len(reg.List()) != 1 {
		t.Fatalf("listed %d times, want once", len(reg.List()))
	}
}

// reaches collects what a Sender reports through Reached.
type reaches struct {
	mu  sync.Mutex
	got map[string]bool
}

func (r *reaches) note(_, host string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.got == nil {
		r.got = map[string]bool{}
	}
	r.got[host] = ok
}

func TestSenderTriesTheNextAddress(t *testing.T) {
	h := newHarness(t)
	s := newSender(t)
	var r reaches
	s.Reached = r.note
	// Nothing listens at 127.0.0.2 on the receiver's port.
	to := h.peer()
	to.Host, to.Others = "127.0.0.2", []string{"127.0.0.1"}

	if _, err := s.Ask(context.Background(), to); err != nil {
		t.Fatalf("ask with a dead first address: %v", err)
	}
	if ok, tried := r.got["127.0.0.2"]; !tried || ok {
		t.Errorf("dead address reported as %v (tried %v), want failed", ok, tried)
	}
	if !r.got["127.0.0.1"] {
		t.Errorf("working address not reported as reached: %v", r.got)
	}

	path := writeFile(t, "note.txt", []byte("over the second address"))
	if sent, err := s.Send(context.Background(), to, []string{path}); err != nil || sent != 1 {
		t.Fatalf("send with a dead first address: %d sent, %v", sent, err)
	}
}

func TestSenderPassesOverAnotherDeviceAtAnAddress(t *testing.T) {
	h := newHarness(t)
	// A different device answers at 127.0.0.2 on the same port, as the
	// router would at an address the receiver was wrongly seen at.
	other, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", net.JoinHostPort("127.0.0.2", fmt.Sprint(h.port)), other.TLSConfig())
	if err != nil {
		t.Skipf("this system has no second loopback address to listen on: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				c.(*tls.Conn).Handshake()
				c.Close()
			}()
		}
	}()

	s := newSender(t)
	var r reaches
	s.Reached = r.note
	to := h.peer()
	to.Host, to.Others = "127.0.0.2", []string{"127.0.0.1"}
	info, err := s.Ask(context.Background(), to)
	if err != nil || info.Fingerprint != h.rc.Info.Fingerprint {
		t.Fatalf("ask: %+v, %v; want the receiver itself", info, err)
	}
	if r.got["127.0.0.2"] {
		t.Error("another device's address was reported as reached")
	}
}

func TestSenderReportsTheBestAddressWhenNoneWorks(t *testing.T) {
	h := newHarness(t)
	s := newSender(t)
	to := h.peer()
	to.Info.Port = 1 // nothing listens there on either address
	to.Others = []string{"127.0.0.2"}
	_, err := s.Ask(context.Background(), to)
	var op *net.OpError
	if !errors.As(err, &op) || op.Op != "dial" || !errors.Is(err, ErrNotReached) {
		t.Fatalf("error: %v, want the failed connection itself, as ErrNotReached", err)
	}
}

func TestAnotherDeviceAtTheOnlyAddressIsNotReached(t *testing.T) {
	h := newHarness(t)
	to := h.peer()
	to.Info.Fingerprint = "someone else" // the receiver's certificate will not match
	if _, err := newSender(t).Send(context.Background(), to, []string{writeFile(t, "note.txt", []byte("x"))}); !errors.Is(err, ErrNotReached) {
		t.Fatalf("error: %v, want ErrNotReached", err)
	}
}

// A device that says it takes files without TLS (LocalSend with its
// encryption switched off) is listed as such. One seen with TLS is never
// turned into one without, whoever says so; the other way round it is.
func TestRegistryKeepsTLSOnceSeen(t *testing.T) {
	reg := &Registry{Self: "me"}
	find := func(fingerprint string) (Peer, bool) {
		for _, p := range reg.List() {
			if p.Info.Fingerprint == fingerprint {
				return p, true
			}
		}
		return Peer{}, false
	}
	reg.Seen(Info{Alias: "Plain", Fingerprint: "plain", Port: 53317, Protocol: "http"}, "192.168.1.30")
	reg.Seen(Info{Alias: "Bo", Fingerprint: "bo", Port: 53317, Protocol: "https"}, "192.168.1.100")
	reg.Seen(Info{Alias: "Cy", Fingerprint: "cy"}, "192.168.1.101") // an answer to a registration names no protocol
	if p, ok := find("plain"); !ok || p.Encrypted() || p.baseURL() != "http://192.168.1.30:53317" {
		t.Fatalf("Plain: listed %v, %+v", ok, p)
	}
	if p, _ := find("cy"); !p.Encrypted() {
		t.Fatal("Cy, which named no protocol, is taken as without TLS")
	}

	reg.Seen(Info{Alias: "Bo", Fingerprint: "bo", Port: 9, Protocol: "http"}, "192.168.1.66")
	if p, _ := find("bo"); !p.Encrypted() || p.baseURL() != "https://192.168.1.100:53317" || len(p.Others) != 0 {
		t.Fatalf("an announce without TLS changed Bo to %s, also at %v", p.baseURL(), p.Others)
	}

	reg.Seen(Info{Alias: "Plain", Fingerprint: "plain", Port: 53317, Protocol: "https"}, "192.168.1.31")
	if p, _ := find("plain"); !p.Encrypted() || p.baseURL() != "https://192.168.1.31:53317" || len(p.Others) != 0 {
		t.Fatalf("Plain with TLS is at %s, also at %v", p.baseURL(), p.Others)
	}
}

// A paired device is only ever listed with TLS.
func TestRegistryListsTLSOnlyDevicesOnlyWithTLS(t *testing.T) {
	reg := &Registry{Self: "me", TLSOnly: func(fingerprint string) bool { return fingerprint == "paired" }}
	reg.Seen(Info{Alias: "Paired", Fingerprint: "paired", Port: 9, Protocol: "http"}, "192.168.1.66")
	reg.Seen(Info{Alias: "Plain", Fingerprint: "plain", Port: 53317, Protocol: "http"}, "192.168.1.30")
	if peers := reg.List(); len(peers) != 1 || peers[0].Info.Alias != "Plain" {
		t.Fatalf("listed %+v, want only Plain", peers)
	}
	reg.Seen(Info{Alias: "Paired", Fingerprint: "paired", Port: 53317, Protocol: "https"}, "192.168.1.100")
	if _, ok := reg.Find("Paired"); !ok {
		t.Fatal("the paired device is not listed with TLS")
	}
}

// Once a device has been reached, what is said of it from another address
// does not change its name or port: anyone can announce its fingerprint.
func TestRegistryBelievesAReachedDeviceOnlyFromWhereItWasReached(t *testing.T) {
	reg := &Registry{Self: "me"}
	bo := func() Peer {
		for _, p := range reg.List() {
			if p.Info.Fingerprint == "bo" {
				return p
			}
		}
		t.Fatal("Bo is not listed")
		return Peer{}
	}
	reg.Seen(Info{Alias: "Bo", Fingerprint: "bo", Port: 53317}, "192.168.1.100")
	// Not reached yet: the last word counts, as it must for a device that
	// starts again on another port before anyone has sent to it.
	reg.Seen(Info{Alias: "Bo", Fingerprint: "bo", Port: 53318}, "192.168.1.100")
	if got := bo().Info.Port; got != 53318 {
		t.Fatalf("port %d before the device was reached, want 53318", got)
	}

	reg.Reached("bo", "192.168.1.100", true)
	reg.Seen(Info{Alias: "Not Bo", Fingerprint: "bo", Port: 9}, "192.168.1.66")
	if p := bo(); p.Info.Port != 53318 || p.Info.Alias != "Bo" || p.Host != "192.168.1.100" {
		t.Fatalf("another address changed a reached device to %q, port %d, at %s", p.Info.Alias, p.Info.Port, p.Host)
	}
	reg.Seen(Info{Alias: "Bo again", Fingerprint: "bo", Port: 53319}, "192.168.1.100")
	if p := bo(); p.Info.Port != 53319 || p.Info.Alias != "Bo again" {
		t.Fatalf("the device's own news was not taken: %q, port %d", p.Info.Alias, p.Info.Port)
	}
}
