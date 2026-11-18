package withdrawn

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"
)

func keys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

var made = time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)

func TestSignAndOpen(t *testing.T) {
	pub, priv := keys(t)
	list := List{Made: made, Versions: []Entry{{Version: "0.1.30", Systems: []string{"windows"}}, {Below: "0.1.29"}}}
	signed, err := Sign(list, priv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := signed.Open(pub)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Made.Equal(made) || len(got.Versions) != 2 || got.Versions[1].Below != "0.1.29" {
		t.Fatalf("opened: %+v", got)
	}

	// One changed byte of the list, and the signature does not agree.
	b, _ := base64.StdEncoding.DecodeString(signed.Withdrawn)
	b[len(b)/2] ^= 1
	changed := Signed{Withdrawn: base64.StdEncoding.EncodeToString(b), Signature: signed.Signature}
	if _, err := changed.Open(pub); err == nil {
		t.Fatal("a changed list was accepted")
	}

	// Another key's signature, no key, and no list.
	other, _ := keys(t)
	if _, err := signed.Open(other); err == nil {
		t.Fatal("a list signed with another key was accepted")
	}
	if _, err := signed.Open(nil); err == nil {
		t.Fatal("a list was accepted with no key")
	}
	if _, err := (Signed{}).Open(pub); err == nil {
		t.Fatal("an empty list was accepted")
	}
}

func TestSignedButWrong(t *testing.T) {
	pub, priv := keys(t)
	for name, list := range map[string]List{
		"no time":           {Versions: []Entry{{Version: "0.1.30"}}},
		"version and below": {Made: made, Versions: []Entry{{Version: "0.1.30", Below: "0.1.29"}}},
		"neither":           {Made: made, Versions: []Entry{{Systems: []string{"linux"}}}},
		"not a version":     {Made: made, Versions: []Entry{{Below: "latest"}}},
		"two numbers":       {Made: made, Versions: []Entry{{Version: "0.1"}}},
	} {
		if _, err := Sign(list, priv); err == nil {
			t.Errorf("%s: signed", name)
		}
	}
	// A list with no versions is a good one: it lets every version back.
	signed, err := Sign(List{Made: made}, priv)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := signed.Open(pub); err != nil || l.Has("0.1.30", "linux") {
		t.Fatalf("empty list: %+v, %v", l, err)
	}
}

func TestHas(t *testing.T) {
	list := List{Made: made, Versions: []Entry{
		{Version: "0.1.30", Systems: []string{"windows"}},
		{Below: "0.1.29"},
		{Version: "0.2.0", Systems: []string{"macos", "linux"}},
	}}
	for _, c := range []struct {
		version, system string
		want            bool
	}{
		{"0.1.30", "windows", true},
		{"0.1.30", "linux", false}, // a system that is not named
		{"0.1.30", "macos", false},
		{"0.1.28", "linux", true}, // below
		{"0.1.9", "macos", true},
		{"0.0.99", "windows", true},
		{"0.1.29", "linux", false}, // below is not "up to"
		{"0.1.31", "windows", false},
		{"0.2.0", "macos", true},
		{"0.2.0", "linux", true},
		{"0.2.0", "windows", false},
		{"00.02.0", "linux", true}, // the same version, written differently
		{"", "linux", false},       // a development build
		{"dev", "linux", false},
		{"0.1", "linux", false},
		{"0.1.28-test", "linux", false},
		{"0.1.28.1", "linux", false},
	} {
		if got := list.Has(c.version, c.system); got != c.want {
			t.Errorf("Has(%q, %q) = %v, want %v", c.version, c.system, got, c.want)
		}
	}
}

func TestSystem(t *testing.T) {
	for goos, want := range map[string]string{"darwin": "macos", "windows": "windows", "linux": "linux"} {
		if got := System(goos); got != want {
			t.Errorf("System(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestTheAppHasAKey(t *testing.T) {
	if len(Key()) != ed25519.PublicKeySize {
		t.Fatal("the public key in the code is not a key")
	}
}
