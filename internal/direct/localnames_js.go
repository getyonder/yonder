package direct

import (
	"context"

	"github.com/pion/webrtc/v4"
)

// localNames does nothing in a browser, which looks the ".local" names of
// the other end up itself.
func localNames(_ context.Context, d webrtc.SessionDescription) webrtc.SessionDescription { return d }
