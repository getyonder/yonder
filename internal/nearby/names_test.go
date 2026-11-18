package nearby

import (
	"context"
	"strings"
	"testing"

	"yonder/internal/names"
)

// The meeting point passes on the names devices give; a device does not
// trust it to have cleaned them. A name is cleaned as it is taken in, so
// the list of nearby devices and the code dialog show it clean.
func TestNamesFromTheMeetingPointAreCleaned(t *testing.T) {
	c := &Client{}
	evil := "Anna\u202e\n" + strings.Repeat("a", 1000)
	want := "Anna" + strings.Repeat("a", names.Max-4)
	if err := c.handle(context.Background(), nil, Message{Type: TypeWelcome, ID: "me", Peers: []Peer{{ID: "1", Name: evil}}}); err != nil {
		t.Fatal(err)
	}
	if err := c.handle(context.Background(), nil, Message{Type: TypeJoin, Peer: &Peer{ID: "2", Name: evil}}); err != nil {
		t.Fatal(err)
	}
	peers := c.Peers()
	if len(peers) != 2 {
		t.Fatalf("%d devices nearby, want 2", len(peers))
	}
	for _, p := range peers {
		if p.Name != want {
			t.Errorf("device %s is shown as %q, want %q", p.ID, p.Name, want)
		}
	}
}
