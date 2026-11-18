// Package gui is everything the window shows and does, with no window in
// it: it drives a Node and turns what happens into a State for the window
// to draw. The window itself (cmd/yonder) is a Shell.
package gui

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"rsc.io/qr"

	"yonder/internal/address"
	"yonder/internal/app"
	"yonder/internal/firewall"
	"yonder/internal/localsend"
	"yonder/internal/nearby"
	"yonder/internal/pairing"
	"yonder/internal/settings"
	"yonder/internal/withdrawn"
)

const (
	// tunnelStart is how long opening the tunnel may take.
	tunnelStart = 30 * time.Second
	// presenceEvery is how often devices are asked whether they are there.
	presenceEvery = 30 * time.Second
	// nearbyWait is how long a device on the local network gets to answer;
	// after nearbyMisses in a row it leaves the list.
	nearbyWait   = 3 * time.Second
	nearbyMisses = 2
	// pairedWait is how long a paired device gets to answer.
	pairedWait = 15 * time.Second
	// redrawEvery limits how often progress reaches the window.
	redrawEvery = 100 * time.Millisecond
	// relayEvery is how often a transfer through the tunnel checks its path.
	relayEvery = 3 * time.Second
	// closedWait is how long Options.Closed gets to answer.
	closedWait = 5 * time.Second
	// doneFor is how long a finished transfer stays on show.
	doneFor = 6 * time.Second
	// maxName is the longest device name, in characters.
	maxName = 40
)

// Shell is what the Service needs from the window around it.
type Shell interface {
	// Changed gives the window a new State to draw.
	Changed(State)
	// Say shows a short message that needs no answer.
	Say(message string)
	// Attention makes sure the window can be seen, for a question.
	Attention()
	// Notify tells the person something when they may not be looking at
	// the window.
	Notify(title, body string)
	// Autostart reports whether Yonder starts when the person logs in.
	Autostart() bool
	// SetAutostart turns starting at login on or off.
	SetAutostart(on bool) error
	// PickFiles asks which files to send; none means the person backed out.
	PickFiles() ([]string, error)
	// PickFolder asks for a folder, starting from one; empty means the
	// person backed out.
	PickFolder(from string) (string, error)
	// Open opens a file or folder with whatever the system uses for it.
	Open(path string) error
	// Browse opens a web address in the person's browser.
	Browse(url string) error
}

// State is everything the window draws.
type State struct {
	// Seq goes up with every State made, so the window can drop one that
	// arrives after a newer one.
	Seq  int    `json:"seq"`
	Name string `json:"name"`
	// Version is this Yonder's version, shown beside its name; the web
	// version has none.
	Version string `json:"version"`
	// Problem says why paired devices cannot reach this one, if they can't.
	Problem string `json:"problem"`
	// Blocked is the notice that something keeps devices on the network
	// out, until it is put away; then Problem says it.
	Blocked *Blocked `json:"blocked"`
	// Update is the notice that a newer version is out, until it is put
	// away, and UpdateCheck whether Yonder asks its site for one.
	Update      *Update `json:"update"`
	UpdateCheck bool    `json:"updateCheck"`
	// Withdrawn is the notice that this version has been withdrawn; it
	// cannot be put away. Its Version is the one to get in its place,
	// empty if none is out yet. See withdrawn.go.
	Withdrawn *Update `json:"withdrawn"`
	// Started is the notice that a newer Yonder was started while this
	// one runs, until it is put away; see started.go.
	Started *Update `json:"started"`
	SaveDir string  `json:"saveDir"`
	// Autostart is whether Yonder starts when the person logs in.
	Autostart bool `json:"autostart"`
	// Visible is whether this device shows itself to others on the same
	// network through the meeting point.
	Visible bool `json:"visible"`
	// Refreshing is whether a refresh the person asked for is still asking
	// devices whether they are there.
	Refreshing bool       `json:"refreshing"`
	Devices    []Device   `json:"devices"`
	Transfers  []Transfer `json:"transfers"`
	Offer      *Offer     `json:"offer"`
	Pairing    *Pairing   `json:"pairing"`
	Code       *Code      `json:"code"`
	Room       *Room      `json:"room"`
	Received   []Received `json:"received"`
	// Unread counts the files received since the list was last looked at.
	Unread int `json:"unread"`
}

// Device is one row of the device list.
type Device struct {
	// ID is the device's fingerprint, or for a Pairable device a name that
	// lasts as long as its connection to the meeting point.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Label is the owner's own name for a paired device, shown beside the
	// one it gave itself; "" for none.
	Label  string `json:"label"`
	Model  string `json:"model"`
	Paired bool   `json:"paired"`
	Nearby bool   `json:"nearby"`
	// Presence is "online", "offline" or, before the first answer,
	// "checking". A nearby device is always online.
	Presence string `json:"presence"`
	// Always is whether its files are saved without asking.
	Always bool `json:"always"`
	// Pairable is whether it is known only through the meeting point: on
	// the same network, but files can only be sent once it is paired.
	Pairable bool `json:"pairable"`
	// LocalSend is whether it is seen on the local network running
	// LocalSend, not Yonder.
	LocalSend bool `json:"localsend"`
	// Unencrypted is whether files go to it without TLS, as to a LocalSend
	// with its encryption switched off: readable on the way, and with
	// nothing to prove the device is the one it says.
	Unencrypted bool `json:"unencrypted"`
}

// Transfer is files on the move, in either direction.
type Transfer struct {
	ID      int    `json:"id"`
	Sending bool   `json:"sending"`
	Device  string `json:"device"`
	Files   int    `json:"files"`
	// File is the one on the move now.
	File  string `json:"file"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	// Rate is bytes a second, smoothed.
	Rate float64 `json:"rate"`
	// Phase is "waiting" (for the other side to accept), "moving", "done"
	// or "failed".
	Phase string `json:"phase"`
	// Note says how a finished transfer went.
	Note string `json:"note"`
	// Relayed is whether the tunnel is going through a relay, which is slow.
	Relayed bool `json:"relayed"`
	// Direct is whether the files go over a data channel, straight between
	// a browser and the other device.
	Direct bool `json:"direct"`
	// Connecting is whether a send still waits for its data channel to
	// form: the other side has not been asked yet.
	Connecting bool `json:"connecting"`
}

// Offer is another device asking to send files.
type Offer struct {
	ID   int    `json:"id"`
	From string `json:"from"`
	// Paired is whether From is a paired device, proven by its certificate.
	Paired bool        `json:"paired"`
	Files  []OfferFile `json:"files"`
}

// OfferFile is one file in an Offer.
type OfferFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Pairing is the "Pair a device" dialog while this device shows a link.
type Pairing struct {
	// Link is empty until the link is ready, which takes a few seconds.
	Link string `json:"link"`
	// QR is the link as a QR code: one string a row, "1" for a dark square.
	QR []string `json:"qr"`
	// Request is the name of a device that used the link and waits for a
	// yes, or empty.
	Request string `json:"request"`
	// Problem says why there is no link, if there isn't.
	Problem string `json:"problem"`
	// Expires is when the link stops working if nobody uses it; it is
	// set with Link.
	Expires time.Time `json:"expires"`
}

// Received is a file that arrived while the app has been running.
type Received struct {
	Name string    `json:"name"`
	Path string    `json:"path"`
	From string    `json:"from"`
	Size int64     `json:"size"`
	At   time.Time `json:"at"`
	// Runs is whether the system would run the file, not show it, if it
	// were opened; the window then says "Show" and opens its folder.
	Runs bool `json:"runs"`
}

// Service is the running app behind the window. Its exported methods are
// what the window can ask for.
type Service struct {
	dir     string
	listen  string
	offline bool
	shell   Shell

	// drawing keeps States reaching the window in the order they were made.
	drawing sync.Mutex

	mu       sync.Mutex
	set      settings.Settings
	node     *app.Node
	stopNode context.CancelFunc
	problem  string
	online   map[string]bool // paired devices that have answered, or not
	missed   map[string]int  // nearby devices that did not answer
	check    chan struct{}   // asks for a presence round now
	// refreshing is set by Refresh and cleared by the presence round that
	// answers it.
	refreshing bool
	moving     []*transfer
	incoming   *transfer
	offer      *offer
	pairing    *pairingDialog
	meeting    string         // the meeting point's address, or empty
	kind       string         // what this device is to others there
	meet       *nearby.Client // nil when not shown to nearby devices
	stopMeet   context.CancelFunc
	code       *codeDialog
	room       *roomDialog // this device shows a code to type on another
	received   []Received
	unread     int
	lastID     int
	seq        int
	// autostart is whether Yonder starts at login, as the system has it.
	autostart bool

	// What is known about devices on the network being kept out; see
	// firewall.go. The first two are nil when it is not looked into.
	readFirewall func(port int) firewall.Status
	fixFirewall  func(port int) error
	fw           firewall.Status
	fwReadAt     time.Time
	fwKick       chan struct{}
	fwHidden     bool            // "Not now", until Yonder is next started
	fixing       bool            // the system's prompt is up
	unreached    bool            // a nearby Yonder could not connect to this one
	askedAt      time.Time       // when one last said
	silent       map[string]bool // nearby devices that did not say
	reachedIn    bool            // a device on the network has connected to this one
	localnet     bool            // the Mac is keeping Yonder off the network
	// What is known about a newer version; see update.go. With no
	// version of its own, Yonder does not ask.
	version, updateURL string
	says               string // the version told to other Yonders; see Options.Says
	started            string // a newer version that was started; see started.go
	quit               func()
	latest, latestPage string
	// The list of withdrawn versions that holds, and the key a list is
	// checked with; see withdrawn.go.
	list        withdrawn.List
	withdrawKey ed25519.PublicKey
	updateAt    time.Time // when the site was last asked
	updateKick  chan struct{}
	// addrs replaces lanAddrs in tests.
	addrs func(port int) []string
	// closed is Options.Closed.
	closed func(ctx context.Context) string
}

type transfer struct {
	Transfer
	// stop ends the transfer; cancelled records that the person asked.
	stop      context.CancelFunc
	cancelled bool
	// dropped is why Yonder itself stopped a transfer that was coming in.
	dropped string
	// way is how the files travel, if a browser is at one end.
	way       app.Way
	drawn     time.Time
	ratedAt   time.Time
	ratedDone int64
	// ease is a send's count, which showSending shows steadily.
	ease eased
}

type offer struct {
	Offer
	device string // the paired sender's fingerprint, if Paired
	answer chan bool
}

type pairingDialog struct {
	Pairing
	stop   context.CancelFunc
	answer chan bool
}

// Options is what Start needs.
type Options struct {
	// Dir holds this device's settings, identity and paired devices.
	Dir string
	// Listen is the local network address to serve on, the LocalSend port
	// if empty.
	Listen string
	// Offline leaves out looking for devices and the tunnel, for tests.
	Offline bool
	// Meeting is the meeting point where devices on the same network find
	// each other ("wss://host/nearby"); empty for none. Kind is what this
	// device is to the others there, nearby.KindApp or nearby.KindBrowser.
	Meeting, Kind string
	// Closed, if set, is asked before each send and each offer: a message
	// in return means Yonder may not move files just now, and is what the
	// person is told. The web version asks its site, which closes for the
	// rest of a month once its server has carried the month's limit.
	Closed func(ctx context.Context) string
	// Firewall reads the system's firewall and FixFirewall opens it for a
	// port; package firewall's if nil, and with Offline neither is used
	// unless given.
	Firewall    func(port int) firewall.Status
	FixFirewall func(port int) error
	// Version is this Yonder's version. With one, Yonder asks now and
	// then whether a newer is out, at UpdateURL, the site's own file if
	// empty; with none (the web version, which is always the newest) it
	// does not.
	Version, UpdateURL string
	// Says is the version this Yonder tells other Yonders that it has;
	// Version if empty. The web version, which asks for no update, gives
	// its version here.
	Says string
	// WithdrawKey is the key a list of withdrawn versions is checked
	// with; the one in package withdrawn if nil.
	WithdrawKey ed25519.PublicKey
	// Quit, if set, quits the app; the window's "Quit Yonder" button
	// uses it. The web version has none.
	Quit func()
}

// Start runs Yonder behind a window.
func Start(o Options, shell Shell) (*Service, error) {
	set, err := settings.Load(o.Dir)
	if err != nil {
		return nil, err
	}
	s := &Service{dir: o.Dir, listen: o.Listen, offline: o.Offline, meeting: o.Meeting, kind: o.Kind, closed: o.Closed, shell: shell, set: set, check: make(chan struct{}, 1), autostart: shell.Autostart(), readFirewall: o.Firewall, fixFirewall: o.FixFirewall, fwKick: make(chan struct{}, 1), version: o.Version, updateURL: o.UpdateURL, quit: o.Quit, updateKick: make(chan struct{}, 1), withdrawKey: o.WithdrawKey}
	if s.says = o.Says; s.says == "" {
		s.says = s.version
	}
	if s.withdrawKey == nil {
		s.withdrawKey = withdrawn.Key()
	}
	if s.version != "" {
		s.restoreKept()
	}
	if s.updateURL == "" {
		s.updateURL = latestURL
	}
	if !o.Offline && o.Kind != nearby.KindBrowser {
		if s.readFirewall == nil {
			s.readFirewall = firewall.Check
		}
		if s.fixFirewall == nil {
			s.fixFirewall = firewall.Fix
		}
	}
	return s, s.startNode()
}

// startNode brings up a Node with the current settings. s.mu must not be
// held.
func (s *Service) startNode() error {
	s.mu.Lock()
	set := s.set
	s.mu.Unlock()
	n, err := app.Start(app.Config{Dir: s.dir, SaveDir: set.SaveDir, Name: set.Name, Listen: s.listen, Browser: s.kind == nearby.KindBrowser, Version: s.says})
	if err != nil {
		return err
	}
	n.Nearby.Added = func(localsend.Peer) { s.changed() }
	n.Updated = s.changed
	n.Announced = s.announced
	n.Receiver.Registered = func(peer localsend.Info, remote string) {
		s.cameIn(remote)
		n.Nearby.Seen(peer, remote)
	}
	n.Receiver.Accept = s.accept
	n.Receiver.Received = s.saved
	n.Receiver.Progress = func(p localsend.Progress) {
		s.mu.Lock()
		t := s.incoming
		s.mu.Unlock()
		if t != nil {
			s.progress(t, p)
		}
	}
	n.Receiver.Finished = s.receiveFinished
	n.Receiver.Quiet = s.receiveQuiet

	ctx, stop := context.WithCancel(context.Background())
	s.mu.Lock()
	s.node, s.stopNode = n, stop
	s.problem = ""
	s.online, s.missed = map[string]bool{}, map[string]int{}
	s.fw, s.fwReadAt, s.unreached, s.askedAt, s.reachedIn, s.silent = firewall.Status{}, time.Time{}, false, time.Time{}, false, map[string]bool{}
	s.mu.Unlock()
	s.meetNearby()
	go s.watchFirewall(ctx, n)
	if s.version != "" {
		go s.watchUpdates(ctx)
	}

	if s.offline {
		go s.presence(ctx, n)
		return nil
	}
	go n.Discover(ctx)
	go func() {
		tctx, cancel := context.WithTimeout(ctx, tunnelStart)
		err := n.OpenTunnel(tctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Println("opening the tunnel:", err)
			s.mu.Lock()
			s.problem = "Paired devices on other networks can't reach this one. Check the internet connection."
			s.mu.Unlock()
			s.changed()
		} else {
			go s.watchHome(ctx, n)
		}
		s.presence(ctx, n)
	}()
	return nil
}

// watchHome checks now and then that this device still answers through its
// home relay, until ctx ends; a home that stays silent is left for another
// (app.CheckHome). It waits while a transfer is under way, since moving
// opens the tunnel again.
func (s *Service) watchHome(ctx context.Context, n *app.Node) {
	tick := time.NewTicker(app.HomeEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
		s.mu.Lock()
		busy := false
		for _, t := range s.moving {
			busy = busy || t.Phase == "waiting" || t.Phase == "moving"
		}
		s.mu.Unlock()
		if busy {
			continue
		}
		if moved, _ := n.CheckHome(ctx); moved {
			// The next round tells the paired devices the new address.
			s.checkSoon()
		}
	}
}

// Close stops Yonder.
func (s *Service) Close() {
	s.mu.Lock()
	n, stop := s.node, s.stopNode
	room := s.room
	s.room = nil
	s.mu.Unlock()
	if room != nil {
		room.stop()
	}
	s.leaveNearby()
	stop()
	n.Close()
}

// restart applies changed settings, which a running Node cannot take.
func (s *Service) restart() error {
	s.StopPairing()
	s.Close()
	err := s.startNode()
	s.changed()
	return err
}

// State is what the window should be showing now.
func (s *Service) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	st := State{
		Seq:         s.seq,
		Name:        s.set.Name,
		Version:     s.version,
		Problem:     s.problem,
		SaveDir:     s.set.SaveDir,
		Autostart:   s.autostart,
		Visible:     !s.set.HideNearby,
		Update:      s.update(),
		Withdrawn:   s.withdrawnNotice(),
		Started:     s.startedNotice(),
		UpdateCheck: !s.set.NoUpdateCheck,
		Refreshing:  s.refreshing,
		Devices:     s.devices(),
		Transfers:   []Transfer{},
		Received:    append([]Received{}, s.received...),
		Unread:      s.unread,
	}
	for _, t := range s.moving {
		st.Transfers = append(st.Transfers, t.Transfer)
	}
	if s.offer != nil {
		o := s.offer.Offer
		st.Offer = &o
	}
	if s.pairing != nil {
		p := s.pairing.Pairing
		st.Pairing = &p
	}
	if s.code != nil {
		c := s.code.Code
		st.Code = &c
	}
	if s.room != nil {
		r := s.room.Room
		st.Room = &r
	}
	if b, line := s.blocked(); b != nil && !s.fwHidden {
		st.Blocked = b
	} else if b != nil && st.Problem == "" {
		st.Problem = line
	}
	return st
}

// changed sends the window the State as it is now.
func (s *Service) changed() {
	s.drawing.Lock()
	defer s.drawing.Unlock()
	s.shell.Changed(s.State())
}

// AppVersion is this Yonder's version, for a terminal command or a second
// copy that asks.
func (s *Service) AppVersion() string { return s.version }

// List is this device's name and its devices as the window lists them, for
// a caller that is not the window.
func (s *Service) List() (name string, devices []Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set.Name, s.devices()
}

// devices merges nearby and paired devices into one list, by name; a
// device that is both is listed once. s.mu must be held.
func (s *Service) devices() []Device {
	paired, _ := s.node.Paired()
	byID := rows(paired, s.node.Nearby.List(), s.online, s.set)
	for _, d := range s.meetDevices(byID) {
		byID[d.ID] = &d
	}
	return byName(byID)
}

// Devices is the list a Node alone can give: its paired and nearby
// devices merged by name, a device that is both listed once. online says
// which paired devices answer; one not in it shows as being checked.
func Devices(paired []pairing.Device, nearby []localsend.Peer, online map[string]bool, set settings.Settings) []Device {
	return byName(rows(paired, nearby, online, set))
}

// rows makes the rows of paired and nearby devices, by ID.
func rows(paired []pairing.Device, nearby []localsend.Peer, online map[string]bool, set settings.Settings) map[string]*Device {
	byID := map[string]*Device{}
	for _, d := range paired {
		presence := "checking"
		if on, known := online[d.Fingerprint]; known {
			presence = map[bool]string{true: "online", false: "offline"}[on]
		}
		byID[d.Fingerprint] = &Device{ID: d.Fingerprint, Name: d.Name, Label: set.Labels[d.Fingerprint], Model: app.Model, Paired: true, Presence: presence, Always: set.Always(d.Fingerprint)}
	}
	for _, p := range nearby {
		d := byID[p.Info.Fingerprint]
		if d == nil {
			d = &Device{ID: p.Info.Fingerprint}
			byID[d.ID] = d
		}
		d.Nearby, d.Presence = true, "online"
		d.Unencrypted = !p.Encrypted()
		if d.Paired {
			// Anyone on the network can announce a paired device's
			// fingerprint, so its row keeps the name from the pairing; the
			// device says a new name itself, through the tunnel.
			continue
		}
		// A nearby device has just told us its name, so that one is current.
		d.Name, d.Model = p.Info.Alias, p.Info.DeviceModel
		d.LocalSend = p.Info.DeviceModel != app.Model
	}
	return byID
}

// byName puts rows in the order the list has: by name, then by ID.
func byName(byID map[string]*Device) []Device {
	out := make([]Device, 0, len(byID))
	for _, d := range byID {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name); a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// presence asks every device whether it is there, now and then every
// presenceEvery or when asked, until ctx ends.
func (s *Service) presence(ctx context.Context, n *app.Node) {
	tick := time.NewTicker(presenceEvery)
	defer tick.Stop()
	for {
		s.presenceRound(ctx, n)
		select {
		case <-tick.C:
		case <-s.check:
		case <-ctx.Done():
			return
		}
	}
}

// presenceRound asks every device once. In the round that answers a
// Refresh, a nearby device that stays silent leaves the list at once.
func (s *Service) presenceRound(ctx context.Context, n *app.Node) {
	s.mu.Lock()
	refresh := s.refreshing
	s.mu.Unlock()
	if refresh {
		defer func() {
			if ctx.Err() != nil {
				// The Node is being replaced; the next one's first round answers.
				return
			}
			s.mu.Lock()
			s.refreshing = false
			s.mu.Unlock()
			s.changed()
		}()
	}
	nearby := map[string]bool{}
	var wg sync.WaitGroup
	for _, p := range n.Nearby.List() {
		nearby[p.Info.Fingerprint] = true
		wg.Go(func() {
			actx, cancel := context.WithTimeout(ctx, nearbyWait)
			defer cancel()
			there := n.NearbyOnline(actx, p)
			if ctx.Err() != nil {
				return
			}
			fp := p.Info.Fingerprint
			s.mu.Lock()
			if there {
				delete(s.missed, fp)
			} else {
				s.missed[fp]++
			}
			gone := s.missed[fp] >= nearbyMisses || !there && refresh
			if gone {
				delete(s.missed, fp)
			}
			s.mu.Unlock()
			if gone {
				n.Nearby.Forget(fp)
				s.changed()
			}
		})
	}
	paired, _ := n.Paired()
	for _, d := range paired {
		if nearby[d.Fingerprint] {
			continue
		}
		wg.Go(func() {
			actx, cancel := context.WithTimeout(ctx, pairedWait)
			defer cancel()
			on := n.Online(actx, d)
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			was, known := s.online[d.Fingerprint]
			s.online[d.Fingerprint] = on
			s.mu.Unlock()
			if !known || was != on {
				s.changed()
			}
		})
	}
	wg.Wait()
}

// checkSoon asks for a presence round without waiting for the next one.
func (s *Service) checkSoon() {
	select {
	case s.check <- struct{}{}:
	default:
	}
}

// Refresh looks again for devices now: this device announces itself on the
// local network, and every listed device is asked whether it is there.
func (s *Service) Refresh() {
	s.mu.Lock()
	if s.refreshing {
		s.mu.Unlock()
		return
	}
	s.refreshing = true
	// Paired devices show as being checked until each has answered.
	s.online = map[string]bool{}
	n, offline := s.node, s.offline
	s.mu.Unlock()
	s.changed()
	if !offline {
		localsend.Announce(n.Info)
	}
	s.checkSoon()
}

// PickAndSend asks which files to send to a device, then sends them.
func (s *Service) PickAndSend(id string) error {
	paths, err := s.shell.PickFiles()
	if err != nil || len(paths) == 0 {
		return err
	}
	return s.Send(id, paths)
}

// Send offers files to the device with the given ID and, if it accepts,
// sends them. It returns once the offer is on its way; the transfer shows
// in the State.
func (s *Service) Send(id string, paths []string) error {
	return s.send(id, paths, nil)
}

// SendWait is Send for a caller that waits for the end, as a terminal
// command does: it returns what the window says of the transfer once it is
// over, and whether the device took files. The transfer shows in the
// State as any other.
func (s *Service) SendWait(id string, paths []string) (note string, ok bool, err error) {
	type end struct {
		note string
		ok   bool
	}
	ended := make(chan end, 1)
	if err := s.send(id, paths, func(note string, ok bool) { ended <- end{note, ok} }); err != nil {
		return "", false, err
	}
	e := <-ended
	return e.note, e.ok, nil
}

// send is Send; done, if given, is told how the transfer ended.
func (s *Service) send(id string, paths []string, done func(note string, ok bool)) error {
	if len(paths) == 0 {
		if done != nil {
			done("", false)
		}
		return nil
	}
	if why := s.closedNow(context.Background()); why != "" {
		return errors.New(why)
	}
	s.mu.Lock()
	n := s.node
	var peer *localsend.Peer
	for _, p := range n.Nearby.List() {
		if p.Info.Fingerprint == id {
			peer = &p
		}
	}
	var device *pairing.Device
	paired, _ := n.Paired()
	for _, d := range paired {
		if d.Fingerprint == id {
			device = &d
		}
	}
	if peer == nil && device == nil {
		s.mu.Unlock()
		return errors.New("That device is no longer in the list.")
	}
	name := ""
	if peer != nil {
		name = peer.Info.Alias
	} else {
		name = device.Name
	}
	ctx, stop := context.WithCancel(context.Background())
	t := s.begin(Transfer{Sending: true, Device: name, Files: len(paths), Phase: "waiting"}, stop)
	s.mu.Unlock()
	s.changed()

	go func() {
		defer stop()
		ctx := localsend.WithProgress(ctx, func(p localsend.Progress) { s.progress(t, p) })
		go s.showSending(ctx, t)
		var sent int
		var err error
		if peer != nil {
			// The local network is the quicker way to a device that is
			// also paired.
			sent, err = n.SendNearby(ctx, *peer, paths)
		}
		// A paired device that takes no connection on the local network
		// (a firewall, or a network that routes oddly) still has the tunnel.
		tunnel := peer == nil || (device != nil && sent == 0 && notReached(err) && ctx.Err() == nil)
		if tunnel {
			go s.watchRelay(ctx, t, func(ctx context.Context) bool { return n.SendRelayed(ctx, *device) })
			sent, err = n.SendPaired(app.WithWay(ctx, func(way app.Way) { s.setWay(t, way) }), *device, paths)
			if errors.Is(err, app.ErrUnreachable) {
				s.mu.Lock()
				s.online[device.Fingerprint] = false
				s.mu.Unlock()
			}
		}
		note := sendNote(name, sent, len(paths), err)
		if err == nil && sent > 0 {
			note += s.wayWord(t)
		}
		switch {
		case tunnel || !notReached(err):
		case noRoute(err):
			s.setLocalnet(true)
			note = "Could not reach " + name + ". macOS is keeping Yonder off your network."
		case peer.Info.DeviceModel == app.Model:
			note = "Could not reach " + name + ". A firewall on it may be keeping this device out; Yonder there will say so."
		default:
			note = "Could not reach " + name + ". A firewall on it may be keeping this device out."
		}
		if device != nil && failed(err) {
			theirs, heard := n.PeerVersion(device.Fingerprint)
			note += olderNote(s.says, theirs, heard)
		}
		s.finish(t, note, err == nil && sent > 0)
		if done != nil {
			done(note, err == nil && sent > 0)
		}
	}()
	return nil
}

// closedNow asks Options.Closed, and gives it a few seconds to answer.
func (s *Service) closedNow(ctx context.Context) string {
	if s.closed == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, closedWait)
	defer cancel()
	return s.closed(ctx)
}

// sendNote says how a send went.
func sendNote(device string, sent, offered int, err error) string {
	switch {
	case errors.Is(err, localsend.ErrDeclined):
		return device + " declined."
	case errors.Is(err, localsend.ErrBusy):
		return device + " is busy with another transfer. Try again in a moment."
	case errors.Is(err, app.ErrUnreachable):
		return device + " is offline, or it has unpaired this device."
	case errors.Is(err, app.ErrNoDirectPath) && sent == 0:
		return "Could not send to " + device + ". The two networks would not connect directly, and Yonder does not pass files between apps through its server."
	case errors.Is(err, app.ErrNoChannel) && sent == 0:
		return "Could not send to " + device + ". The two devices could not connect directly, and Yonder does not pass files through its server. Try again."
	case errors.Is(err, app.ErrOldDevice) && sent == 0:
		return "Could not send to " + device + ". Its Yonder is older and cannot connect directly. Update Yonder there, or reload its page."
	case err != nil && sent > 0:
		return fmt.Sprintf("Sent %d of %s to %s, then it stopped: %v", sent, plural(offered, "file"), device, err)
	case err != nil:
		return fmt.Sprintf("Could not send to %s: %v", device, err)
	case sent == 0:
		return device + " took none of the files."
	case sent < offered:
		return fmt.Sprintf("Sent %d of %s to %s; it did not take the rest.", sent, plural(offered, "file"), device)
	}
	return fmt.Sprintf("Sent %s to %s.", plural(sent, "file"), device)
}

// failed reports whether err is a send that went wrong, and not one that
// the other person declined, or that either person stopped.
func failed(err error) bool {
	return err != nil && !errors.Is(err, localsend.ErrDeclined) && !errors.Is(err, localsend.ErrBusy) && !errors.Is(err, context.Canceled)
}

// olderNote is what the line of a failed send to a paired device ends
// with when that device has an older Yonder than this one, or one from
// before Yonders told each other their versions: updating it may be the
// cure. own is this Yonder's version; theirs and heard are as
// app.Node.PeerVersion gives them. It is empty when the device has said
// nothing in this run, when either is a development build, and when the
// device is not older.
func olderNote(own, theirs string, heard bool) string {
	if !heard || app.ToldVersion(own) == "dev" || theirs == "dev" {
		return ""
	}
	switch {
	case theirs == "":
		return " The other device has an older Yonder. Updating it may help."
	case newer(own, theirs):
		return " The other device has an older Yonder (" + theirs + "). Updating it may help."
	}
	return ""
}

// begin adds a transfer to the State. s.mu must be held.
func (s *Service) begin(view Transfer, stop context.CancelFunc) *transfer {
	s.lastID++
	view.ID = s.lastID
	t := &transfer{Transfer: view, stop: stop}
	s.moving = append(s.moving, t)
	return t
}

// progress records how far a transfer has got and, not too often, redraws.
func (s *Service) progress(t *transfer, p localsend.Progress) {
	now := time.Now()
	s.mu.Lock()
	if t.Phase != "waiting" && t.Phase != "moving" {
		s.mu.Unlock()
		return
	}
	started := t.Phase == "waiting"
	if started {
		t.ratedAt = now
	}
	t.Phase, t.File, t.Total = "moving", p.File, p.Total
	if t.Sending {
		// A send's count comes in bursts; showSending shows it.
		t.ease.count(now, p.Done)
		s.mu.Unlock()
		if started {
			s.changed()
		}
		return
	}
	t.Done = p.Done
	t.rate(now)
	draw := now.Sub(t.drawn) >= redrawEvery
	if draw {
		t.drawn = now
	}
	s.mu.Unlock()
	if draw {
		s.changed()
	}
}

// rate works out a transfer's speed from how far Done has moved, about
// twice a second. s.mu must be held.
func (t *transfer) rate(now time.Time) {
	dt := now.Sub(t.ratedAt)
	if dt < 500*time.Millisecond {
		return
	}
	rate := float64(t.Done-t.ratedDone) / dt.Seconds()
	if t.Rate > 0 {
		rate = 0.6*t.Rate + 0.4*rate
	}
	t.Rate, t.ratedAt, t.ratedDone = rate, now, t.Done
}

// showSending moves what is shown of a send towards its count (see
// ease.go), and redraws, until ctx ends.
func (s *Service) showSending(ctx context.Context, t *transfer) {
	tick := time.NewTicker(redrawEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
		now := time.Now()
		s.mu.Lock()
		moved := false
		if t.Phase == "moving" {
			done := t.ease.show(now, t.Done)
			moved = done != t.Done
			t.Done = done
			t.rate(now)
		}
		s.mu.Unlock()
		if moved {
			s.changed()
		}
	}
}

// setWay records how a transfer's files travel.
func (s *Service) setWay(t *transfer, way app.Way) {
	s.mu.Lock()
	t.way, t.Direct, t.Connecting = way, way == app.WayChannel, way == app.WayConnecting
	s.mu.Unlock()
	s.changed()
}

// wayWord is what the note of a transfer that went well ends with when a
// browser was at one end: the way its files travelled, in a word.
func (s *Service) wayWord(t *transfer) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch t.way {
	case app.WayChannel:
		return " Direct."
	case app.WayRelay:
		return " Relayed."
	}
	return ""
}

// watchRelay keeps a transfer's Relayed up to date until ctx ends. The path
// can start on a relay and go direct a few seconds later. relayed asks about
// the connection that carries the transfer's files, which is either
// device's.
func (s *Service) watchRelay(ctx context.Context, t *transfer, relayed func(context.Context) bool) {
	tick := time.NewTicker(relayEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
		s.mu.Lock()
		moving := t.Phase == "moving"
		s.mu.Unlock()
		if !moving {
			continue
		}
		actx, cancel := context.WithTimeout(ctx, relayEvery)
		now := relayed(actx)
		cancel()
		s.mu.Lock()
		changed := t.Phase == "moving" && t.Relayed != now
		if changed {
			t.Relayed = now
		}
		s.mu.Unlock()
		if changed {
			s.changed()
		}
	}
}

// finish ends a transfer: one the person cancelled just goes, one that
// worked stays for a moment, and one that failed stays until dismissed.
func (s *Service) finish(t *transfer, note string, ok bool) {
	s.mu.Lock()
	t.stop()
	if s.incoming == t {
		s.incoming = nil
	}
	if t.Sending {
		// The transfer is over, so the count is how far it got.
		t.Done = max(t.Done, t.ease.sent)
	}
	switch {
	case t.cancelled:
		s.remove(t)
	case ok:
		t.Phase, t.Note = "done", note
		time.AfterFunc(doneFor, func() { s.DismissTransfer(t.ID) })
	default:
		t.Phase, t.Note = "failed", note
	}
	s.mu.Unlock()
	s.changed()
}

// remove takes a transfer out of the State. s.mu must be held.
func (s *Service) remove(t *transfer) {
	for i, m := range s.moving {
		if m == t {
			s.moving = append(s.moving[:i], s.moving[i+1:]...)
			return
		}
	}
}

// find returns the transfer with the given ID, or nil. s.mu must be held.
func (s *Service) find(id int) *transfer {
	for _, t := range s.moving {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// CancelTransfer stops a transfer that is waiting or moving.
func (s *Service) CancelTransfer(id int) {
	s.mu.Lock()
	t := s.find(id)
	if t == nil || (t.Phase != "waiting" && t.Phase != "moving") {
		s.mu.Unlock()
		return
	}
	t.cancelled = true
	receiving := s.incoming == t
	n := s.node
	s.mu.Unlock()
	if receiving {
		n.Receiver.Cancel()
		s.finish(t, "", false) // in case the Receiver had already let it go
	} else {
		t.stop() // which makes the send return, and finish it
	}
}

// DismissTransfer clears a finished transfer from the window.
func (s *Service) DismissTransfer(id int) {
	s.mu.Lock()
	t := s.find(id)
	if t == nil || t.Phase == "waiting" || t.Phase == "moving" {
		s.mu.Unlock()
		return
	}
	s.remove(t)
	s.mu.Unlock()
	s.changed()
}

// accept is the Receiver's Accept: it asks the person, unless the offer is
// from a paired device they said never to ask about.
func (s *Service) accept(ctx context.Context, o localsend.Offer) []string {
	if why := s.closedNow(ctx); why != "" {
		s.shell.Say(why)
		return nil
	}
	s.mu.Lock()
	n := s.node
	s.mu.Unlock()
	// Files from another app never come through the relay.
	admitted, guard := n.AdmitOffer(ctx, o)
	if !admitted {
		return nil
	}
	s.mu.Lock()
	from, isPaired := o.Sender.Alias, false
	var device pairing.Device
	paired, _ := n.Paired()
	for _, d := range paired {
		// Only a certificate proves who is asking; the name and the
		// fingerprint in the offer are whatever the sender says.
		if o.CertFingerprint != "" && strings.EqualFold(d.Fingerprint, o.CertFingerprint) {
			from, isPaired, device = d.Name, true, d
		}
	}
	if strings.TrimSpace(from) == "" {
		from = "An unnamed device"
	}
	yes := isPaired && s.set.Always(device.Fingerprint)
	var ask *offer
	if !yes {
		s.lastID++
		ask = &offer{Offer: Offer{ID: s.lastID, From: from, Paired: isPaired}, device: device.Fingerprint, answer: make(chan bool, 1)}
		for _, f := range o.Files {
			ask.Files = append(ask.Files, OfferFile{Name: f.FileName, Size: f.Size})
		}
		sort.Slice(ask.Files, func(i, j int) bool { return ask.Files[i].Name < ask.Files[j].Name })
		s.offer = ask
	}
	s.mu.Unlock()

	if ask != nil {
		s.changed()
		s.shell.Attention()
		who := from
		if !isPaired {
			who += " (unverified device)"
		}
		s.notify(who+" wants to send "+plural(len(ask.Files), "file"), offerSummary(ask.Files))
		select {
		case yes = <-ask.answer:
		case <-ctx.Done(): // the sender gave up
		}
		s.mu.Lock()
		s.offer = nil
		s.mu.Unlock()
		if !yes {
			s.changed()
			return nil
		}
	}

	way := n.OfferWay(ctx, o)
	tctx, stop := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.incoming != nil {
		s.remove(s.incoming) // one that never finished; the Receiver has let it go
	}
	t := s.begin(Transfer{Device: from, Files: len(o.Files), Phase: "waiting", Direct: way == app.WayChannel}, stop)
	t.way = way
	s.incoming = t
	s.mu.Unlock()
	if isPaired && way != app.WayChannel {
		// Fetched files arrive on this device's own connection.
		go s.watchRelay(tctx, t, func(ctx context.Context) bool { return n.Relayed(ctx, device, o.Fetch != "") })
	}
	if guard != nil {
		go guard(tctx, func() {
			s.mu.Lock()
			t.dropped = "The transfer from " + from + " stopped. The two networks no longer connect directly, and Yonder does not pass files between apps through its server."
			s.mu.Unlock()
			n.Receiver.Cancel() // which finishes it, with that as the note
		})
	}
	s.changed()
	ids := make([]string, len(o.Files))
	for i, f := range o.Files {
		ids[i] = f.ID
	}
	return ids
}

// AnswerOffer accepts or declines the offer on show. always also stops
// asking about files from that device, if it is a paired one.
func (s *Service) AnswerOffer(id int, accept, always bool) error {
	s.mu.Lock()
	o := s.offer
	if o == nil || o.ID != id {
		s.mu.Unlock()
		return nil
	}
	var err error
	if accept && always && o.Paired {
		s.set.SetAlways(o.device, true)
		err = s.set.Save(s.dir)
	}
	s.mu.Unlock()
	select {
	case o.answer <- accept:
	default:
	}
	return err
}

// saved is the Receiver's Received.
func (s *Service) saved(path string, f localsend.FileMeta) {
	s.mu.Lock()
	from := ""
	if s.incoming != nil {
		from = s.incoming.Device
	}
	s.received = append([]Received{{Name: filepath.Base(path), Path: path, From: from, Size: f.Size, At: time.Now(), Runs: runsWhenOpened(path)}}, s.received...)
	s.unread++
	s.mu.Unlock()
	s.changed()
}

// receiveQuiet is the Receiver's Quiet: it gives the note for a transfer
// stopped because the sender went quiet.
func (s *Service) receiveQuiet() {
	s.mu.Lock()
	if t := s.incoming; t != nil && t.dropped == "" {
		t.dropped = "The transfer from " + t.Device + " stopped: nothing arrived for a minute. Ask them to send it again."
	}
	s.mu.Unlock()
}

// receiveFinished is the Receiver's Finished.
func (s *Service) receiveFinished(complete bool) {
	s.mu.Lock()
	t := s.incoming
	s.mu.Unlock()
	if t == nil {
		return
	}
	note := fmt.Sprintf("Received %s from %s.", plural(t.Files, "file"), t.Device) + s.wayWord(t)
	if !complete {
		note = fmt.Sprintf("The transfer from %s stopped before every file arrived.", t.Device)
	}
	s.mu.Lock()
	if t.dropped != "" {
		note = t.dropped
	}
	s.mu.Unlock()
	s.finish(t, note, complete)
}

// SeenReceived marks the received files as looked at.
func (s *Service) SeenReceived() {
	s.mu.Lock()
	seen := s.unread > 0
	s.unread = 0
	s.mu.Unlock()
	if seen {
		s.changed()
	}
}

// OpenReceived opens a received file, by its place in the list. A file the
// system would run is not opened: its folder is.
func (s *Service) OpenReceived(i int) error {
	s.mu.Lock()
	if i < 0 || i >= len(s.received) {
		s.mu.Unlock()
		return nil
	}
	path := s.received[i].Path
	s.mu.Unlock()
	if runsWhenOpened(path) {
		return s.shell.Open(filepath.Dir(path))
	}
	return s.shell.Open(path)
}

// OpenSaveDir opens the folder received files go to.
func (s *Service) OpenSaveDir() error {
	s.mu.Lock()
	dir := s.set.SaveDir
	s.mu.Unlock()
	return s.shell.Open(dir)
}

// OpenDonate opens the website's page about donating.
func (s *Service) OpenDonate() error { return s.shell.Browse(address.Website + "donate/") }

// StartPairing opens a pairing and shows its link. The link takes a few
// seconds to appear in the State.
func (s *Service) StartPairing() {
	ctx, stop := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.pairing != nil {
		s.mu.Unlock()
		stop()
		return
	}
	dialog := &pairingDialog{stop: stop, answer: make(chan bool, 1)}
	s.pairing = dialog
	n := s.node
	s.mu.Unlock()
	s.changed()

	// update changes the dialog, unless it has been closed meanwhile.
	update := func(f func(*Pairing)) {
		s.mu.Lock()
		if s.pairing == dialog {
			f(&dialog.Pairing)
		}
		s.mu.Unlock()
		s.changed()
	}
	go func() {
		defer stop()
		p, err := n.Pair(ctx, func(ctx context.Context, joiner pairing.Device) bool {
			update(func(v *Pairing) { v.Request = joiner.Name })
			s.shell.Attention()
			select {
			case yes := <-dialog.answer:
				if !yes {
					update(func(v *Pairing) { v.Request = "" })
				}
				return yes
			case <-ctx.Done():
				return false
			}
		})
		if err != nil {
			if ctx.Err() == nil {
				update(func(v *Pairing) {
					v.Problem = "Could not make a pairing link. Pairing needs the internet; check the connection and try again."
				})
			}
			return
		}
		update(func(v *Pairing) { v.Link, v.QR, v.Expires = p.Link, qrRows(p.Link), p.Expires() })
		joiner, err := p.Wait(ctx)
		if err != nil {
			if ctx.Err() == nil { // not closed by the person: the link no longer works
				update(func(v *Pairing) {
					*v = Pairing{Problem: "This link has ended. Close this and open it again for a new one."}
					if errors.Is(err, pairing.ErrPairedUnderAnotherKey) {
						v.Problem = "That device is already paired under another key. Unpair it on both devices, then pair again."
					}
				})
			}
			return
		}
		s.mu.Lock()
		if s.pairing == dialog {
			s.pairing = nil
		}
		s.mu.Unlock()
		s.changed()
		s.shell.Say("Paired with " + joiner.Name + ".")
		s.checkSoon()
	}()
}

// qrRows draws text as a QR code, one string a row with "1" for dark, or
// nothing if the text is too long for one.
func qrRows(text string) []string {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return nil
	}
	rows := make([]string, code.Size)
	for y := range rows {
		row := make([]byte, code.Size)
		for x := range row {
			row[x] = '0'
			if code.Black(x, y) {
				row[x] = '1'
			}
		}
		rows[y] = string(row)
	}
	return rows
}

// AnswerPairing says yes or no to the device that used the link.
func (s *Service) AnswerPairing(yes bool) {
	s.mu.Lock()
	dialog := s.pairing
	s.mu.Unlock()
	if dialog == nil {
		return
	}
	select {
	case dialog.answer <- yes:
	default:
	}
}

// StopPairing closes the pairing dialog; its link stops working.
func (s *Service) StopPairing() {
	s.mu.Lock()
	dialog := s.pairing
	s.pairing = nil
	s.mu.Unlock()
	if dialog != nil {
		dialog.stop()
		s.changed()
	}
}

// Join pairs with the device that made link, and returns its name. It
// waits for that device's owner to say yes.
func (s *Service) Join(link string) (string, error) {
	link = strings.TrimSpace(link)
	if _, err := pairing.ParseLink(link); err != nil {
		return "", errors.New("That is not a Yonder pairing link.")
	}
	s.mu.Lock()
	n := s.node
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	host, err := n.Join(ctx, link)
	switch {
	case errors.Is(err, pairing.ErrDeclined):
		return "", errors.New("The other device refused: its owner said no, or the link was already used or has ended. Make a new link.")
	case errors.Is(err, pairing.ErrPairedUnderAnotherKey):
		return "", errors.New("That device is already paired under another key. Unpair it on both devices, then pair again.")
	case errors.Is(err, app.ErrTunnelDown):
		return "", errors.New("Could not pair: this device is not connected to Yonder's relay yet. Check its internet connection and try again.")
	case errors.Is(err, app.ErrOtherRelay):
		return "", errors.New("Could not pair: that link does not use Yonder's relay. Make a new link on the other device.")
	case errors.Is(err, app.ErrNotReached):
		return "", errors.New("Could not pair: the other device was not reached. Check both devices are online and the other one still shows the link.")
	case errors.Is(err, app.ErrNoAnswer):
		return "", errors.New("Could not pair: the other device was reached, but no answer came. Its owner has to say yes there; make a new link and try again.")
	case errors.Is(err, pairing.ErrHostNotStored):
		return "", errors.New("Could not pair: the other device could not save the pairing.")
	case errors.Is(err, app.ErrNotStored):
		return "", errors.New("Could not pair: this device could not save the pairing. Unpair this device on the other one, then try again.")
	case err != nil:
		return "", errors.New("Could not pair. The link works once and only while the other device shows it; check both devices are online and make a new link.")
	}
	s.changed()
	s.checkSoon()
	return host.Name, nil
}

// Unpair forgets a paired device and stops letting it in.
func (s *Service) Unpair(id string) error {
	s.mu.Lock()
	n := s.node
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), tunnelStart)
	defer cancel()
	err := n.Unpair(ctx, id)
	s.mu.Lock()
	delete(s.online, id)
	s.set.SetAlways(id, false)
	s.set.SetLabel(id, "")
	if serr := s.set.Save(s.dir); err == nil {
		err = serr
	}
	s.mu.Unlock()
	s.changed()
	return err
}

// SetAlways turns asking before saving off or back on for a paired device.
func (s *Service) SetAlways(id string, always bool) error {
	s.mu.Lock()
	s.set.SetAlways(id, always)
	err := s.set.Save(s.dir)
	s.mu.Unlock()
	s.changed()
	return err
}

// SetLabel gives a paired device the owner's own name, which stays on this
// device; "" takes it away.
func (s *Service) SetLabel(id, label string) error {
	label, err := tidyName(label)
	if err != nil {
		return err
	}
	s.mu.Lock()
	n := s.node
	s.mu.Unlock()
	paired, _ := n.Paired()
	if !slices.ContainsFunc(paired, func(d pairing.Device) bool { return d.Fingerprint == id }) {
		return errors.New("That device is not paired.")
	}
	s.mu.Lock()
	s.set.SetLabel(id, label)
	err = s.set.Save(s.dir)
	s.mu.Unlock()
	s.changed()
	return err
}

// notify shows a notification. Every one goes through here: the text may
// hold what another device sent, and some notification programs show markup.
func (s *Service) notify(title, body string) {
	s.shell.Notify(noticeText(title, noticeTitleMax), noticeText(body, noticeBodyMax))
}

const (
	noticeTitleMax = 100 // characters
	noticeBodyMax  = 200
)

// noticeText is text from outside made fit for a notification: without
// "<", ">" and control characters, and cut to max characters.
func noticeText(text string, max int) string {
	text = strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
	if r := []rune(text); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return text
}

// offerSummary names what is offered in one line: "a.jpg", or "a.jpg and
// 2 more".
func offerSummary(files []OfferFile) string {
	if len(files) == 0 {
		return ""
	}
	if len(files) == 1 {
		return files[0].Name
	}
	return fmt.Sprintf("%s and %d more", files[0].Name, len(files)-1)
}

// SetAutostart turns starting when the person logs in on or off.
func (s *Service) SetAutostart(on bool) error {
	err := s.shell.SetAutostart(on)
	now := s.shell.Autostart()
	s.mu.Lock()
	s.autostart = now
	s.mu.Unlock()
	s.changed()
	return err
}

// SetName renames this device. A paired device shows the new name after
// it next asks this one whether it is there (see app.Node's ask), and a
// device on the same network as soon as it hears the announcement.
func (s *Service) SetName(name string) error {
	name, err := tidyName(name)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("The device needs a name.")
	}
	s.mu.Lock()
	if name == s.set.Name {
		s.mu.Unlock()
		return nil
	}
	s.set.Name = name
	err = s.set.Save(s.dir)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.restart()
}

// tidyName is a name as typed with its spaces put right, or why it will
// not do.
func tidyName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if len([]rune(name)) > maxName {
		return "", fmt.Errorf("That name is too long; keep it to %d characters.", maxName)
	}
	return name, nil
}

// ChooseSaveDir asks for the folder received files should go to.
func (s *Service) ChooseSaveDir() error {
	s.mu.Lock()
	from := s.set.SaveDir
	s.mu.Unlock()
	dir, err := s.shell.PickFolder(from)
	if err != nil || dir == "" || dir == from {
		return err
	}
	s.mu.Lock()
	s.set.SaveDir = dir
	err = s.set.Save(s.dir)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.restart()
}

// plural writes a count with its noun: "1 file", "2 files".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
