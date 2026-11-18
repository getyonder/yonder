// Package firewall reads whether this computer's own firewall keeps devices
// on the network from reaching Yonder, and puts it right when the person
// asks. What each system allows, and how sure that is, is in
// docs/notes/2026-11-15-firewall-notice-requirements.md.
package firewall

import (
	"errors"
	"fmt"
)

// DiscoveryPort is where devices announce themselves (UDP), whatever port
// Yonder itself listens on.
const DiscoveryPort = 53317

// Status is what could be read without administrator rights.
type Status struct {
	// On means a firewall this package knows is on, whether or not its
	// rules could be read.
	On bool
	// Blocking means it is known to keep devices out of Yonder's port.
	Blocking bool
}

// ErrCancelled means the person backed out of the system's prompt.
var ErrCancelled = errors.New("cancelled")

// needed lists what must be let in for a Yonder on port: its own port, and
// the announcements of other devices.
func needed(port int) []string {
	return []string{fmt.Sprintf("%d/tcp", port), fmt.Sprintf("%d/udp", DiscoveryPort)}
}
