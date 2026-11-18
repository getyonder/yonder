// Package tunneltest makes tunnel addresses for tests that stay offline.
package tunneltest

import (
	"testing"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"

	"yonder/internal/tunnel"
)

// Addr is a well-formed address, on tunnel.Relay, of a device that does not
// exist.
func Addr(t *testing.T) tunnel.Addr {
	t.Helper()
	info := tailcat.ConnInfo{
		ServerPublic:      tailcat.NodePublic{NodePublic: key.NewNode().Public()},
		ServerDiscoPublic: tailcat.DiscoPublic{DiscoPublic: key.NewDisco().Public()},
		PresharedKey:      tailcat.NewPresharedKey(),
		// Any region will do; On puts the relay in its place.
		RegionID: 1,
	}
	addr, err := tunnel.Addr(info.Addr()).On(tunnel.Relay)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}
