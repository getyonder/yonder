//go:build !js

package direct

// The desktop build: pion's own WebRTC, with the settings that the plan
// and the security review ask for. pion_js.go is the browser's side of the
// same three things: newPeerConnection, gathering and prepare, and the
// constant receiverCanWait.

import (
	"context"
	"math"
	"net"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"

	"yonder/internal/ownroute"
)

// receiverCanWait: pion calls a channel's message callback from one
// goroutine, one message after the other, so the callback can hold the
// channel's reading back by not returning.
const receiverCanWait = true

// checkInterval is the time between pion's connectivity checks on one
// pair of addresses.
const checkInterval = 200 * time.Millisecond

// newPeerConnection makes a peer connection that gets limit to connect.
func newPeerConnection(cfg Config, limit time.Duration) (*webrtc.PeerConnection, error) {
	se := webrtc.SettingEngine{}

	// Nothing of pion's is logged: at its finer levels it writes
	// descriptions and addresses.
	se.LoggerFactory = quiet{}

	if n, err := newOwnRoute(); err == nil && n != nil && !cfg.Loopback {
		se.SetNet(n)
	}

	// The ".local" names are asked for in localnames.go, not by pion.
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)

	if cfg.Loopback {
		se.SetIncludeLoopbackCandidate(true)
		se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	} else {
		se.SetInterfaceFilter(ownroute.UsableInterface)
		se.SetIPFilter(ownroute.UsableIP)
		se.SetRemoteIPFilter(ownroute.UsableIP)
	}

	// pion gives up on a pair of addresses after 7 checks, under two
	// seconds. A hole through two routers can take longer, and the other
	// end starts later than this one, so the checks last the whole limit.
	se.SetICEMaxBindingRequests(uint16(min(int64(limit/checkInterval)+100, math.MaxUint16)))
	// A connection that formed and then goes silent has failed after 25
	// seconds, as pion has it; one that never formed, after the limit.
	se.SetICETimeouts(5*time.Second, max(limit, 25*time.Second), 2*time.Second)
	// pion asks the STUN server from every interface and waits 5 seconds
	// for those that get no reply.
	se.SetSTUNGatherTimeout(min(limit, 3*time.Second))

	// The limits of the security review (its condition 4).
	se.SetDTLSConnectContextMaker(func() (context.Context, func()) {
		return context.WithTimeout(context.Background(), limit)
	})
	se.SetSCTPMaxMessageSize(maxMessage)
	se.SetSCTPMaxReceiveBufferSize(readLimit)

	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	return api.NewPeerConnection(webrtc.Configuration{ICEServers: cfg.servers()})
}

// gathering must be called before the local description is set. The
// function it returns waits until every address is in the description.
func gathering(pc *webrtc.PeerConnection, stun bool) func(ctx context.Context, done <-chan struct{}) error {
	complete := webrtc.GatheringCompletePromise(pc)
	return func(ctx context.Context, done <-chan struct{}) error {
		select {
		case <-complete:
			return nil
		case <-done:
			return ErrLost
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// prepare has nothing to do here; see pion_js.go.
func prepare(*webrtc.DataChannel) {}

// quiet is a logger for pion that writes nothing.
type quiet struct{}

func (quiet) NewLogger(string) logging.LeveledLogger { return quiet{} }

func (quiet) Trace(string)          {}
func (quiet) Tracef(string, ...any) {}
func (quiet) Debug(string)          {}
func (quiet) Debugf(string, ...any) {}
func (quiet) Info(string)           {}
func (quiet) Infof(string, ...any)  {}
func (quiet) Warn(string)           {}
func (quiet) Warnf(string, ...any)  {}
func (quiet) Error(string)          {}
func (quiet) Errorf(string, ...any) {}
