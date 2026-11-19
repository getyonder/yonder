package app

import (
	"context"
	"net"
	"slices"
	"strconv"
	"testing"

	"yonder/internal/ownroute"
)

// A Node that listens on every interface, as the app does (":53317"),
// keeps a tied listener for each interface of a local network, and a
// connection to that interface's address is answered by the Node.
func TestNodeAnswersOnTiedListener(t *testing.T) {
	local := ownroute.Local()
	if len(local) == 0 {
		t.Skip("this computer has no interface of a local network")
	}
	n := startNode(t, Config{Dir: t.TempDir(), SaveDir: t.TempDir(), Name: "Open Harbour", Listen: ":0"})
	if n.tied == nil {
		t.Fatal("the Node has no tied listeners")
	}
	until(t, "a tied listener for "+local[0].Name, func() bool { return slices.Contains(n.tied.names(), local[0].Name) })
	var ip net.IP
	for _, ipn := range local[0].Nets {
		if v4 := ipn.IP.To4(); v4 != nil && v4.IsPrivate() {
			ip = v4
		}
	}
	other := newNode(t, "Asking Comet")
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(n.Info.Port))
	if !other.Reaches(ctx, []string{addr}, func(fp string) bool { return fp == n.Info.Fingerprint }) {
		t.Errorf("the Node did not answer at %s", addr)
	}
}
