package pairing

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"yonder/internal/names"
	"yonder/internal/tunnel"
	"yonder/internal/tunnel/tunneltest"
)

func device(name string) Device {
	return Device{Name: name, Fingerprint: "FP-" + name, Addr: tunnel.Addr("tc-" + name), ClientID: tunnel.ClientID("nodekey:" + name)}
}

// serve runs a Host on a local port the test server holds, and returns a
// dialler that reaches it whatever address is asked for, as the tunnel's
// dialler does.
func serve(t *testing.T, h *Host) func(context.Context, string, string) (net.Conn, error) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, srv.Listener.Addr().String())
	}
}

func TestPairingExchangesRecords(t *testing.T) {
	var stored Device
	h := &Host{
		Self:    device("host"),
		Confirm: func(_ context.Context, joiner Device) bool { return joiner.Name == "joiner" },
		Paired:  func(joiner Device) error { stored = joiner; return nil },
	}
	dial := serve(t, h)

	got, err := Join(context.Background(), dial, device("joiner"))
	if err != nil {
		t.Fatal(err)
	}
	if got != device("host") || stored != device("joiner") {
		t.Fatalf("joiner got %+v, host stored %+v", got, stored)
	}
	if !h.Done() {
		t.Fatal("host is not done after pairing")
	}
	// A pairing works once.
	if _, err := Join(context.Background(), dial, device("latecomer")); !errors.Is(err, ErrDeclined) {
		t.Fatalf("second use of the pairing: err %v", err)
	}
}

func TestPairingDeclined(t *testing.T) {
	stored := false
	h := &Host{
		Self:    device("host"),
		Confirm: func(context.Context, Device) bool { return false },
		Paired:  func(Device) error { stored = true; return nil },
	}
	dial := serve(t, h)
	if _, err := Join(context.Background(), dial, device("joiner")); !errors.Is(err, ErrDeclined) {
		t.Fatalf("err %v, want declined", err)
	}
	if stored || !h.Done() {
		t.Fatal("a declined device was stored, or the pairing is still open")
	}
	// After a "no" the link is used up: nobody is asked again.
	h.Confirm = func(context.Context, Device) bool { t.Error("the owner was asked again"); return true }
	if _, err := Join(context.Background(), dial, device("again")); !errors.Is(err, ErrDeclined) || stored {
		t.Fatalf("asking again after a no: err %v, stored %v", err, stored)
	}
}

// Once the link has expired nobody is asked.
func TestPairingExpires(t *testing.T) {
	h := &Host{
		Self:    device("host"),
		Confirm: func(context.Context, Device) bool { t.Error("the owner was asked"); return true },
		Paired:  func(Device) error { return nil },
		Expires: time.Now().Add(-time.Second),
	}
	if _, err := Join(context.Background(), serve(t, h), device("joiner")); !errors.Is(err, ErrDeclined) {
		t.Fatalf("err %v, want declined", err)
	}
}

// Ended hears how the question ended: paired, declined, or failed.
func TestPairingEnded(t *testing.T) {
	full := errors.New("disk full")
	for _, tc := range []struct {
		yes   bool
		saved error
		want  error
	}{{true, nil, nil}, {false, nil, ErrDeclined}, {true, full, full}} {
		var ended []error
		h := &Host{
			Self:    device("host"),
			Confirm: func(context.Context, Device) bool { return tc.yes },
			Paired:  func(Device) error { return tc.saved },
			Ended:   func(_ Device, err error) { ended = append(ended, err) },
		}
		Join(context.Background(), serve(t, h), device("joiner"))
		if len(ended) != 1 || !errors.Is(ended[0], tc.want) {
			t.Errorf("yes %v, saved %v: Ended heard %v, want once %v", tc.yes, tc.saved, ended, tc.want)
		}
	}
}

func TestPairingFailsIfTheHostCannotSave(t *testing.T) {
	h := &Host{
		Self:    device("host"),
		Confirm: func(context.Context, Device) bool { return true },
		Paired:  func(Device) error { return errors.New("disk full") },
	}
	if _, err := Join(context.Background(), serve(t, h), device("joiner")); !errors.Is(err, ErrHostNotStored) || !h.Done() {
		t.Fatalf("err %v, done %v; want ErrHostNotStored and the pairing over", err, h.Done())
	}
}

func TestIncompleteRecordsAreRefused(t *testing.T) {
	asked := false
	h := &Host{Self: device("host"), Confirm: func(context.Context, Device) bool { asked = true; return true }, Paired: func(Device) error { return nil }}
	dial := serve(t, h)
	incomplete := device("joiner")
	incomplete.ClientID = ""
	if _, err := Join(context.Background(), dial, incomplete); err == nil || asked {
		t.Fatalf("err %v, owner asked %v; want a refusal without asking", err, asked)
	}
}

func TestLinks(t *testing.T) {
	addr, err := ParseLink("  " + Link("tcSomeAddress_-123") + "\n")
	if err != nil || addr != "tcSomeAddress_-123" {
		t.Fatalf("got %q, %v", addr, err)
	}
	if want := "https://app.getyonder.org/#pair=tcNew"; Link("tcNew") != want {
		t.Fatalf("Link is %q, want %q", Link("tcNew"), want)
	}
	for _, bad := range []string{"", "tcSomeAddress", "yonder:pair/tcSomeAddress", linkPrefix, "https://example.com"} {
		if _, err := ParseLink(bad); err == nil {
			t.Errorf("ParseLink(%q) succeeded", bad)
		}
	}
}

func TestStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "yonder")
	s := Open(dir)

	keys, err := s.Keys()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir).Keys()
	if err != nil || again.Server != keys.Server || again.Client != keys.Client {
		t.Fatalf("keys changed on the second load (err %v)", err)
	}

	if devices, err := s.Devices(); err != nil || len(devices) != 0 {
		t.Fatalf("fresh store: %v, %v", devices, err)
	}
	renamed := device("b")
	renamed.Name = "b renamed"
	for _, d := range []Device{device("b"), device("a"), renamed} {
		if err := s.Add(d); err != nil {
			t.Fatal(err)
		}
	}
	devices, _ := Open(dir).Devices()
	if len(devices) != 2 || devices[0].Name != "a" || devices[1].Name != "b renamed" {
		t.Fatalf("devices: %+v", devices)
	}
	for _, c := range []struct {
		fingerprint, name string
		want              bool
	}{{"FP-b", "b again", true}, {"FP-b", "b again", false}, {"FP-b", " ", false}, {"FP-z", "z", false}} {
		if changed, err := s.Rename(c.fingerprint, c.name); err != nil || changed != c.want {
			t.Errorf("Rename(%q, %q) = %v, %v, want %v", c.fingerprint, c.name, changed, err, c.want)
		}
	}
	if devices, _ = Open(dir).Devices(); len(devices) != 2 || devices[1].Name != "b again" {
		t.Fatalf("devices after renaming: %+v", devices)
	}
	if err := s.Remove("FP-a"); err != nil {
		t.Fatal(err)
	}
	if devices, _ := s.Devices(); len(devices) != 1 || devices[0].Fingerprint != "FP-b" {
		t.Fatalf("after remove: %+v", devices)
	}

	for _, name := range []string{"tunnel.json", "devices.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s is mode %o, want 600", name, perm)
		}
	}
}

// A paired device's address can be replaced by one with the same keys on a
// relay in the list, and by nothing else.
func TestReaddress(t *testing.T) {
	s := Open(t.TempDir())
	bo := device("Bo")
	bo.Addr = tunneltest.Addr(t)
	if err := s.Add(bo); err != nil {
		t.Fatal(err)
	}
	was := tunnel.Relays()
	tunnel.UseRelays([]string{"relay2.example"})
	t.Cleanup(func() { tunnel.UseRelays(was) })
	on := func(relay string) tunnel.Addr {
		addr, err := bo.Addr.On(relay)
		if err != nil {
			t.Fatal(err)
		}
		return addr
	}
	addr := on("relay2.example")
	for _, tc := range []struct {
		what        string
		fingerprint string
		addr        tunnel.Addr
		changes     bool
	}{
		{"something that is not an address", bo.Fingerprint, "not an address", false},
		{"a device that is not paired", "FP-nobody", addr, false},
		{"other keys", bo.Fingerprint, tunneltest.Addr(t), false},
		{"a relay not in the list", bo.Fingerprint, on("elsewhere.example:8443"), false},
		{"a new address", bo.Fingerprint, addr, true},
		{"the same address again", bo.Fingerprint, addr, false},
	} {
		if changed, err := s.Readdress(tc.fingerprint, tc.addr); err != nil || changed != tc.changes {
			t.Errorf("%s: changed %v (err %v), want %v", tc.what, changed, err, tc.changes)
		}
	}
	want := bo
	want.Addr = addr
	if got, err := s.Devices(); err != nil || len(got) != 1 || got[0] != want {
		t.Errorf("stored %+v (err %v), want only %+v", got, err, want)
	}
}

// A device paired again with the same key is recorded afresh; a device
// stating the fingerprint of one already paired, with another key, is
// refused, and the old record stays.
func TestPairedFingerprintKeepsItsKey(t *testing.T) {
	s := Open(t.TempDir())
	bo := device("Bo")
	if err := s.Add(bo); err != nil {
		t.Fatal(err)
	}
	again := bo
	again.Name = "Bo's new name"
	if err := s.Add(again); err != nil {
		t.Fatalf("pairing Bo again with the same key: %v", err)
	}
	impostor := again
	impostor.ClientID, impostor.Addr = "nodekey:impostor", "tc-impostor"
	if err := s.Add(impostor); !errors.Is(err, ErrPairedUnderAnotherKey) {
		t.Fatalf("pairing Bo's fingerprint under another key: err %v", err)
	}
	// The same fingerprint in other letters is the same device.
	impostor.Fingerprint = strings.ToUpper(impostor.Fingerprint)
	if err := s.Add(impostor); !errors.Is(err, ErrPairedUnderAnotherKey) {
		t.Fatalf("pairing Bo's fingerprint in capitals under another key: err %v", err)
	}
	impostor.Fingerprint = again.Fingerprint
	if got, _ := s.Devices(); len(got) != 1 || got[0] != again {
		t.Fatalf("stored %+v, want only %+v", got, again)
	}

	// The joiner is told why.
	h := &Host{Self: device("host"), Confirm: func(context.Context, Device) bool { return true }, Paired: s.Add}
	if _, err := Join(context.Background(), serve(t, h), impostor); !errors.Is(err, ErrPairedUnderAnotherKey) {
		t.Fatalf("joiner: err %v, want ErrPairedUnderAnotherKey", err)
	}
}

func TestNamesLoseControlCharacters(t *testing.T) {
	evil := "Bob\n2026-01-01 forged\x1b[2J‮"
	want := "Bob2026-01-01 forged[2J"
	if got := names.Clean(evil); got != want {
		t.Fatalf("names.Clean = %q, want %q", got, want)
	}

	s := Open(t.TempDir())
	if err := s.Add(device("Bob")); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.Rename("FP-Bob", evil); err != nil || !changed {
		t.Fatalf("rename: %v %v", changed, err)
	}
	devices, _ := s.Devices()
	if len(devices) != 1 || devices[0].Name != want {
		t.Fatalf("stored %+v", devices)
	}
	// A name with nothing left is refused, as an empty one is.
	if changed, _ := s.Rename("FP-Bob", "\n\x1b"); changed {
		t.Fatal("a name of control characters was taken")
	}

	// The exchange cleans the other device's name on both sides.
	var stored Device
	h := &Host{
		Self:    func() Device { d := device("host"); d.Name = "Host\nforged"; return d }(),
		Confirm: func(context.Context, Device) bool { return true },
		Paired:  func(j Device) error { stored = j; return nil },
	}
	dial := serve(t, h)
	j := device("joiner")
	j.Name = "Joiner\nforged"
	got, err := Join(context.Background(), dial, j)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Hostforged" || stored.Name != "Joinerforged" {
		t.Fatalf("host record %q, joiner record %q", got.Name, stored.Name)
	}
}

func TestWriteKeepsMode600OverAnOlderTmpFile(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "devices.json.tmp")
	if err := os.WriteFile(tmp, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chmod(tmp, 0o644)
	s := Open(dir)
	if err := s.Add(device("Bob")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("devices.json has mode %o", info.Mode().Perm())
	}
}
