package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateKeepsTheSameIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "yonder")
	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Fingerprint) != 64 {
		t.Fatalf("fingerprint %q is not a SHA-256 in hex", first.Fingerprint)
	}
	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second.Fingerprint != first.Fingerprint {
		t.Fatal("fingerprint changed on the second load")
	}
	info, err := os.Stat(filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("private key is mode %o, want 600", perm)
	}
}
