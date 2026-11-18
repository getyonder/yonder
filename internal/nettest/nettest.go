// Package nettest gates the tests that go through the relay
// (tunnel.Relay), so the ordinary test run stays offline.
package nettest

import (
	"os"
	"testing"
)

// Need skips the test unless YONDER_NET_TESTS is set:
//
//	YONDER_NET_TESTS=1 go test ./...
func Need(t *testing.T) {
	t.Helper()
	if os.Getenv("YONDER_NET_TESTS") == "" {
		t.Skip("set YONDER_NET_TESTS=1 to run tests that use the relay")
	}
}
