package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yonder/internal/withdrawn"
)

// pair is a test key: the private half as the environment holds it.
func pair(t *testing.T) (private string, public ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(priv.Seed()), pub
}

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "withdrawn.list.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

var now = time.Date(2026, 12, 1, 10, 0, 0, 500, time.FixedZone("", 3600))

func TestSignThenLatest(t *testing.T) {
	private, public := pair(t)
	list := write(t, `{"versions":[{"version":"0.1.30","systems":["windows"]},{"below":"0.1.29"}]}`)
	out := filepath.Join(t.TempDir(), "withdrawn.json")
	if err := sign(list, out, private+"\n", public, now); err != nil {
		t.Fatal(err)
	}
	l, err := read(out, public)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Made.Equal(time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)) || !l.Has("0.1.30", "windows") || l.Has("0.1.30", "linux") || !l.Has("0.1.28", "linux") {
		t.Fatalf("the list: %+v", l)
	}

	// A second list at the same time or before, as with a clock that is
	// behind, is refused, and the first stays; a later one is made.
	for _, at := range []time.Time{now, now.Add(-time.Hour)} {
		if err := sign(list, out, private, public, at); err == nil {
			t.Fatalf("a list was made at %v over one made at %v", at, now)
		}
	}
	if l, err := read(out, public); err != nil || !l.Has("0.1.28", "linux") {
		t.Fatalf("the first list after the refusals: %+v, %v", l, err)
	}
	if err := sign(list, out, private, public, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	// latest.json as the app reads it: the two old fields and the list.
	var buf bytes.Buffer
	if err := printLatest(&buf, out, "0.1.31", public); err != nil {
		t.Fatal(err)
	}
	var file struct {
		Version, Page string
		withdrawn.Signed
	}
	if err := json.Unmarshal(buf.Bytes(), &file); err != nil {
		t.Fatal(err)
	}
	if file.Version != "0.1.31" || file.Page != page {
		t.Fatalf("latest.json: %s", buf.String())
	}
	if got, err := file.Open(public); err != nil || !got.Has("0.1.28", "macos") {
		t.Fatalf("the list in latest.json: %+v, %v", got, err)
	}

	// Another key in the app: the release's check fails.
	_, other := pair(t)
	if err := printLatest(&buf, out, "0.1.31", other); err == nil {
		t.Fatal("a list that does not agree with the app's key was passed")
	}
	// No key in the app yet, or no list.
	if err := printLatest(&buf, out, "0.1.31", nil); err == nil {
		t.Fatal("passed with no key")
	}
	if err := printLatest(&buf, filepath.Join(t.TempDir(), "none.json"), "0.1.31", public); err == nil {
		t.Fatal("passed with no list")
	}
}

func TestSignRefuses(t *testing.T) {
	private, public := pair(t)
	otherPrivate, _ := pair(t)
	good := write(t, `{"versions":[]}`)
	for name, c := range map[string]struct{ list, private string }{
		"no key":            {good, ""},
		"not a key":         {good, "c2VjcmV0"},
		"another key":       {good, otherPrivate},
		"no file":           {filepath.Join(t.TempDir(), "none.json"), private},
		"not json":          {write(t, `versions: 0.1.30`), private},
		"not a version":     {write(t, `{"versions":[{"version":"latest"}]}`), private},
		"version and below": {write(t, `{"versions":[{"version":"0.1.30","below":"0.1.29"}]}`), private},
	} {
		out := filepath.Join(t.TempDir(), "withdrawn.json")
		if err := sign(c.list, out, c.private, public, now); err == nil {
			t.Errorf("%s: signed", name)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("%s: a file was written", name)
		}
	}
	// No entries is a good list: it lets every version back.
	out := filepath.Join(t.TempDir(), "withdrawn.json")
	if err := sign(good, out, private, public, now); err != nil {
		t.Fatal(err)
	}
}

func TestLatestTooLong(t *testing.T) {
	private, public := pair(t)
	var entries []string
	for range 200 {
		entries = append(entries, `{"version":"0.1.30","systems":["windows","macos","linux"]}`)
	}
	out := filepath.Join(t.TempDir(), "withdrawn.json")
	if err := sign(write(t, `{"versions":[`+strings.Join(entries, ",")+`]}`), out, private, public, now); err != nil {
		t.Fatal(err)
	}
	if err := printLatest(&bytes.Buffer{}, out, "0.1.31", public); err == nil {
		t.Fatal("a latest.json over 4 KB was passed")
	}
}

func TestNewKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	var public bytes.Buffer
	if err := newKey(f, &public); err != nil {
		t.Fatal(err)
	}
	f.Close()
	private, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(public.String()), "\n")
	pub, err := base64.StdEncoding.DecodeString(lines[len(lines)-1])
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("public half: %q", public.String())
	}
	if strings.Contains(public.String(), strings.TrimSpace(string(private))) {
		t.Fatal("the private half is shown")
	}
	// The two halves are a pair: a list signed with one opens with the other.
	out := filepath.Join(t.TempDir(), "withdrawn.json")
	if err := sign(write(t, `{"versions":[{"below":"0.1.29"}]}`), out, string(private), pub, now); err != nil {
		t.Fatal(err)
	}
}
