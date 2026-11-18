package pairing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"yonder/internal/address"
	"yonder/internal/tunnel"
)

// The exchange is one HTTP request over a tunnel address made just for it:
// the joining device posts its record, the host asks its owner, and answers
// with its own record. The address in the link is the one-time secret; it
// stops working when the host closes the pairing.

// A link is the address of Yonder's web version with the pairing address
// after the "#". A phone's camera opens it straight from the QR code, and
// the part after "#" is never sent to the web server, so the secret stays
// between the two devices. The apps read the address out of it themselves,
// whatever site the link names: only the part after linkMark counts.
const (
	linkPrefix = address.WebApp + linkMark
	linkMark   = "#pair="
)

// ErrDeclined means the host said no, or the pairing was already used.
var ErrDeclined = errors.New("the other device declined to pair")

// ErrHostNotStored means the host's owner said yes, but the host could not
// store the pairing.
var ErrHostNotStored = errors.New("the other device could not store the pairing")

// Link wraps a pairing address as something to paste or show as a QR code.
func Link(addr tunnel.Addr) string { return linkPrefix + string(addr) }

// ParseLink gets the pairing address back out of a link.
func ParseLink(link string) (tunnel.Addr, error) {
	_, addr, ok := strings.Cut(strings.TrimSpace(link), linkMark)
	if !ok || addr == "" {
		return "", errors.New("not a Yonder pairing link")
	}
	return tunnel.Addr(addr), nil
}

// Host answers a pairing request. Its owner is asked about one device at
// most, and only before Expires; serve it on a listener that is closed once
// Done.
type Host struct {
	// Self is this device's record, sent to the joiner once confirmed.
	Self Device
	// Confirm asks this device's owner whether to pair with the joiner.
	Confirm func(ctx context.Context, joiner Device) bool
	// Paired stores the joiner and lets it in, before the joiner is told
	// it worked. An error fails the pairing.
	Paired func(joiner Device) error
	// Ended, if set, is called once the owner has been asked, before the
	// joiner is told: with nil if the joiner is paired, ErrDeclined, or the
	// error from Paired.
	Ended func(joiner Device, err error)
	// Expires is when the pairing ends if nobody has asked; zero for never.
	Expires time.Time

	mu   sync.Mutex
	busy bool
	over bool
}

// Done reports whether the owner has been asked, after which Host refuses
// everything, whatever the answer was.
func (h *Host) Done() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.over
}

// Asking reports whether the owner is being asked now.
func (h *Host) Asking() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.busy
}

func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.URL.Path != "/pair" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	var joiner Device
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&joiner); err != nil || !joiner.valid() {
		http.Error(w, "invalid device", http.StatusBadRequest)
		return
	}
	joiner = joiner.cleaned()
	h.mu.Lock()
	if h.busy || h.over || (!h.Expires.IsZero() && time.Now().After(h.Expires)) {
		h.mu.Unlock()
		http.Error(w, "pairing is not open", http.StatusForbidden)
		return
	}
	h.busy = true
	h.mu.Unlock()

	err := ErrDeclined
	if h.Confirm(r.Context(), joiner) && r.Context().Err() == nil {
		err = h.Paired(joiner)
	}
	// One question for each link: a "no" ends it too, so a stranger cannot
	// ask again and again until the owner says yes by mistake.
	h.mu.Lock()
	h.busy = false
	h.over = true
	h.mu.Unlock()
	if h.Ended != nil {
		h.Ended(joiner, err)
	}

	switch {
	case errors.Is(err, ErrDeclined):
		http.Error(w, "declined", http.StatusForbidden)
	case errors.Is(err, ErrPairedUnderAnotherKey):
		http.Error(w, "already paired under another key", http.StatusConflict)
	case err != nil:
		http.Error(w, "could not save the pairing", http.StatusInternalServerError)
	default:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(h.Self)
	}
}

// Join asks the host behind dial to pair with self and returns the host's
// record. dial is the pairing tunnel's dialler. Waiting for the host's
// owner to confirm can take as long as ctx allows.
func Join(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error), self Device) (Device, error) {
	body, err := json.Marshal(self)
	if err != nil {
		return Device{}, err
	}
	client := &http.Client{Transport: &http.Transport{DialContext: dial}}
	defer client.CloseIdleConnections()
	// The host name is a placeholder; dial decides where this goes.
	req, err := http.NewRequestWithContext(ctx, "POST", "http://pair.yonder/pair", bytes.NewReader(body))
	if err != nil {
		return Device{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Device{}, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusForbidden:
		return Device{}, ErrDeclined
	case http.StatusConflict:
		return Device{}, ErrPairedUnderAnotherKey
	case http.StatusInternalServerError:
		return Device{}, ErrHostNotStored
	default:
		return Device{}, fmt.Errorf("pairing failed: HTTP %d", resp.StatusCode)
	}
	var host Device
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&host); err != nil || !host.valid() {
		return Device{}, errors.New("pairing failed: the other device sent an invalid record")
	}
	return host.cleaned(), nil
}
