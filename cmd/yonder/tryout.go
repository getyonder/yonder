//go:build !server

package main

import "crypto/ed25519"

// tryout is where the update check asks and the key it checks a list of
// withdrawn versions with, in place of the site and the key in the app.
// Only a build for trying things (-tags server, whose window is a page) has
// them: the app that people run asks nobody else and trusts no other key.
func tryout() (updateURL string, key ed25519.PublicKey) { return "", nil }
