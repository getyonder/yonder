package gui

import (
	"context"
	"errors"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"yonder/internal/app"
	"yonder/internal/firewall"
	"yonder/internal/localsend"
	"yonder/internal/nearby"
)

// The notice that something on this computer keeps devices on its network
// out. Three things can raise it: the system's firewall as far as it can
// be read (package firewall), another Yonder on the network saying it
// cannot connect to this one (the catch-all, through the meeting point),
// and on the Mac the network refusing what Yonder sends, which is how a
// denied Local Network permission shows.
// When another Yonder cannot connect and the system is seen to send the
// local network's traffic out by another interface (firewall.Status,
// Diverted), the notice names that cause and not the firewall.

const (
	// firewallEvery is how often the firewall is looked at while all
	// seems well, and firewallSoon while the notice is up, so that it
	// goes soon after the cause does.
	firewallEvery = 10 * time.Minute
	firewallSoon  = 30 * time.Second
	// reachWait is how long a nearby Yonder gets to say whether it can
	// connect to this one. An older Yonder never answers.
	reachWait = 15 * time.Second
)

// Blocked is the notice at the top of Devices.
type Blocked struct {
	Title string `json:"title"`
	Text  string `json:"text"`
	// Button is what the button that puts it right says; empty when
	// Yonder can do nothing about the cause.
	Button string `json:"button"`
}

// blocked is the notice to show, with the short line that stands for it
// once it has been put away; nil if nothing is known to be wrong. s.mu
// must be held.
func (s *Service) blocked() (*Blocked, string) {
	switch {
	case s.localnet:
		return &Blocked{
			Title:  "macOS is keeping Yonder off your network",
			Text:   "Yonder can't find devices on your network or send to them. Paired devices are not affected. In System Settings, go to Privacy & Security, then Local Network, and turn on Yonder.",
			Button: "Open Settings…",
		}, "macOS is keeping Yonder off your network"
	case s.unreached && s.fw.Diverted && !s.fw.Blocking:
		return &Blocked{
			Title: "A VPN on this computer is taking your network's traffic",
			Text:  "Another Yonder on your network could not reach this one, because this computer sends its answers into a VPN, not onto the network. Paired devices are not affected.",
		}, "A VPN is taking your network's traffic"
	case s.fw.Blocking, s.unreached && s.fw.On:
		b := &Blocked{
			Title:  "This computer's firewall is keeping other devices out",
			Text:   "Devices on your network can't find Yonder here or send it files. Paired devices still can.",
			Button: "Let them in…",
		}
		if firewall.Manual {
			b.Text += " In System Settings, go to Network, then Firewall, then Options, and allow Yonder."
			b.Button = "Open Settings…"
		}
		return b, "Firewall is keeping other devices out"
	case s.unreached:
		return &Blocked{
			Title: "Something is keeping other devices out",
			Text:  "Another Yonder on your network could not reach this one. Check this computer's firewall, and that the network lets its devices talk to each other; guest Wi-Fi often doesn't. Paired devices are not affected.",
		}, "Something is keeping other devices out"
	}
	return nil, ""
}

// watchFirewall keeps what blocked says current until ctx ends.
func (s *Service) watchFirewall(ctx context.Context, n *app.Node) {
	for {
		s.checkFirewall(ctx, n)
		s.mu.Lock()
		b, _ := s.blocked()
		s.mu.Unlock()
		wait := firewallEvery
		if b != nil {
			wait = firewallSoon
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-s.fwKick:
		}
	}
}

// kickFirewall has watchFirewall look again now: a Yonder that could test
// this one may have arrived.
func (s *Service) kickFirewall() {
	select {
	case s.fwKick <- struct{}{}:
	default:
	}
}

// checkFirewall reads the system's firewall, unless that was done a moment
// ago, has a nearby Yonder test this one if that could tell more, and
// redraws if the notice changed. s.mu must not be held.
func (s *Service) checkFirewall(ctx context.Context, n *app.Node) {
	s.mu.Lock()
	_, before := s.blocked()
	read := s.readFirewall
	if time.Since(s.fwReadAt) < firewallSoon {
		read = nil
	}
	s.mu.Unlock()
	if read != nil {
		fw := read(n.Info.Port)
		s.mu.Lock()
		s.fw, s.fwReadAt = fw, time.Now()
		s.mu.Unlock()
	}
	s.askNearby(ctx, n)
	s.mu.Lock()
	_, after := s.blocked()
	s.mu.Unlock()
	if after != before {
		s.changed()
	}
}

// askNearby asks a Yonder at the meeting point to connect to this one over
// the local network, which finds what cannot be read: a firewall Yonder
// does not know, or rules kept from it. Only a plain "could not" raises
// the notice. s.mu must not be held.
func (s *Service) askNearby(ctx context.Context, n *app.Node) {
	s.mu.Lock()
	meet := s.meet
	if s.reachedIn {
		s.unreached = false
	}
	skip := s.readFirewall == nil || meet == nil || s.kind != nearby.KindApp || s.fw.Blocking || s.reachedIn || time.Since(s.askedAt) < firewallSoon
	addrs := s.addrs
	s.mu.Unlock()
	if skip {
		return
	}
	if addrs == nil {
		addrs = s.lanAddrs
	}
	mine := addrs(n.Info.Port)
	if len(mine) == 0 {
		return
	}
	for _, p := range meet.Peers() {
		s.mu.Lock()
		silent := s.silent[p.ID]
		s.mu.Unlock()
		if p.Kind != nearby.KindApp || silent {
			continue
		}
		actx, cancel := context.WithTimeout(ctx, reachWait)
		ok, err := meet.AskReach(actx, p.ID, mine)
		cancel()
		s.mu.Lock()
		if err != nil {
			s.silent[p.ID] = true
			s.mu.Unlock()
			continue
		}
		s.unreached, s.askedAt = !ok, time.Now()
		s.mu.Unlock()
		return
	}
}

// lanAddrs lists where a device on the local network would connect to
// this one.
func (s *Service) lanAddrs(port int) []string {
	p := strconv.Itoa(port)
	if host, _, err := net.SplitHostPort(s.listen); err == nil {
		if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
			return []string{net.JoinHostPort(host, p)}
		}
	}
	var out []string
	for _, ipnet := range lanNets() {
		if len(out) < 4 {
			out = append(out, net.JoinHostPort(ipnet.IP.String(), p))
		}
	}
	return out
}

// lanNets lists this computer's addresses on local networks, each with
// its network.
func lanNets() []*net.IPNet {
	var out []*net.IPNet
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil && ipnet.IP.IsPrivate() {
				out = append(out, ipnet)
			}
		}
	}
	return out
}

// cameIn notes that a device introduced itself to this one. If it did so
// from the local network, nothing is keeping devices out.
func (s *Service) cameIn(remote string) {
	ip := net.ParseIP(remote)
	if ip == nil {
		return
	}
	for _, ipnet := range lanNets() {
		if ipnet.Contains(ip) && !ipnet.IP.Equal(ip) {
			s.mu.Lock()
			first := !s.reachedIn
			s.reachedIn = true
			s.mu.Unlock()
			if first {
				s.kickFirewall()
			}
			return
		}
	}
}

// announced is told how each announcement on the local network went. On
// the Mac, "no route to host" on a network that is plainly there is what a
// denied Local Network permission looks like; there is no way to ask.
func (s *Service) announced(err error) {
	s.setLocalnet(noRoute(err))
}

// noRoute reports whether err is the Mac refusing the local network.
func noRoute(err error) bool {
	return runtime.GOOS == "darwin" && err != nil && strings.Contains(err.Error(), "no route to host")
}

func (s *Service) setLocalnet(denied bool) {
	s.mu.Lock()
	was := s.localnet
	s.localnet = denied
	s.mu.Unlock()
	if was != denied {
		s.changed()
	}
}

// notReached reports whether a send failed before it began, because no
// connection could be made: nothing answered, or what answered was not the
// device.
func notReached(err error) bool {
	return errors.Is(err, localsend.ErrNotReached)
}

// FixFirewall is the notice's button: it lets devices in, after the
// system's own prompt, or on the Mac opens the page of System Settings
// where the person can.
func (s *Service) FixFirewall() error {
	s.mu.Lock()
	n, fix, localnet, busy := s.node, s.fixFirewall, s.localnet, s.fixing
	s.fixing = true
	s.mu.Unlock()
	if busy {
		return nil
	}
	defer func() {
		s.mu.Lock()
		s.fixing = false
		s.mu.Unlock()
	}()
	if localnet {
		return firewall.OpenLocalNetwork()
	}
	if fix == nil {
		return errors.New("Yonder can't change this system's firewall.")
	}
	if err := fix(n.Info.Port); errors.Is(err, firewall.ErrCancelled) {
		return nil
	} else if err != nil {
		return err
	}
	if firewall.Manual {
		// The change is the person's to make; the notice goes once it shows.
		return nil
	}
	// Look again at once, and have a nearby Yonder try again.
	s.mu.Lock()
	s.fwReadAt, s.askedAt = time.Time{}, time.Time{}
	asked := s.unreached
	s.unreached = false
	s.mu.Unlock()
	s.checkFirewall(context.Background(), n)
	s.mu.Lock()
	b, _ := s.blocked()
	if b == nil && asked && s.askedAt.IsZero() {
		// Nobody answered this time; what was found before still stands.
		s.unreached = true
		b, _ = s.blocked()
	}
	offline := s.offline
	s.mu.Unlock()
	s.changed()
	if b != nil {
		s.shell.Say("Something is still keeping other devices out.")
		return nil
	}
	if !offline {
		// So that devices on the network list this one without waiting.
		localsend.Announce(n.Info)
	}
	s.shell.Say("The firewall now lets devices on your network reach Yonder.")
	return nil
}

// HideFirewall is "Not now": the notice goes until Yonder is next started,
// and the line under this device's name says what it said.
func (s *Service) HideFirewall() {
	s.mu.Lock()
	s.fwHidden = true
	s.mu.Unlock()
	s.changed()
}
