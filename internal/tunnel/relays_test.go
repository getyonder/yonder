package tunnel_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"yonder/internal/pairing"
	"yonder/internal/tunnel"
)

// useList points the list at a server of the test's own, and puts back the
// list built in afterwards.
func useList(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	old := tunnel.ListURL
	tunnel.ListURL = srv.URL + "/relays.json"
	t.Cleanup(func() {
		srv.Close()
		tunnel.ListURL = old
		tunnel.UseRelays(nil)
	})
	return srv
}

const twoRelays = `{"Regions": {
	"2": {"RegionID": 2, "Nodes": [{"Name": "2", "RegionID": 2, "HostName": "relay-2.example.com", "DERPPort": 443}]},
	"1": {"RegionID": 1, "Nodes": [{"Name": "1", "RegionID": 1, "HostName": "relay.getyonder.org"}]}
}}`

// The list comes from the site, is kept in the settings folder, and the copy
// is used when the site cannot be reached; with neither, the list built in.
func TestRelayList(t *testing.T) {
	ctx := context.Background()
	if got := tunnel.Relays(); !slices.Equal(got, []string{tunnel.Relay}) {
		t.Fatalf("the list built in: %v", got)
	}
	srv := useList(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(twoRelays))
	}))
	dir := t.TempDir()
	store := pairing.Open(dir)
	if err := tunnel.LoadRelays(ctx, store); err != nil {
		t.Fatal(err)
	}
	want := []string{tunnel.Relay, "relay-2.example.com:443"}
	if got := tunnel.Relays(); !slices.Equal(got, want) {
		t.Fatalf("from the site: %v, want %v", got, want)
	}
	if data, _, ok := store.RelayList(); !ok || string(data) != twoRelays {
		t.Fatalf("the copy kept: %v, %q", ok, data)
	}

	// The site is gone and the copy is old: the copy still serves.
	srv.Close()
	tunnel.UseRelays(nil)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "relays.json"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := tunnel.LoadRelays(ctx, store); err != nil {
		t.Fatal(err)
	}
	if got := tunnel.Relays(); !slices.Equal(got, want) {
		t.Fatalf("from the copy: %v, want %v", got, want)
	}

	// No site and no copy: the list stays as it was.
	tunnel.UseRelays(nil)
	if err := tunnel.LoadRelays(ctx, pairing.Open(t.TempDir())); err == nil {
		t.Error("a list was loaded from nowhere")
	}
	if got := tunnel.Relays(); !slices.Equal(got, []string{tunnel.Relay}) {
		t.Fatalf("with no list to be had: %v", got)
	}
}

// A broken or hostile list is held to at most 16 relays, all on port 443;
// one with no such relay leaves the list built in.
func TestBrokenRelayList(t *testing.T) {
	var list string
	useList(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(list))
	}))
	// regions is a list of n relays, with the given port.
	regions := func(n int, port func(i int) int) string {
		var r []string
		for i := 1; i <= n; i++ {
			r = append(r, fmt.Sprintf(`"%d": {"RegionID": %d, "Nodes": [{"Name": "%d", "RegionID": %d, "HostName": "r%02d.example.com", "DERPPort": %d}]}`, i, i, i, i, i, port(i)))
		}
		return `{"Regions": {` + strings.Join(r, ",") + `}}`
	}

	list = regions(40, func(i int) int {
		if i%2 == 0 {
			return 22
		}
		return 443
	})
	if err := tunnel.LoadRelays(context.Background(), pairing.Open(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	got := tunnel.Relays()[1:]
	if len(got) != 16 || got[0] != "r01.example.com:443" || got[15] != "r31.example.com:443" {
		t.Errorf("from 40 relays, half on port 22: %v", got)
	}

	list = regions(3, func(int) int { return 8443 })
	if err := tunnel.LoadRelays(context.Background(), pairing.Open(t.TempDir())); err == nil {
		t.Error("a list with no relay on port 443 was taken")
	}
	if got := tunnel.Relays(); !slices.Equal(got, []string{tunnel.Relay}) {
		t.Errorf("after a list with nothing usable: %v", got)
	}
}

// A relay of the list is taken only if its name is a plain DNS name: a name
// with a port, a path or a user, and an IP address, are left out.
func TestRelayListNames(t *testing.T) {
	useList(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Regions": {
			"1": {"RegionID": 1, "Nodes": [{"Name": "1", "RegionID": 1, "HostName": "127.0.0.1:22"}]},
			"2": {"RegionID": 2, "Nodes": [{"Name": "2", "RegionID": 2, "HostName": "192.168.1.1"}]},
			"3": {"RegionID": 3, "Nodes": [{"Name": "3", "RegionID": 3, "HostName": "u@h/path"}]},
			"4": {"RegionID": 4, "Nodes": [{"Name": "4", "RegionID": 4, "HostName": "[::1]"}]},
			"6": {"RegionID": 6, "Nodes": [{"Name": "6", "RegionID": 6, "HostName": "127.1"}]},
			"7": {"RegionID": 7, "Nodes": [{"Name": "7", "RegionID": 7, "HostName": "0x7f000001"}]},
			"8": {"RegionID": 8, "Nodes": [{"Name": "8", "RegionID": 8, "HostName": "localhost"}]},
			"9": {"RegionID": 9, "Nodes": [{"Name": "9", "RegionID": 9, "HostName": "h.example.com?x#y"}]},
			"5": {"RegionID": 5, "Nodes": [{"Name": "5", "RegionID": 5, "HostName": "relay-5.example.com"}]}
		}}`))
	}))
	if err := tunnel.LoadRelays(context.Background(), pairing.Open(t.TempDir())); err != nil {
		t.Fatal(err)
	}
	if got := tunnel.Relays()[1:]; !slices.Equal(got, []string{"relay-5.example.com"}) {
		t.Errorf("relays taken from the list: %v, want relay-5.example.com alone", got)
	}
}
