package usage

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// server is a made-up machine: a counter file to set, a clock to move, and
// the meter reading both.
type server struct {
	t             *testing.T
	counter, file string
	clock         time.Time
}

func newServer(t *testing.T) *server {
	dir := t.TempDir()
	s := &server{t: t, counter: filepath.Join(dir, "tx_bytes"), file: filepath.Join(dir, "usage.json"),
		clock: time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	s.count(0)
	return s
}

// count sets the interface's counter, as the system would.
func (s *server) count(n uint64) {
	s.t.Helper()
	if err := os.WriteFile(s.counter, []byte(strconv.FormatUint(n, 10)+"\n"), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// open starts the meter, as a start of the server program would.
func (s *server) open(limitGB uint64) *Meter {
	s.t.Helper()
	m, err := open(s.counter, s.file, limitGB*GB, func() time.Time { return s.clock })
	if err != nil {
		s.t.Fatal(err)
	}
	return m
}

func update(t *testing.T, m *Meter) {
	t.Helper()
	if err := m.Update(); err != nil {
		t.Fatal(err)
	}
}

func TestCountsWhatIsSent(t *testing.T) {
	s := newServer(t)
	s.count(5 * GB) // sent before counting began: not ours
	m := s.open(1)
	if got := m.Sent(); got != 0 {
		t.Fatalf("a fresh count is %d, want 0", got)
	}
	s.count(5*GB + 300)
	update(t, m)
	s.count(5*GB + 1000)
	update(t, m)
	if got := m.Sent(); got != 1000 {
		t.Fatalf("sent %d, want 1000", got)
	}
	if m.Over() {
		t.Fatal("over the limit after 1000 bytes of a gigabyte")
	}
	s.count(6*GB + 1000)
	update(t, m)
	if !m.Over() {
		t.Fatal("not over the limit after a gigabyte and more")
	}
}

func TestNoLimit(t *testing.T) {
	s := newServer(t)
	m := s.open(0)
	s.count(1000 * GB)
	update(t, m)
	if m.Over() {
		t.Fatal("over a limit that was not set")
	}
}

func TestProgramRestartKeepsTheCount(t *testing.T) {
	s := newServer(t)
	m := s.open(1)
	s.count(400)
	update(t, m)
	s.count(700) // sent while the program was down: still counted
	m = s.open(1)
	update(t, m)
	if got := m.Sent(); got != 700 {
		t.Fatalf("sent %d after a restart of the program, want 700", got)
	}
}

func TestMachineRestart(t *testing.T) {
	s := newServer(t)
	m := s.open(1)
	s.count(400)
	update(t, m)
	s.count(50) // the counter began again from zero
	update(t, m)
	if got := m.Sent(); got != 450 {
		t.Fatalf("sent %d after a restart of the machine, want 450", got)
	}
}

func TestNewMonth(t *testing.T) {
	s := newServer(t)
	m := s.open(1)
	s.count(2 * GB)
	update(t, m)
	if !m.Over() {
		t.Fatal("not over the limit")
	}
	if got, want := m.Resets(), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("resets %v, want %v", got, want)
	}
	s.clock = time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC)
	if m.Over() {
		t.Fatal("still over the limit in the new month, before an update")
	}
	s.count(2*GB + 10)
	update(t, m)
	if got := m.Sent(); got != 10 {
		t.Fatalf("sent %d in the new month, want 10", got)
	}
	// And it is the new month that was saved.
	if got := s.open(1).Sent(); got != 10 {
		t.Fatalf("sent %d after reopening in the new month, want 10", got)
	}
}

func TestResetsAcrossTheYear(t *testing.T) {
	s := newServer(t)
	s.clock = time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)
	if got, want := s.open(1).Resets(), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("resets %v, want %v", got, want)
	}
}

func TestUnreadableCountStartsAgain(t *testing.T) {
	s := newServer(t)
	if err := os.WriteFile(s.file, []byte("not a count"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.count(900)
	m := s.open(1)
	s.count(1000)
	update(t, m)
	if got := m.Sent(); got != 100 {
		t.Fatalf("sent %d, want 100", got)
	}
}

func TestMissingCounter(t *testing.T) {
	s := newServer(t)
	m := s.open(1)
	s.count(400)
	update(t, m)
	if err := os.Remove(s.counter); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(); err == nil {
		t.Fatal("an update with no counter to read did not fail")
	}
	if got := m.Sent(); got != 400 {
		t.Fatalf("sent %d after a failed update, want 400", got)
	}
	if _, err := open(s.counter, filepath.Join(t.TempDir(), "usage.json"), GB, time.Now); err == nil {
		t.Fatal("a meter opened with no counter to read")
	}
}
