// Package withdrawn is the list of Yonder's versions that have been
// withdrawn: versions with a fault that their people must be told of. The
// site's file about the newest version carries the list and a signature
// over it. The list is signed with a key that is never on the server, so
// one who takes the server cannot withdraw a version; the public half of
// the key is in this package. The package is apart from the window so that
// the command that signs a list, and the check before a release, do not
// carry the app with them.
package withdrawn

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

// publicKey is the public half of the key the list is signed with, in
// base64. The key was made on 2026-11-18 by yonder-withdraw -newkey; its
// private half is kept in a password manager and nowhere else.
const publicKey = "Jigoq+NnXdW0a8UfkrSbhyF597wMwlqmWH512yT6ckk="

// Key is the key a list's signature is checked with; nil if the text
// above is not a key, and then no list is accepted.
func Key() ed25519.PublicKey {
	key, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil
	}
	return key
}

// List is the signed list.
type List struct {
	// Made is when the list was signed. Of two lists, the one made later
	// holds.
	Made time.Time `json:"made"`
	// Versions holds the withdrawn versions.
	Versions []Entry `json:"versions"`
}

// Entry withdraws one version, or every version below one.
type Entry struct {
	// Version is the one version withdrawn, and Below the version that
	// every older one is withdrawn with. An entry has one of the two.
	Version string `json:"version,omitempty"`
	Below   string `json:"below,omitempty"`
	// Systems holds the systems the entry is for (windows, macos, linux,
	// as the update check names them); empty means every system.
	Systems []string `json:"systems,omitempty"`
}

// Signed is a list as it travels and as it is kept: the list's bytes and
// the signature over them, both in base64. The signature is over the exact
// bytes that are decoded, so the order of the fields does not matter.
type Signed struct {
	Withdrawn string `json:"withdrawn"`
	Signature string `json:"signature"`
}

// Sign makes the signed form of l with the private key.
func Sign(l List, key ed25519.PrivateKey) (Signed, error) {
	if err := l.valid(); err != nil {
		return Signed{}, err
	}
	b, err := json.Marshal(l)
	if err != nil {
		return Signed{}, err
	}
	return Signed{
		Withdrawn: base64.StdEncoding.EncodeToString(b),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, b)),
	}, nil
}

// Open checks s against key and reads the list in it. A list whose
// signature does not agree, or that cannot be read, is an error.
func (s Signed) Open(key ed25519.PublicKey) (List, error) {
	var l List
	if len(key) != ed25519.PublicKeySize {
		return l, errors.New("withdrawn: no key to check the list with")
	}
	b, err := base64.StdEncoding.DecodeString(s.Withdrawn)
	if err != nil {
		return l, errors.New("withdrawn: the list is not base64")
	}
	sig, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil {
		return l, errors.New("withdrawn: the signature is not base64")
	}
	if !ed25519.Verify(key, b, sig) {
		return l, errors.New("withdrawn: the signature does not agree with the list")
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return List{}, err
	}
	if err := l.valid(); err != nil {
		return List{}, err
	}
	return l, nil
}

// valid reports what is wrong with a list, if anything: no time, or an
// entry that is not one version or one "below", or that names a version
// that is not three numbers.
func (l List) valid() error {
	if l.Made.IsZero() {
		return errors.New("withdrawn: the list does not say when it was made")
	}
	for _, e := range l.Versions {
		if (e.Version == "") == (e.Below == "") {
			return errors.New("withdrawn: an entry must have a version or a below, and not both")
		}
		if _, ok := numbers(e.Version + e.Below); !ok {
			return errors.New("withdrawn: " + strconv.Quote(e.Version+e.Below) + " is not a version")
		}
	}
	return nil
}

// Has reports whether version is withdrawn on system (windows, macos or
// linux). A version that is not three numbers, a development build's say,
// is never withdrawn.
func (l List) Has(version, system string) bool {
	v, ok := numbers(version)
	if !ok {
		return false
	}
	for _, e := range l.Versions {
		if len(e.Systems) > 0 && !slices.Contains(e.Systems, system) {
			continue
		}
		if e.Version != "" {
			if w, ok := numbers(e.Version); ok && slices.Compare(v, w) == 0 {
				return true
			}
		} else if w, ok := numbers(e.Below); ok && slices.Compare(v, w) < 0 {
			return true
		}
	}
	return false
}

// System is the name a list has for the system Go calls goos.
func System(goos string) string {
	if goos == "darwin" {
		return "macos"
	}
	return goos
}

// numbers is the three numbers of a version; false if it is not three
// numbers with points between them.
func numbers(v string) ([]int, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil, false
	}
	out := make([]int, 3)
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || strings.TrimLeft(part, "0123456789") != "" {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}
