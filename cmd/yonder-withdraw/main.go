// yonder-withdraw makes the signed list of withdrawn versions that the
// site's latest.json carries (package withdrawn).
//
// To withdraw a version, or to let one back: change
// packaging/withdrawn.list.json, then, from the top of the repository,
//
//	YONDER_WITHDRAW_KEY=$(a command that prints the private key) go run ./cmd/yonder-withdraw
//
// which writes packaging/withdrawn.json. The key comes from a password
// manager at that moment; it is never in a file here. Commit both files and make a web
// release (packaging/api.sh); the apps see the list within a day.
//
//	yonder-withdraw -latest 0.1.31
//
// prints latest.json for that version with the list in it, and fails if
// the list's signature does not agree with the key in the app. It needs no
// key; packaging/api.sh uses it.
//
//	yonder-withdraw -newkey | (something that stores its input)
//
// makes the key, one time. The private half goes to the standard output,
// which must not be a terminal, and the public half, which goes into
// package withdrawn, to the standard error.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"yonder/internal/withdrawn"
)

// keyEnv is the environment variable the private key is read from: the
// key's 32-byte seed in base64, as -newkey writes it.
const keyEnv = "YONDER_WITHDRAW_KEY"

// page is where latest.json sends a person for a newer version.
const page = "https://getyonder.org/#download"

func main() {
	list := flag.String("list", "packaging/withdrawn.list.json", "the entries: {\"versions\":[{\"version\":\"0.1.30\",\"systems\":[\"windows\"]},{\"below\":\"0.1.29\"}]}")
	out := flag.String("out", "packaging/withdrawn.json", "the signed list")
	newkey := flag.Bool("newkey", false, "make a key: the private half to the standard output, the public half to the standard error")
	latest := flag.String("latest", "", "print latest.json for this version, with the signed list checked against the key in the app")
	flag.Parse()
	var err error
	switch {
	case *newkey:
		err = newKey(os.Stdout, os.Stderr)
	case *latest != "":
		err = printLatest(os.Stdout, *out, *latest, withdrawn.Key())
	default:
		err = sign(*list, *out, os.Getenv(keyEnv), withdrawn.Key(), time.Now())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "yonder-withdraw:", err)
		os.Exit(1)
	}
}

// newKey makes a key. The private half is written to secret, which must
// not be a terminal, so that it is never shown.
func newKey(secret *os.File, public io.Writer) error {
	if info, err := secret.Stat(); err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return errors.New("the standard output is a terminal; send it through a pipe to where the key is kept")
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(secret, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		return err
	}
	fmt.Fprintln(public, "public key, for internal/withdrawn/withdrawn.go:")
	fmt.Fprintln(public, base64.StdEncoding.EncodeToString(pub))
	return nil
}

// sign reads the entries at list, signs them with the private key (its
// seed in base64) as made at now, and writes the signed list to out. It
// refuses a private key that is not the pair of public, the key in the
// app, and checks what it wrote as the app will.
func sign(list, out, private string, public ed25519.PublicKey, now time.Time) error {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(private))
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New(keyEnv + " must hold the private key")
	}
	key := ed25519.NewKeyFromSeed(seed)
	if !key.Public().(ed25519.PublicKey).Equal(public) {
		return errors.New("the private key is not the pair of the public key in the app")
	}
	// The apps take a list only if it was made later than the one they
	// hold, so a clock that is behind would make a list that none takes.
	made := now.UTC().Truncate(time.Second)
	if before, err := read(out, public); err == nil && !made.After(before.Made) {
		return fmt.Errorf("%s was made %s, which is not before now (%s); is the clock right?", out, before.Made.Format(time.RFC3339), made.Format(time.RFC3339))
	}
	b, err := os.ReadFile(list)
	if err != nil {
		return err
	}
	var entries struct {
		Versions []withdrawn.Entry `json:"versions"`
	}
	if err := json.Unmarshal(b, &entries); err != nil {
		return fmt.Errorf("%s: %w", list, err)
	}
	signed, err := withdrawn.Sign(withdrawn.List{Made: made, Versions: entries.Versions}, key)
	if err != nil {
		return err
	}
	b, err = json.MarshalIndent(signed, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	l, err := read(out, public)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s: made %s, %d %s\n", out, l.Made.Format(time.RFC3339), len(l.Versions), plural(len(l.Versions), "entry", "entries"))
	return nil
}

// read reads the signed list at path and checks it with key.
func read(path string, key ed25519.PublicKey) (withdrawn.List, error) {
	_, l, err := readSigned(path, key)
	return l, err
}

func readSigned(path string, key ed25519.PublicKey) (withdrawn.Signed, withdrawn.List, error) {
	var signed withdrawn.Signed
	b, err := os.ReadFile(path)
	if err != nil {
		return signed, withdrawn.List{}, err
	}
	if err := json.Unmarshal(b, &signed); err != nil {
		return signed, withdrawn.List{}, fmt.Errorf("%s: %w", path, err)
	}
	l, err := signed.Open(key)
	if err != nil {
		return signed, l, fmt.Errorf("%s: %w", path, err)
	}
	return signed, l, nil
}

// printLatest writes latest.json for version to w, with the signed list at
// path in it, checked with key.
func printLatest(w io.Writer, path, version string, key ed25519.PublicKey) error {
	signed, _, err := readSigned(path, key)
	if err != nil {
		return err
	}
	b, err := json.Marshal(struct {
		Version string `json:"version"`
		Page    string `json:"page"`
		withdrawn.Signed
	}{version, page, signed})
	if err != nil {
		return err
	}
	// An older Yonder reads no more than 4 KB of the file.
	if len(b) >= 1<<12 {
		return fmt.Errorf("latest.json would be %d bytes; keep it under 4096 (use a \"below\" entry)", len(b))
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
