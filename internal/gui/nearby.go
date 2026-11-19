package gui

import (
	"context"
	"errors"
	"strings"
	"time"

	"yonder/internal/app"
	"yonder/internal/nearby"
	"yonder/internal/pairing"
)

// Devices on the same network that Yonder cannot find by itself (a web page
// cannot search the network) are found through a meeting point: see package
// nearby. Here they join the device list with a Pair button, and pairing
// with one goes through a dialog where both owners compare a code.

const (
	// meetPrefix starts the ID of a device known only through the meeting
	// point; no fingerprint starts this way.
	meetPrefix = "nearby:"
	// askWait is how long a nearby device gets to answer a request to
	// pair, before any person is involved.
	askWait = 20 * time.Second
	// joinWait is how long pairing may take once both owners have said yes.
	joinWait = 2 * time.Minute
)

// Code is the dialog for pairing with a nearby device.
type Code struct {
	// Name is the other device.
	Name string `json:"name"`
	// Code is the six digits both devices show.
	Code string `json:"code"`
	// Asked is whether the other device started this.
	Asked bool `json:"asked"`
	// Waiting is whether this device's owner has said the codes match and
	// the rest is up to the other device.
	Waiting bool `json:"waiting"`
}

type codeDialog struct {
	Code
	session *nearby.Session
	// room is whether the two met by a code (room.go), not as nearby
	// devices.
	room bool
	// ctx ends when the dialog closes, whatever the reason, and with it
	// whatever the pairing was in the middle of.
	ctx  context.Context
	stop context.CancelFunc
}

// meetNearby goes to the meeting point, if there is one and the person has
// not turned it off, and stays until leaveNearby. s.mu must not be held.
func (s *Service) meetNearby() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meeting == "" || s.set.HideNearby || s.meet != nil {
		return
	}
	node := s.node
	c := &nearby.Client{
		URL: s.meeting, Name: s.set.Name, Kind: s.kind, Version: app.ToldVersion(s.says), Fingerprint: s.node.Info.Fingerprint,
		Changed: func() { s.changed(); s.kickFirewall(); node.NetworkChanged() }, Asked: s.asked,
	}
	if n := s.node; s.kind == nearby.KindApp {
		// Tell a nearby Yonder that asks whether this one can connect to it.
		c.Reach = func(ctx context.Context, from nearby.Peer, addrs []string) bool {
			return n.Reaches(ctx, addrs, from.Is)
		}
	}
	ctx, stop := context.WithCancel(context.Background())
	s.meet, s.stopMeet = c, stop
	go c.Run(ctx)
}

// leaveNearby ends the connection to the meeting point, if there is one.
// s.mu must not be held.
func (s *Service) leaveNearby() {
	s.mu.Lock()
	stop := s.stopMeet
	s.meet, s.stopMeet = nil, nil
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// SetVisible turns showing this device to others on the same network on or
// off. Off means no connection to the meeting point at all.
func (s *Service) SetVisible(on bool) error {
	s.mu.Lock()
	if on == !s.set.HideNearby {
		s.mu.Unlock()
		return nil
	}
	s.set.HideNearby = !on
	err := s.set.Save(s.dir)
	s.mu.Unlock()
	if on {
		s.meetNearby()
	} else {
		s.leaveNearby()
	}
	s.changed()
	return err
}

// meetDevices lists the devices at the meeting point that are not already
// in the list as paired or seen on the local network. s.mu must be held.
func (s *Service) meetDevices(known map[string]*Device) []Device {
	if s.meet == nil {
		return nil
	}
	var out []Device
peers:
	for _, p := range s.meet.Peers() {
		for fingerprint, d := range known {
			if p.Is(fingerprint) {
				// A paired device that is here is on this network too. It
				// is reached through the tunnel, so only once that answers.
				if d.Presence == "online" {
					d.Nearby = true
				}
				continue peers
			}
		}
		model := "Yonder"
		if p.Kind == nearby.KindBrowser {
			model = "Yonder in a browser"
		}
		out = append(out, Device{ID: meetPrefix + p.ID, Name: p.Name, Model: model, Nearby: true, Presence: "online", Pairable: true})
	}
	return out
}

// PairNearby asks a nearby device to pair. When it has answered, both
// devices show a code; AnswerCode takes it from there.
func (s *Service) PairNearby(id string) error {
	s.mu.Lock()
	c := s.meet
	busy := s.code != nil
	s.mu.Unlock()
	peerID, ok := strings.CutPrefix(id, meetPrefix)
	if c == nil || !ok {
		return errors.New("That device is no longer nearby.")
	}
	if busy {
		return errors.New("Finish the pairing that is already open first.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), askWait)
	defer cancel()
	session, err := c.Pair(ctx, peerID)
	switch {
	case errors.Is(err, nearby.ErrRefused):
		return errors.New("That device is busy with another pairing. Try again in a moment.")
	case errors.Is(err, nearby.ErrBusy):
		return errors.New("Finish the pairing that is already open first.")
	case errors.Is(err, nearby.ErrGone):
		return errors.New("That device is no longer nearby.")
	case err != nil:
		return errors.New("That device did not answer. Check Yonder is still open on it and try again.")
	}
	s.showCode(session, false)
	return nil
}

// asked is called when a nearby device wants to pair with this one.
func (s *Service) asked(session *nearby.Session) {
	s.showCode(session, false)
	s.shell.Attention()
}

// showCode opens the code dialog for a session and closes it again if the
// session ends before the pairing is made. room is whether the two devices
// met by a code. Without a dialog in return, one was already open and the
// session has been called off.
func (s *Service) showCode(session *nearby.Session, room bool) *codeDialog {
	dialog := &codeDialog{Code: Code{Name: session.Peer.Name, Code: session.Code, Asked: session.Asked}, session: session, room: room}
	dialog.ctx, dialog.stop = context.WithCancel(context.Background())
	s.mu.Lock()
	if s.code != nil {
		s.mu.Unlock()
		session.Close()
		return nil
	}
	s.code = dialog
	s.mu.Unlock()
	s.changed()
	go func() {
		<-session.Done()
		err := session.Err()
		// Ended from this side: that side sees to the dialog.
		if err == nil || !s.closeCode(dialog) {
			return
		}
		switch {
		case errors.Is(err, nearby.ErrRefused):
			s.shell.Say(dialog.Name + " did not pair.")
		case errors.Is(err, nearby.ErrExpired):
			s.shell.Say("The pairing with " + dialog.Name + " took too long. Try again.")
		case dialog.room:
			s.shell.Say(dialog.Name + " left. Pair again with a new code.")
		default:
			s.shell.Say(dialog.Name + " is no longer nearby.")
		}
	}()
	return dialog
}

// closeCode takes the code dialog down and stops what it was doing, and
// reports whether it was up.
func (s *Service) closeCode(dialog *codeDialog) bool {
	s.mu.Lock()
	up := s.code == dialog
	if up {
		s.code = nil
	}
	s.mu.Unlock()
	if up {
		dialog.stop()
		s.changed()
	}
	return up
}

// AnswerCode is this device's owner saying whether the other device shows
// the same code. No calls the pairing off, also after a yes while the
// other device is awaited; yes makes the pairing once the other owner has
// said yes too.
func (s *Service) AnswerCode(yes bool) {
	s.mu.Lock()
	dialog, n := s.code, s.node
	if dialog == nil || (yes && dialog.Waiting) {
		s.mu.Unlock()
		return
	}
	if yes {
		dialog.Waiting = true
	}
	s.mu.Unlock()
	if !yes {
		dialog.session.Close()
		s.closeCode(dialog)
		return
	}
	s.changed()
	go func() {
		var other pairing.Device
		var err error
		if dialog.Asked {
			other, err = joinNearby(dialog.ctx, n, dialog.session)
		} else {
			other, err = hostNearby(dialog.ctx, n, dialog.session)
		}
		if dialog.session.Err() != nil {
			return // the session's own ending is reported where it is watched
		}
		// Heard by the other device only if the session is still going,
		// which is when this side failed before the link crossed.
		dialog.session.Close()
		// If the dialog has gone, whoever closed it has said why.
		if !s.closeCode(dialog) {
			return
		}
		if err != nil {
			s.shell.Say("Could not pair with " + dialog.Name + ". Check both devices are online and try again.")
			return
		}
		s.shell.Say("Paired with " + other.Name + ".")
		s.checkSoon()
	}()
}

// hostNearby is the asking device's part once its owner has said yes: open
// an ordinary pairing and give its link to the other device alone.
func hostNearby(ctx context.Context, n *app.Node, session *nearby.Session) (pairing.Device, error) {
	// No question to ask when the link is used: only the device whose
	// code this owner has just checked can read it.
	p, err := n.Pair(ctx, func(context.Context, pairing.Device) bool { return true })
	if err != nil {
		return pairing.Device{}, err
	}
	if err := session.SendLink(p.Link); err != nil {
		over, cancel := context.WithCancel(ctx)
		cancel()
		p.Wait(over) // closes the pairing
		return pairing.Device{}, err
	}
	other, err := p.Wait(ctx)
	if err == nil {
		session.Finish()
	}
	return other, err
}

// joinNearby is the asked device's part once its owner has said yes: wait
// for the link and use it.
func joinNearby(ctx context.Context, n *app.Node, session *nearby.Session) (pairing.Device, error) {
	link, err := session.Link(ctx)
	if err != nil {
		return pairing.Device{}, err
	}
	// The session has done its work, and the other device leaving the
	// meeting point now must not look like a failure: what is left goes
	// through the tunnel.
	session.Finish()
	ctx, cancel := context.WithTimeout(ctx, joinWait)
	defer cancel()
	return n.Join(ctx, link)
}
