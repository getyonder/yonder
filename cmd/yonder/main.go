// Yonder sends files between devices, on the same network or not.
//
// Run with no command, it opens its window. It can also be driven from the
// terminal:
//
//	yonder                      open the window
//	yonder -hidden              the same, but start in the tray (as at login)
//	yonder receive              receive, asking before each offer
//	yonder -pair receive        the same, and invite one device to pair
//	yonder -join LINK receive   the same, and pair using another device's link
//	yonder devices              list paired devices and whether they are online
//	yonder devices -json        list every device, paired and nearby, for a program to read
//	yonder send DEVICE FILE...  send files to a paired or nearby device, by name
//	yonder send -id ID FILE...  the same, by the device's ID from `devices -json`
//	yonder unpair DEVICE        forget a paired device
//
// Only the window and `receive` keep the tunnel open, so pairing happens
// there. A yonder that is already running picks up an unpair made from the
// terminal when it is restarted.
//
// `devices` and `send` ask the Yonder that is already running, if one is
// (see package control), and its window shows the transfer. Only with none
// running do they start one of their own for the one command.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"yonder/internal/app"
	"yonder/internal/control"
	"yonder/internal/gui"
	"yonder/internal/localsend"
	"yonder/internal/nearby"
	"yonder/internal/pairing"
	"yonder/internal/settings"
)

const version = "0.1.43"

const (
	// findFor is how long `send` looks for a nearby device before giving up.
	findFor = 5 * time.Second
	// tunnelStart is how long opening the tunnel may take.
	tunnelStart = 30 * time.Second
	// pingFor is how long `devices` waits for a paired device to answer.
	pingFor = 12 * time.Second
	// lookFor is how long `devices -json`, with no Yonder running, gives
	// devices on the local network to answer.
	lookFor = 2 * time.Second
)

const usage = "usage: yonder [flags] [receive | devices [-json] [-app] | send DEVICE FILE... | send -id ID FILE... | unpair DEVICE]"

func main() {
	log.SetFlags(0)
	configDir, _ := os.UserConfigDir()
	var (
		dir       = flag.String("config", filepath.Join(configDir, "yonder"), "folder for this device's settings, identity and paired devices")
		saveDir   = flag.String("save", "", "folder for received files, instead of the one in the settings (terminal commands only)")
		name      = flag.String("name", "", "this device's name, instead of the one in the settings (terminal commands only)")
		port      = flag.Int("port", localsend.DefaultPort, "port to listen on; a free one is used if it is taken")
		acceptAll = flag.Bool("accept-all", false, "accept every offer and pairing without asking")
		pair      = flag.Bool("pair", false, "while receiving, invite one device to pair")
		join      = flag.String("join", "", "while receiving, pair using this link from another device")
		meeting   = flag.String("meeting", nearby.DefaultURL, "meeting point where devices on the same network find each other (the window only); empty for none")
		hidden    = flag.Bool(hiddenFlag, false, "start in the tray without showing the window, as at login")
	)
	flag.Parse()
	listen := fmt.Sprintf(":%d", *port)
	if flag.NArg() == 0 {
		if err := runWindow(*dir, listen, *meeting, *hidden); err != nil {
			log.Fatal(err)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// start brings up a Yonder for the one command. The name and save
	// folder are the window's settings, unless overridden.
	start := func() (*app.Node, settings.Settings) {
		set, err := settings.Load(*dir)
		if err != nil {
			log.Fatal(err)
		}
		if *name != "" {
			set.Name = *name
		}
		if *saveDir != "" {
			set.SaveDir = *saveDir
		}
		n, err := app.Start(app.Config{Dir: *dir, SaveDir: set.SaveDir, Name: set.Name, Listen: listen, Version: version})
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			if err := n.Discover(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "finding devices on the local network:", err)
			}
		}()
		return n, set
	}
	running := control.Path(*dir)

	var err error
	switch args := flag.Args(); {
	case args[0] == "receive" && len(args) == 1:
		n, _ := start()
		err = receive(ctx, n, *acceptAll, *pair, *join)
		n.Close()
	case args[0] == "devices":
		err = devices(ctx, running, start, args[1:])
	case args[0] == "send":
		err = send(ctx, running, start, args[1:])
	case args[0] == "unpair" && len(args) == 2:
		n, _ := start()
		err = unpair(ctx, n, args[1])
		n.Close()
	default:
		err = errors.New(usage)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// receive takes offers from nearby and paired devices until ctx ends.
func receive(ctx context.Context, n *app.Node, acceptAll, pair bool, join string) error {
	ask := terminal{acceptAll: acceptAll}
	n.Nearby.Added = func(p localsend.Peer) {
		fmt.Printf("found %s at %s\n", plain(p.Info.Alias), p.Host)
	}
	n.Receiver.Received = func(path string, _ localsend.FileMeta) {
		fmt.Println("saved", path)
	}
	n.Receiver.Accept = func(ctx context.Context, o localsend.Offer) []string {
		from := plain(o.Sender.Alias)
		if d, ok := n.PairedSender(ctx); ok {
			from = plain(d.Name) + " (paired)"
		}
		// Files from another app never come through the relay. Here a
		// transfer already under way is not watched, as the window's is.
		if ok, _ := n.AdmitOffer(ctx, o); !ok {
			fmt.Printf("%s offered files, but the two networks would not connect directly\n", from)
			return nil
		}
		fmt.Printf("%s wants to send %s:\n", from, plural(len(o.Files), "file"))
		for _, f := range o.Files {
			fmt.Printf("  %s (%d bytes)\n", plain(f.FileName), f.Size)
		}
		if !ask.yes("accept?") {
			return nil
		}
		ids := make([]string, len(o.Files))
		for i, f := range o.Files {
			ids[i] = f.ID
		}
		return ids
	}
	fmt.Printf("yonder %s: %q listening on port %d, saving to %s\n", version, n.Info.Alias, n.Info.Port, n.Receiver.SaveDir)

	tctx, cancel := context.WithTimeout(ctx, tunnelStart)
	err := n.OpenTunnel(tctx)
	cancel()
	if err != nil {
		if pair || join != "" {
			return fmt.Errorf("pairing needs the tunnel: %w", err)
		}
		fmt.Fprintln(os.Stderr, "paired devices on other networks cannot reach this one:", err)
	}

	if join != "" {
		host, err := n.Join(ctx, join)
		if err != nil {
			return err
		}
		fmt.Printf("paired with %s\n", host.Name)
	}
	if pair {
		p, err := n.Pair(ctx, func(_ context.Context, joiner pairing.Device) bool {
			return ask.yes(fmt.Sprintf("pair with %s?", joiner.Name))
		})
		if err != nil {
			return err
		}
		fmt.Printf("to pair, run this on the other device (the link is a secret until used):\n  yonder -join %s\n", p.Link)
		joiner, err := p.Wait(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("paired with %s\n", joiner.Name)
	}
	<-ctx.Done()
	return nil
}

// devices lists this device's devices: the running Yonder's list if one
// is running at the socket, otherwise what a Yonder started for the
// question finds. As text it is the paired devices and whether each
// answers; with -json it is a control.List of every device.
func devices(ctx context.Context, running string, start func() (*app.Node, settings.Settings), args []string) error {
	flags := flag.NewFlagSet("devices", flag.ContinueOnError)
	asJSON := flags.Bool("json", false, "print every device, paired and nearby, as JSON")
	onlyApp := flags.Bool("app", false, "ask only the Yonder that is running; start none")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New(usage)
	}
	list, err := control.Devices(ctx, running)
	switch {
	case errors.Is(err, control.ErrNotRunning) && *onlyApp && *asJSON:
		list = control.List{Version: control.Version, Devices: []gui.Device{}, App: version}
	case errors.Is(err, control.ErrNotRunning) && !*onlyApp:
		n, set := start()
		list = look(ctx, n, set)
		n.Close()
	case err != nil:
		return err
	}
	if *asJSON {
		out := json.NewEncoder(os.Stdout)
		out.SetIndent("", "  ")
		return out.Encode(list)
	}
	paired := 0
	for _, d := range list.Devices {
		if !d.Paired {
			continue
		}
		paired++
		state := map[string]string{"online": "online", "offline": "offline, or it has unpaired this device"}[d.Presence]
		if state == "" {
			state = "being checked"
		}
		fmt.Printf("%s: %s\n", d.Name, state)
	}
	if paired == 0 {
		fmt.Println("no paired devices")
	}
	return nil
}

// look is the list a Yonder started for the question can give: every
// paired device asked whether it answers, and the devices on the local
// network that said so within lookFor.
func look(ctx context.Context, n *app.Node, set settings.Settings) control.List {
	paired, _ := n.Paired()
	ctx, cancel := context.WithTimeout(ctx, pingFor)
	defer cancel()
	answers := make([]bool, len(paired))
	var wg sync.WaitGroup
	for i, d := range paired {
		wg.Go(func() { answers[i] = n.Online(ctx, d) })
	}
	wg.Go(func() {
		select {
		case <-time.After(lookFor):
		case <-ctx.Done():
		}
	})
	wg.Wait()
	online := map[string]bool{}
	for i, d := range paired {
		online[d.Fingerprint] = answers[i]
	}
	return control.List{Version: control.Version, Name: n.Info.Alias, Devices: gui.Devices(paired, n.Nearby.List(), online, set), App: version}
}

// send sends files to one device, named by -id or by the first argument.
// The running Yonder does it if one is running at the socket; otherwise a
// Yonder started for the command.
func send(ctx context.Context, running string, start func() (*app.Node, settings.Settings), args []string) error {
	flags := flag.NewFlagSet("send", flag.ContinueOnError)
	id := flags.String("id", "", "the device's ID, as `devices -json` gives it, in place of its name")
	if err := flags.Parse(args); err != nil {
		return errors.New(usage)
	}
	device, paths := "", flags.Args()
	if *id == "" && len(paths) > 0 {
		device, paths = paths[0], paths[1:]
	}
	if len(paths) == 0 {
		return errors.New(usage)
	}
	// The running Yonder has its own working folder.
	for i, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return err
		}
		if _, err := os.Stat(abs); err != nil {
			return err
		}
		paths[i] = abs
	}
	sent, err := control.SendFiles(ctx, running, control.Send{ID: *id, Device: device, Paths: paths})
	switch {
	case errors.Is(err, control.ErrNotRunning):
		n, _ := start()
		defer n.Close()
		return sendAlone(ctx, n, *id, device, paths)
	case err != nil:
		return err
	case !sent.OK:
		return errors.New(sent.Note)
	}
	fmt.Println(sent.Note)
	return nil
}

// sendAlone sends files from a Yonder started for the command: to a paired
// device through the tunnel, otherwise to one found on the local network.
// The device is the one with the ID or, with no ID, the one of that name.
func sendAlone(ctx context.Context, n *app.Node, id, device string, paths []string) error {
	called := fmt.Sprintf("is called %q", device)
	paired := func() (pairing.Device, bool) { return n.FindPaired(device) }
	nearby := func() (localsend.Peer, bool) { return n.Nearby.Find(device) }
	if id != "" {
		called = fmt.Sprintf("has the ID %q", id)
		paired = func() (pairing.Device, bool) {
			all, _ := n.Paired()
			for _, d := range all {
				if d.Fingerprint == id {
					return d, true
				}
			}
			return pairing.Device{}, false
		}
		nearby = func() (localsend.Peer, bool) {
			for _, p := range n.Nearby.List() {
				if p.Info.Fingerprint == id {
					return p, true
				}
			}
			return localsend.Peer{}, false
		}
	}
	var sent int
	var err error
	if d, ok := paired(); ok {
		fmt.Printf("offering %s to %s (paired), waiting for them to accept...\n", plural(len(paths), "file"), d.Name)
		sent, err = n.SendPaired(ctx, d, paths)
	} else if p, ok := findNearby(ctx, nearby); ok {
		fmt.Printf("offering %s to %s, waiting for them to accept...\n", plural(len(paths), "file"), p.Info.Alias)
		sent, err = n.SendNearby(ctx, p, paths)
	} else {
		var names []string
		for _, p := range n.Nearby.List() {
			names = append(names, p.Info.Alias)
		}
		if len(names) == 0 {
			return fmt.Errorf("no paired or nearby device %s, and no other devices are nearby", called)
		}
		return fmt.Errorf("no paired or nearby device %s; nearby I can see: %s", called, strings.Join(names, ", "))
	}
	if err != nil {
		return err
	}
	fmt.Printf("sent %d of %s\n", sent, plural(len(paths), "file"))
	return nil
}

// findNearby waits for a device to show up on the local network, for at
// most findFor; find looks for it among those seen so far.
func findNearby(ctx context.Context, find func() (localsend.Peer, bool)) (localsend.Peer, bool) {
	deadline := time.After(findFor)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if p, ok := find(); ok {
			return p, true
		}
		select {
		case <-tick.C:
		case <-deadline:
			return localsend.Peer{}, false
		case <-ctx.Done():
			return localsend.Peer{}, false
		}
	}
}

func unpair(ctx context.Context, n *app.Node, device string) error {
	d, ok := n.FindPaired(device)
	if !ok {
		return fmt.Errorf("no paired device is called %q", device)
	}
	if err := n.Unpair(ctx, d.Fingerprint); err != nil {
		return err
	}
	fmt.Printf("unpaired %s; if yonder is running on this device, restart it to stop letting %s in\n", d.Name, d.Name)
	return nil
}

// terminal asks yes/no questions of the person at the keyboard, one at a
// time.
type terminal struct {
	acceptAll bool
	mu        sync.Mutex
}

func (t *terminal) yes(question string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.acceptAll {
		fmt.Println(question, "yes (accept-all)")
		return true
	}
	fmt.Print(question, " [y/N] ")
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(answer), "y")
}

// plain removes what would act on the terminal, or hide the real name, if
// printed: control codes (escape sequences, carriage returns) and the
// invisible formatting characters that turn text around. A sender chooses
// these names.
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}

// plural writes a count with its noun: "1 file", "2 files".
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
