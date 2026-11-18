//go:build server

package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
)

// tryout is where the update check asks and the key it checks a list of
// withdrawn versions with, in place of the site and the key in the app:
// YONDER_TRY_UPDATE_URL and YONDER_TRY_WITHDRAW_KEY (a public key, in
// base64). Only this build for trying things has them; see tryout.go.
func tryout() (updateURL string, key ed25519.PublicKey) {
	if b, err := base64.StdEncoding.DecodeString(os.Getenv("YONDER_TRY_WITHDRAW_KEY")); err == nil && len(b) == ed25519.PublicKeySize {
		key = b
	}
	return os.Getenv("YONDER_TRY_UPDATE_URL"), key
}
