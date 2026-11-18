package app

import (
	"context"
	"errors"
	"testing"

	"yonder/internal/pairing"
	"yonder/internal/tunnel/tunneltest"
)

// A pairing link is from anyone, and the address in it names its own
// relay. One that is not on Yonder's list is refused before anything is
// dialled: the node here has no store and no tunnel, so a Join that went
// further would not return this error.
func TestJoinRefusesALinkOnAnotherRelay(t *testing.T) {
	addr, err := tunneltest.Addr(t).On("relay.attacker.example")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Node{}).Join(ctx, pairing.Link(addr)); !errors.Is(err, ErrOtherRelay) {
		t.Fatalf("a link on another relay: err %v, want ErrOtherRelay", err)
	}
}
