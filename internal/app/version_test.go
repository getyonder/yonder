package app

import (
	"context"
	"strings"
	"testing"
)

// Two paired Yonders tell each other their versions: the one that asks
// with its question, the one that answers with the answer.
func TestPairedDevicesTellTheirVersions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB, toA := wiredPair(t)
	a.version, b.version = "0.1.32", "0.1.31"
	if v, heard := a.PeerVersion(toB.Fingerprint); heard || v != "" {
		t.Fatalf("before a word: %q, heard %v", v, heard)
	}
	if !a.Online(ctx, toB) {
		t.Fatal("B did not answer")
	}
	if v, heard := a.PeerVersion(toB.Fingerprint); !heard || v != "0.1.31" {
		t.Fatalf("A has B as %q, heard %v", v, heard)
	}
	if v, heard := b.PeerVersion(toA.Fingerprint); !heard || v != "0.1.32" {
		t.Fatalf("B has A as %q, heard %v", v, heard)
	}
}

// A Yonder from before versions were told says none; it is heard, and its
// version is empty. One that says something that is not a version is the
// same, and a development build says "dev".
func TestVersionsThatAreNotTold(t *testing.T) {
	for said, want := range map[string]string{"": "", "soon": "", "0.1": "", "dev": "dev", strings.Repeat("9", 40): ""} {
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		a, b, toB, toA := wiredPair(t)
		a.version, b.version = said, said
		if !a.Online(ctx, toB) {
			t.Fatalf("%q: B did not answer", said)
		}
		cancel()
		if v, heard := a.PeerVersion(toB.Fingerprint); !heard || v != want {
			t.Errorf("%q in an answer: A has B as %q, heard %v", said, v, heard)
		}
		if v, heard := b.PeerVersion(toA.Fingerprint); !heard || v != want {
			t.Errorf("%q in a question: B has A as %q, heard %v", said, v, heard)
		}
	}
}

func TestToldVersion(t *testing.T) {
	for version, want := range map[string]string{"0.1.31": "0.1.31", "": "dev", "0.1.31-test": "dev", "0.1": "dev"} {
		if got := ToldVersion(version); got != want {
			t.Errorf("ToldVersion(%q) = %q, want %q", version, got, want)
		}
	}
}
