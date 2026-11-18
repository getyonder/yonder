package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"

	"yonder/internal/nettest"
)

func TestConnectRejectsAnythingButAnAddress(t *testing.T) {
	for _, bad := range []Addr{"", "192.168.1.5", "tcnot-really-an-address"} {
		if _, err := Connect(bad, nil); err == nil {
			t.Errorf("Connect(%q) succeeded", bad)
		}
	}
}

// An ordinary HTTP server and client work through the tunnel unchanged,
// which is all the rest of Yonder asks of this package.
func TestHTTPThroughTheTunnel(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srv, err := Listen(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go http.Serve(srv.Listener(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s", r.URL.Path)
	}))

	cl, err := Connect(srv.Addr(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	client := &http.Client{Transport: &http.Transport{DialContext: cl.DialContext}}
	defer client.CloseIdleConnections()

	req, _ := http.NewRequestWithContext(ctx, "GET", "http://device.yonder/there", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello /there" {
		t.Fatalf("got %q", body)
	}
}

func TestBadClientIDIsRefused(t *testing.T) {
	if _, err := Listen(context.Background(), NewKeys(), []ClientID{"not a key"}); err == nil {
		t.Fatal("a malformed client id was accepted")
	}
}

func TestKeysSurviveJSON(t *testing.T) {
	keys := NewKeys()
	want, err := keys.ClientID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(keys)
	var back Keys
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if got, err := back.ClientID(); err != nil || got != want {
		t.Fatalf("client id after a round trip: %q, %v; want %q", got, err, want)
	}
	if other, _ := NewKeys().ClientID(); other == want {
		t.Fatal("two fresh identities share a client id")
	}
}

// The behaviour pairing rests on, through this package's own API: a saved
// address survives a restart, only allowed clients get in, one can be added
// while running, and the server knows who each connection is from.
func TestSavedKeysAndAllowList(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	host, friend, stranger := NewKeys(), NewKeys(), NewKeys()
	friendID, _ := friend.ClientID()
	strangerID, _ := stranger.ClientID()

	first, err := Listen(ctx, host, []ClientID{friendID})
	if err != nil {
		t.Fatal(err)
	}
	addr := first.Addr()
	if len(host.Region) == 0 {
		t.Fatal("Listen did not record the relay region in the keys")
	}
	first.Close()

	srv, err := Listen(ctx, host, []ClientID{friendID})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.Addr() != addr {
		t.Fatal("the address changed across a restart")
	}
	who := make(chan ClientID, 4)
	go http.Serve(srv.Listener(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
		remote, _ := net.ResolveTCPAddr("tcp", r.RemoteAddr)
		who <- srv.PeerID(local, remote)
	}))

	get := func(keys *Keys) error {
		cl, err := Connect(addr, keys)
		if err != nil {
			return err
		}
		defer cl.Close()
		if err := cl.Ping(ctx); err != nil {
			return err
		}
		client := &http.Client{Transport: &http.Transport{DialContext: cl.DialContext}}
		defer client.CloseIdleConnections()
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://device.yonder/", nil)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}

	if err := get(friend); err != nil {
		t.Fatalf("allowed client: %v", err)
	}
	if got := <-who; got != friendID {
		t.Fatalf("server saw %q, want the friend's id", got)
	}
	if err := get(stranger); err == nil {
		t.Fatal("a client that is not allowed got in")
	}
	if err := srv.Allow(strangerID); err != nil {
		t.Fatal(err)
	}
	if err := get(stranger); err != nil {
		t.Fatalf("client allowed while running: %v", err)
	}
}

// Relay is "host" or "host:port"; the port must reach tailcat, or it would
// try 443.
func TestRelayRegion(t *testing.T) {
	old := Relay
	defer func() { Relay = old }()
	for _, tc := range []struct {
		relay, host string
		port        int
		bad         bool
	}{
		{relay: "relay.example.com:8443", host: "relay.example.com", port: 8443},
		{relay: "relay.example.com", host: "relay.example.com"},
		{relay: "relay.example.com:0", bad: true},
		{relay: "relay.example.com:https", bad: true},
	} {
		Relay = tc.relay
		r, err := relayRegion()
		if tc.bad {
			if err == nil {
				t.Errorf("%q: no error", tc.relay)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.relay, err)
			continue
		}
		if n := r.Nodes[0]; n.HostName != tc.host || n.DERPPort != tc.port || r.RegionID == 0 {
			t.Errorf("%q: got host %q port %d region %d", tc.relay, n.HostName, n.DERPPort, r.RegionID)
		}
	}
	Relay = ""
	if r, err := relayRegion(); r != nil || err != nil {
		t.Errorf("no relay: got %v, %v", r, err)
	}
}

// A new address names the relay, so the other device needs no setting, and
// an address made on another relay is told apart.
func TestAddressNamesTheRelay(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	srv, err := Listen(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if got := srv.Addr().relays(); !slices.Equal(got, []string{Relay}) {
		t.Errorf("a fresh address is on %v, not %s", got, Relay)
	}
	// The address in a pairing link is such a one: it must pass the check
	// a device makes before it dials it.
	if !srv.Addr().Listed() {
		t.Error("a fresh address is not on the list of relays")
	}
	old := Relay
	defer func() { Relay = old }()
	Relay = "elsewhere.example.com:8443"
	if len(srv.Addr().Elsewhere()) != 1 {
		t.Error("the address claims to be on a relay it was not made with")
	}
}
