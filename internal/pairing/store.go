// Package pairing is how two devices on different networks come to trust
// each other: the record each keeps of the other, where those records and
// this device's tunnel keys live on disk, and the one-time exchange that
// creates them.
package pairing

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"yonder/internal/names"
	"yonder/internal/tunnel"
)

// Device is what one paired device knows about another.
type Device struct {
	Name string `json:"name"`
	// Fingerprint is the device's LocalSend certificate fingerprint, the
	// same on the local network and through the tunnel.
	Fingerprint string `json:"fingerprint"`
	// Addr is where to reach it through the tunnel. It is a secret.
	Addr tunnel.Addr `json:"addr"`
	// ClientID is the key it connects with, which we let in.
	ClientID tunnel.ClientID `json:"clientId"`
}

// cleaned is the record with its name cleaned: every name taken from
// another device goes through names.Clean.
func (d Device) cleaned() Device {
	d.Name = names.Clean(d.Name)
	return d
}

// valid reports whether a device record from the other side is usable.
func (d Device) valid() bool {
	return names.Clean(d.Name) != "" && d.Fingerprint != "" && d.Addr != "" && d.ClientID != ""
}

// Store keeps this device's tunnel keys and its paired devices in a folder,
// in files only the owner can read.
type Store struct {
	dir string
	mu  sync.Mutex
}

// Open returns the store in dir. Nothing is read or created yet.
func Open(dir string) *Store { return &Store{dir: dir} }

func (s *Store) keysPath() string    { return filepath.Join(s.dir, "tunnel.json") }
func (s *Store) devicesPath() string { return filepath.Join(s.dir, "devices.json") }
func (s *Store) relaysPath() string  { return filepath.Join(s.dir, "relays.json") }

// RelayList returns the last list of relays fetched, as it came, and when it
// was kept (see tunnel.LoadRelays).
func (s *Store) RelayList() (data []byte, kept time.Time, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(s.relaysPath())
	if err != nil {
		return nil, time.Time{}, false
	}
	data, err = os.ReadFile(s.relaysPath())
	return data, info.ModTime(), err == nil
}

// KeepRelayList replaces the copy of the list of relays.
func (s *Store) KeepRelayList(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeBytes(s.relaysPath(), data)
}

// Keys loads the tunnel keys, creating them on first use.
func (s *Store) Keys() (*tunnel.Keys, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys tunnel.Keys
	err := read(s.keysPath(), &keys)
	if errors.Is(err, fs.ErrNotExist) {
		fresh := tunnel.NewKeys()
		return fresh, write(s.keysPath(), fresh)
	}
	return &keys, err
}

// SaveKeys writes the keys back, after Listen has filled in the region.
func (s *Store) SaveKeys(keys *tunnel.Keys) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return write(s.keysPath(), keys)
}

// Devices lists the paired devices, by name.
func (s *Store) Devices() ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devices()
}

func (s *Store) devices() ([]Device, error) {
	var devices []Device
	if err := read(s.devicesPath(), &devices); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })
	return devices, nil
}

// ErrPairedUnderAnotherKey means a device with that fingerprint is already
// paired, with another tunnel key. Nothing proves the key a device states,
// so the new one could be any device's claim to be the old one.
var ErrPairedUnderAnotherKey = errors.New("this device is already paired under another key; unpair it on both devices, then pair again")

// Add records a paired device, replacing an earlier record of the same one
// with the same key. One with another key is refused with
// ErrPairedUnderAnotherKey.
func (s *Store) Add(d Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	devices, err := s.devices()
	if err != nil {
		return err
	}
	for _, old := range devices {
		if strings.EqualFold(old.Fingerprint, d.Fingerprint) && old.ClientID != d.ClientID {
			return ErrPairedUnderAnotherKey
		}
	}
	return write(s.devicesPath(), append(without(devices, d.Fingerprint), d.cleaned()))
}

// Rename records the name a paired device now gives itself, and reports
// whether that changed anything. A device that is not paired stays so.
func (s *Store) Rename(fingerprint, name string) (bool, error) {
	return s.update(fingerprint, func(d *Device) { d.Name = names.Clean(name) })
}

// Readdress records the tunnel address a paired device is now reached at,
// and reports whether that changed anything. Only an address with the keys
// already stored, on a relay in the list, is taken (see tunnel.Addr.MovedTo).
// A device that is not paired stays so.
func (s *Store) Readdress(fingerprint string, addr tunnel.Addr) (bool, error) {
	return s.update(fingerprint, func(d *Device) {
		if d.Addr.MovedTo(addr) {
			d.Addr = addr
		}
	})
}

// update lets f change a paired device's record, and reports whether the
// record is different and still valid afterwards, in which case it is
// stored.
func (s *Store) update(fingerprint string, f func(*Device)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	devices, err := s.devices()
	if err != nil {
		return false, err
	}
	for i, d := range devices {
		if !strings.EqualFold(d.Fingerprint, fingerprint) {
			continue
		}
		if f(&d); d == devices[i] || !d.valid() {
			break
		}
		devices[i] = d
		// It is asked often and seldom changes, so only then is it written.
		err := write(s.devicesPath(), devices)
		return err == nil, err
	}
	return false, nil
}

// Remove forgets the device with the given fingerprint.
func (s *Store) Remove(fingerprint string) error {
	return s.change(func(devices []Device) []Device { return without(devices, fingerprint) })
}

func (s *Store) change(f func([]Device) []Device) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	devices, err := s.devices()
	if err != nil {
		return err
	}
	return write(s.devicesPath(), f(devices))
}

func without(devices []Device, fingerprint string) []Device {
	out := make([]Device, 0, len(devices))
	for _, d := range devices {
		if !strings.EqualFold(d.Fingerprint, fingerprint) {
			out = append(out, d)
		}
	}
	return out
}

func read(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// write replaces path in one step, so a crash cannot leave half a file.
func write(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeBytes(path, b)
}

// writeBytes is write for what is already in its final form.
func writeBytes(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// The temporary file is made only if none is there: one left there
	// beforehand would keep its mode, so it goes first. (It is removed only
	// when it is there, because in a browser removing what is not there
	// fails in a way of its own.)
	tmp := path + ".tmp"
	create := func() (*os.File, error) {
		return os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	f, err := create()
	if errors.Is(err, fs.ErrExist) {
		if err = os.Remove(tmp); err == nil {
			f, err = create()
		}
	}
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
