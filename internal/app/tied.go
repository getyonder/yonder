package app

import (
	"context"
	"net"
	"sync"
	"time"

	"yonder/internal/ownroute"
)

// tiedEvery is how often the interfaces are looked at for one that came
// or went.
const tiedEvery = 10 * time.Second

// tied keeps one listener for each interface of a local network, at the
// port of the listener for every interface and beside it. Each is tied to
// its interface, so the answers to a connection that arrives there leave
// the same way, whatever the system's routes say (package ownroute). The
// listener for every interface stays for all else: the loopback and the
// interfaces that get no listener here.
type tied struct {
	port int
	// list gives the interfaces that get a listener, open makes the
	// listener of one, and serve answers on it until it is closed.
	list  func() []ownroute.Interface
	open  func(name string, port int) (net.Listener, error)
	serve func(net.Listener)

	mu  sync.Mutex
	lns map[tiedKey]net.Listener
}

// tiedKey names an interface. An interface that went and came back has
// its name and a new index, and a listener is tied to the index.
type tiedKey struct {
	name  string
	index int
}

// look closes the listener of each interface that is gone and opens one
// for each that is new. An interface whose listener could not be opened
// is tried again at the next look.
func (t *tied) look() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := map[tiedKey]bool{}
	for _, ifc := range t.list() {
		now[tiedKey{ifc.Name, ifc.Index}] = true
	}
	for k, ln := range t.lns {
		if !now[k] {
			ln.Close()
			delete(t.lns, k)
		}
	}
	for k := range now {
		if t.lns[k] != nil {
			continue
		}
		ln, err := t.open(k.name, t.port)
		if err != nil {
			continue
		}
		if t.lns == nil {
			t.lns = map[tiedKey]net.Listener{}
		}
		t.lns[k] = ln
		go t.serve(ln)
	}
}

// names lists the interfaces that have a listener now.
func (t *tied) names() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for k := range t.lns {
		out = append(out, k.name)
	}
	return out
}

// run looks now, then every tiedEvery and at each kick, until ctx ends;
// then it closes every listener.
func (t *tied) run(ctx context.Context, kick <-chan struct{}) {
	for {
		t.look()
		select {
		case <-ctx.Done():
			t.mu.Lock()
			for k, ln := range t.lns {
				ln.Close()
				delete(t.lns, k)
			}
			t.mu.Unlock()
			return
		case <-time.After(tiedEvery):
		case <-kick:
		}
	}
}

// NetworkChanged says that this device's network may have changed, so
// that the interfaces are looked at now and not at the next turn.
func (n *Node) NetworkChanged() {
	select {
	case n.tiedKick <- struct{}{}:
	default:
	}
}
