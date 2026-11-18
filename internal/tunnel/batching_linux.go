package tunnel

import "os"

// noBatching is the switch in Tailscale's code that stops it handing the
// tunnel's packets to Linux several at a time (UDP segmentation offload).
const noBatching = "TS_DEBUG_DISABLE_UDP_GSO"

// Linux refuses such a batch whole when its packets are larger than the
// interface they leave through allows, which they are through a VPN's
// interface with a small limit (Tailscale's is 1280 bytes; the tunnel's
// packets are up to 1388). Tailscale's code does not then fall back to
// single packets, so a transfer crawls on the ones sent again alone: 4 MiB
// took near 110 s where it takes 3 s. Single packets are split by the system
// and arrive, so batching is off, unless the user has set the switch.
func init() {
	if _, set := os.LookupEnv(noBatching); !set {
		os.Setenv(noBatching, "true")
	}
}
