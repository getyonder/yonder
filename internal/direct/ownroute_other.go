//go:build !linux && !js

package direct

import "github.com/pion/transport/v5"

// newOwnRoute gives no network of its own: only on Linux is one needed
// (ownroute_linux.go).
func newOwnRoute() (transport.Net, error) { return nil, nil }
