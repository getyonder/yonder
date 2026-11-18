// Package usage keeps count of what Yonder's server has sent this month, so
// the web version can close before the server's traffic costs money.
//
// It counts what the server's host counts and bills: bytes sent out of the
// network interface, and none of those coming in. A file that passes through
// the relay comes in from one device and goes out to the other, so it counts
// once.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GB is a gigabyte as the limit means it. The smaller of the two sizes in
// use, so the limit is reached no later than the host would say.
const GB = 1_000_000_000

// CounterPath is where Linux keeps the bytes an interface has sent since the
// machine started.
func CounterPath(iface string) string {
	return filepath.Join("/sys/class/net", iface, "statistics", "tx_bytes")
}

// Meter is the month's count. The zero value is not usable; call Open.
type Meter struct {
	counter, file string
	limit         uint64 // bytes; 0 for none
	// now is the clock; tests replace it.
	now func() time.Time

	mu    sync.Mutex
	state state
}

// state is what is kept between runs.
type state struct {
	// Month is the month Sent is for, as "2006-01", in UTC.
	Month string `json:"month"`
	// Sent is the bytes sent in Month so far.
	Sent uint64 `json:"sent"`
	// Counter is the interface's counter when it was last read.
	Counter uint64 `json:"counter"`
}

// Open returns a meter that reads the counter file at counter (see
// CounterPath) and keeps the month's count in file. limit is the month's
// limit in bytes; 0 means none. A count kept from before is carried on. With none
// kept, counting starts from now: what the counter already holds is not
// this month's to answer for.
func Open(counter, file string, limit uint64) (*Meter, error) {
	return open(counter, file, limit, time.Now)
}

func open(counter, file string, limit uint64, now func() time.Time) (*Meter, error) {
	m := &Meter{counter: counter, file: file, limit: limit, now: now}
	b, err := os.ReadFile(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &m.state); err == nil {
			return m, nil
		}
		// A file that cannot be read as a count is no reason to keep the
		// site down; the month starts again.
		log.Printf("usage: %s is not a count, starting from zero", file)
		m.state = state{}
	}
	cur, err := m.read()
	if err != nil {
		return nil, err
	}
	m.state = state{Month: month(now()), Counter: cur}
	return m, m.save()
}

func month(t time.Time) string { return t.UTC().Format("2006-01") }

// read returns the interface's counter.
func (m *Meter) read() (uint64, error) {
	b, err := os.ReadFile(m.counter)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("usage: %s does not hold a number", m.counter)
	}
	return n, nil
}

// save replaces the file in one step, so a crash cannot leave half of one.
// m.mu must be held, or the meter not yet shared.
func (m *Meter) save() error {
	b, err := json.Marshal(m.state)
	if err != nil {
		return err
	}
	tmp := m.file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.file)
}

// Update reads the counter, adds what was sent since the last reading to
// the month's count, and saves it.
func (m *Meter) Update() error {
	cur, err := m.read()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sent := cur - m.state.Counter
	if cur < m.state.Counter {
		// The counter starts from zero when the machine does, so all of it
		// is new.
		sent = cur
	}
	// What was sent since the last reading goes to the month it was read
	// in: at most a minute of it belongs to the month before.
	if now := month(m.now()); now != m.state.Month {
		m.state.Month, m.state.Sent = now, 0
	}
	m.state.Sent += sent
	m.state.Counter = cur
	return m.save()
}

// Run updates the count every so often until ctx ends.
func (m *Meter) Run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := m.Update(); err != nil {
				log.Printf("usage: %v", err)
			}
		}
	}
}

// Sent is the bytes sent this month, as of the last update.
func (m *Meter) Sent() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if month(m.now()) != m.state.Month {
		return 0 // a new month the next update has yet to see
	}
	return m.state.Sent
}

// Over reports whether the month's limit has been reached.
func (m *Meter) Over() bool { return m.limit > 0 && m.Sent() >= m.limit }

// Resets is when the count next starts from zero: the first moment of next
// month, in UTC.
func (m *Meter) Resets() time.Time {
	now := m.now().UTC()
	return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}
