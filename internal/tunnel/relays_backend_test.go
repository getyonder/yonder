package tunnel_test

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"testing"

	"yonder/internal/pairing"
	"yonder/internal/tunnel"
)

// This test reads the application server's folder, which is back-end code;
// packaging/publish.sh leaves files named _backend_test.go out of the public
// repository.

// The list the site serves is the list built in: the main relay alone.
func TestSiteRelayList(t *testing.T) {
	site := filepath.Join("..", "..", "cmd", "yonder-api", "files", "relays.json")
	useList(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, site)
	}))
	if err := tunnel.LoadRelays(context.Background(), pairing.Open(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	// With another main relay, what the site lists shows after it.
	old := tunnel.Relay
	defer func() { tunnel.Relay = old }()
	tunnel.Relay = "main.example.com"
	if got := tunnel.Relays(); !slices.Equal(got, []string{"main.example.com", "relay.getyonder.org"}) {
		t.Errorf("the site lists %v, want relay.getyonder.org alone", got[1:])
	}
}
