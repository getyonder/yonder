package tunnel

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"tailscale.com/tailcfg"

	"yonder/internal/nettest"
)

// keysOn is fresh keys whose saved home is relay.
func keysOn(t *testing.T, relay string) *Keys {
	t.Helper()
	k := NewKeys()
	if relay != "" {
		r, err := regionOf(relay)
		if err != nil {
			t.Fatal(err)
		}
		k.Region, _ = json.Marshal(r)
	}
	return k
}

// A device keeps its home while it is in the list; a device with no home,
// or one whose home has left the list, takes the nearest, which with one
// relay is that one, unmeasured.
func TestHome(t *testing.T) {
	t.Cleanup(func() { UseRelays(nil) })
	ctx := context.Background()
	UseRelays([]string{"relay-2.example.com:8443"})
	for _, home := range []string{Relay, "relay-2.example.com:8443"} {
		if got := keysOn(t, home).home(ctx); got != home {
			t.Errorf("home %s, with two relays listed: now %s", home, got)
		}
	}
	UseRelays(nil)
	for _, home := range []string{"", "relay-2.example.com:8443", "gone.invalid"} {
		if got := keysOn(t, home).home(ctx); got != Relay {
			t.Errorf("home %q, with the main relay alone: now %s", home, got)
		}
	}
}

// With a choice, the nearest is one of the relays in the list. The same
// relay under two spellings counts as two.
func TestNearest(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list := []string{Relay, Relay + ":443"}
	if got := nearest(ctx, list); !slices.Contains(list, got) {
		t.Errorf("nearest of %v: %q", list, got)
	}
}

// A home that does not answer at the start gives way to the nearest other
// relay that does; a home that answers is kept.
func TestHomeMovesWhenDown(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	t.Cleanup(func() { UseRelays(nil) })
	friend, _ := NewKeys().ClientID()
	// listenOn starts a device whose home is home, and says which relay
	// its address points at, and that its keys now save that relay.
	listenOn := func(home string) []string {
		t.Helper()
		keys := keysOn(t, home)
		srv, err := Start(ctx, keys, []ClientID{friend})
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Close()
		on := srv.Addr().relays()
		var saved tailcfg.DERPRegion
		if json.Unmarshal(keys.Region, &saved) != nil || len(saved.Nodes) == 0 || !slices.Equal(on, []string{nameOf(saved.Nodes[0])}) {
			t.Errorf("home %s: the keys do not save the relay the address is on, %v", home, on)
		}
		return on
	}

	UseRelays([]string{"down.invalid"})
	if got := listenOn("down.invalid"); !slices.Equal(got, []string{Relay}) {
		t.Errorf("the home is down: the address is on %v, want %s", got, Relay)
	}
	other := Relay + ":443"
	UseRelays([]string{other})
	if got := listenOn(other); !slices.Equal(got, []string{other}) {
		t.Errorf("the home answers: the address is on %v, want %s", got, other)
	}
}
