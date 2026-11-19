package app

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"

	"yonder/internal/ownroute"
)

// tiedWorld is the interfaces that a tied under test sees, set by hand,
// with a record of what it opened and which listeners have ended.
type tiedWorld struct {
	mu     sync.Mutex
	ifaces []ownroute.Interface
	refuse string   // the interface whose listener cannot be opened
	opened []string // each interface a listener was opened for, in order
	ended  int      // listeners whose serve has returned
}

func (w *tiedWorld) set(refuse string, ifaces ...ownroute.Interface) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refuse, w.ifaces = refuse, ifaces
}

func (w *tiedWorld) seen() (opened []string, ended int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.opened), w.ended
}

func (w *tiedWorld) tied() *tied {
	return &tied{
		port: 53317,
		list: func() []ownroute.Interface {
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.ifaces
		},
		open: func(name string, port int) (net.Listener, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			if name == w.refuse {
				return nil, errors.New("refused")
			}
			w.opened = append(w.opened, name)
			return net.Listen("tcp", "127.0.0.1:0")
		},
		serve: func(ln net.Listener) {
			for {
				c, err := ln.Accept()
				if err != nil {
					w.mu.Lock()
					w.ended++
					w.mu.Unlock()
					return
				}
				c.Close()
			}
		},
	}
}

// expect checks which interfaces have a listener, what was opened so far
// and, once the goroutines have noticed, how many listeners have ended.
func (w *tiedWorld) expect(t *testing.T, td *tied, step string, have, opened []string, ended int) {
	t.Helper()
	got := td.names()
	slices.Sort(got)
	if !slices.Equal(got, have) {
		t.Fatalf("%s: listeners for %v, want %v", step, got, have)
	}
	if got, _ := w.seen(); !slices.Equal(got, opened) {
		t.Fatalf("%s: opened %v, want %v", step, got, opened)
	}
	until(t, step+": the closed listeners to end", func() bool { _, n := w.seen(); return n >= ended })
	if _, n := w.seen(); n != ended {
		t.Fatalf("%s: %d listeners ended, want %d", step, n, ended)
	}
}

// The listeners follow the interfaces: one is opened for a new interface,
// the one of an interface that is gone is closed, and nothing is done for
// an interface that stays.
func TestTiedListenersFollowInterfaces(t *testing.T) {
	w := &tiedWorld{}
	td := w.tied()
	wlan, eth := ownroute.Interface{Name: "wlan0", Index: 2}, ownroute.Interface{Name: "eth0", Index: 3}

	w.set("", wlan)
	td.look()
	w.expect(t, td, "one interface", []string{"wlan0"}, []string{"wlan0"}, 0)

	td.look()
	w.expect(t, td, "nothing changed", []string{"wlan0"}, []string{"wlan0"}, 0)

	w.set("", wlan, eth)
	td.look()
	w.expect(t, td, "a cable is put in", []string{"eth0", "wlan0"}, []string{"wlan0", "eth0"}, 0)

	w.set("", eth)
	td.look()
	w.expect(t, td, "the Wi-Fi goes", []string{"eth0"}, []string{"wlan0", "eth0"}, 1)

	// The Wi-Fi comes back as a new interface with the old name, and the
	// system refuses its listener once.
	w.set("wlan0", eth, ownroute.Interface{Name: "wlan0", Index: 7})
	td.look()
	w.expect(t, td, "the listener is refused", []string{"eth0"}, []string{"wlan0", "eth0"}, 1)
	w.set("", eth, ownroute.Interface{Name: "wlan0", Index: 7})
	td.look()
	w.expect(t, td, "the Wi-Fi is back", []string{"eth0", "wlan0"}, []string{"wlan0", "eth0", "wlan0"}, 1)

	// The same name with a new index is a new interface: the listener
	// was tied to the old one.
	w.set("", eth, ownroute.Interface{Name: "wlan0", Index: 8})
	td.look()
	w.expect(t, td, "the Wi-Fi is made again", []string{"eth0", "wlan0"}, []string{"wlan0", "eth0", "wlan0", "wlan0"}, 2)

	w.set("")
	td.look()
	w.expect(t, td, "no network", nil, []string{"wlan0", "eth0", "wlan0", "wlan0"}, 4)
}

// run looks at each kick, and closes every listener when it ends.
func TestTiedListenersRun(t *testing.T) {
	w := &tiedWorld{}
	td := w.tied()
	w.set("", ownroute.Interface{Name: "wlan0", Index: 2})
	ctx, stop := context.WithCancel(context.Background())
	kick, done := make(chan struct{}), make(chan struct{})
	go func() { td.run(ctx, kick); close(done) }()
	until(t, "the first look", func() bool { return len(td.names()) == 1 })

	w.set("", ownroute.Interface{Name: "wlan0", Index: 2}, ownroute.Interface{Name: "eth0", Index: 3})
	kick <- struct{}{}
	until(t, "the look after the kick", func() bool { return len(td.names()) == 2 })

	stop()
	<-done
	w.expect(t, td, "after the end", nil, []string{"wlan0", "eth0"}, 2)
}

// A Node that listens at one address has no tied listeners.
func TestFixedAddressGetsNoTiedListeners(t *testing.T) {
	if n := newNode(t, "Quiet Harbour"); n.tied != nil {
		t.Error("a Node at 127.0.0.1 has tied listeners")
	}
}
