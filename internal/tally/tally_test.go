package tally

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"yonder/internal/tally/agent"
)

// list is a made-up country list, in the form of DB-IP's file.
const list = `192.0.2.0,192.0.2.255,NZ
198.51.100.0,198.51.100.127,DE
not,a,line
203.0.113.5,203.0.113.5,FR
2001:db8::,2001:db8:0:ffff:ffff:ffff:ffff:ffff,JP
2001:db8:2::,2001:db8:2::ff,BR
`

// server is a made-up machine: a folder for the totals, a country list, a
// clock to move, and the tally using them.
type server struct {
	t     *testing.T
	dir   string
	list  string
	clock time.Time
	*Tally
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{t: t, dir: filepath.Join(t.TempDir(), "tally"), list: filepath.Join(t.TempDir(), "countries.csv"),
		clock: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	if err := os.WriteFile(s.list, []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	s.restart()
	return s
}

// restart is the program starting again: what was not written is gone.
func (s *server) restart() {
	s.Tally = open(s.dir, LoadCountries(s.list), func() time.Time { return s.clock })
}

func (s *server) flush() {
	s.t.Helper()
	if err := s.Flush(); err != nil {
		s.t.Fatal(err)
	}
}

// read is what Read gives from a day on.
func (s *server) read(since string) []Record {
	s.t.Helper()
	var b bytes.Buffer
	if err := Read(s.dir, since, &b); err != nil {
		s.t.Fatal(err)
	}
	var records []Record
	for dec := json.NewDecoder(&b); dec.More(); {
		var r Record
		if err := dec.Decode(&r); err != nil {
			s.t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}

// records is what the database holds of a day, each as "event country
// version system" with its count.
func (s *server) records(day string) map[string]uint64 {
	s.t.Helper()
	out := map[string]uint64{}
	for _, r := range s.read(day) {
		if r.Day == day {
			out[strings.TrimSpace(fmt.Sprintf("%s %s %s %s", r.Event, r.Country, r.Version, r.System))] += r.Count
		}
	}
	return out
}

func same(t *testing.T, got, want map[string]uint64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

var none netip.Addr

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

// The totals of a day are in the database, by the fields each event has, and
// no address is anywhere in the folder.
func TestTotals(t *testing.T) {
	s := newServer(t)
	visitors := []string{"192.0.2.7", "198.51.100.9", "2001:db8::1234", "8.8.8.8"}
	for _, v := range visitors {
		s.Add(Homepage, addr(v), "0.1.27", "linux")
		s.Add(WebApp, addr(v), "0.1.27", "linux")
		s.Add(UpdateCheck, addr(v), "0.1.27", "linux")
	}
	s.Add(UpdateCheck, addr("192.0.2.8"), "0.1.27", "linux")
	s.Add(UpdateCheck, addr("192.0.2.8"), "0.1.28", "windows")
	s.Add(UpdateCheck, addr("192.0.2.8"), "", "")
	s.Add(RoomOpened, none, "", "")
	s.Add(CodeStarted, none, "", "")
	s.Add(CodeCompleted, none, "", "")
	s.Add(NearbyStarted, none, "", "")
	s.Add(NearbyStarted, none, "", "")
	s.Add(NearbyCompleted, none, "", "")
	s.Add("made-up", none, "", "")
	s.flush()
	same(t, s.records("2026-11-17"), map[string]uint64{
		"homepage":                       4,
		"web-app NZ":                     1,
		"web-app DE":                     1,
		"web-app JP":                     1,
		"web-app":                        1,
		"update-check NZ 0.1.27 linux":   2,
		"update-check DE 0.1.27 linux":   1,
		"update-check JP 0.1.27 linux":   1,
		"update-check  0.1.27 linux":     1,
		"update-check NZ 0.1.28 windows": 1,
		"update-check NZ":                1,
		"room-opened":                    1,
		"code-pairing-started":           1,
		"code-pairing-completed":         1,
		"nearby-pairing-started":         2,
		"nearby-pairing-completed":       1,
	})
	files, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(s.dir, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range append(visitors, "192.0.2.8", "192.0.2.", "2001:db8") {
			if bytes.Contains(b, []byte(v)) {
				t.Fatalf("%s holds the address %s:\n%s", f.Name(), v, b)
			}
		}
	}
	if len(files) != 1 || files[0].Name() != File {
		t.Fatalf("the folder holds %v, want only the database", files)
	}
}

// A count made after midnight UTC belongs to the new day, and the day that
// is over keeps what was counted in it, its last minute too.
func TestMidnight(t *testing.T) {
	s := newServer(t)
	s.Add(Homepage, none, "", "")
	s.flush()
	s.clock = time.Date(2026, 11, 17, 23, 59, 59, 0, time.UTC)
	s.Add(Homepage, none, "", "")
	s.clock = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	s.Add(Homepage, none, "", "")
	s.Add(RoomOpened, none, "", "")
	s.flush()
	same(t, s.records("2026-11-17"), map[string]uint64{"homepage": 2})
	same(t, s.records("2026-11-18"), map[string]uint64{"homepage": 1, "room-opened": 1})

	// The day is by UTC, whatever the clock's own zone.
	s.clock = time.Date(2026, 11, 19, 1, 0, 0, 0, time.FixedZone("east", 2*60*60))
	s.Add(WebApp, none, "", "")
	s.flush()
	same(t, s.records("2026-11-18"), map[string]uint64{"homepage": 1, "room-opened": 1, "web-app": 1})
}

// The rows of a day that is over are not written again.
func TestPastDayIsLeftAlone(t *testing.T) {
	s := newServer(t)
	s.Add(Homepage, none, "", "")
	s.clock = s.clock.Add(24 * time.Hour)
	s.flush()
	if _, err := s.db.Exec(`UPDATE totals SET count = 99 WHERE day = '2026-11-17'`); err != nil {
		t.Fatal(err)
	}
	s.Add(Homepage, none, "", "")
	s.flush()
	s.restart()
	s.Add(Homepage, none, "", "")
	s.flush()
	same(t, s.records("2026-11-17"), map[string]uint64{"homepage": 99})
	same(t, s.records("2026-11-18"), map[string]uint64{"homepage": 2})
}

// After a restart the day's totals carry on from the database.
func TestRestart(t *testing.T) {
	s := newServer(t)
	s.Add(UpdateCheck, addr("192.0.2.7"), "0.1.27", "macos")
	s.Add(Homepage, none, "", "")
	s.flush()
	s.Add(Homepage, none, "", "") // not written: lost with the restart
	s.restart()
	s.Add(UpdateCheck, addr("192.0.2.7"), "0.1.27", "macos")
	s.Add(Homepage, none, "", "")
	s.flush()
	same(t, s.records("2026-11-17"), map[string]uint64{"homepage": 2, "update-check NZ 0.1.27 macos": 2})
}

// dayFile writes a day's totals as the program before the database did.
func (s *server) dayFile(day, content string) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, day+".json"), []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// The day files of the time before the database are read into it once:
// today's is carried on from, and a file that changes later, or comes back
// with a rollback, is not read a second time.
func TestDayFilesAreReadOnce(t *testing.T) {
	s := newServer(t)
	s.dayFile("2026-11-16", `[{"day":"2026-11-16","event":"homepage","country":"","version":"","system":"","count":7},
		{"day":"2026-11-16","event":"update-check","country":"NZ","version":"0.1.27","system":"linux","count":3},
		{"day":"2026-11-15","event":"homepage","country":"","version":"","system":"","count":50}]`)
	s.dayFile("2026-11-17", `[{"day":"2026-11-17","event":"homepage","country":"","version":"","system":"","count":4}]`)
	s.dayFile("notes", `[{"day":"2026-11-14","event":"homepage","country":"","version":"","system":"","count":1}]`)
	// The first server opened an empty folder; the files are found by the
	// next start.
	s.restart()
	s.Add(Homepage, none, "", "")
	s.flush()
	same(t, s.records("2026-11-16"), map[string]uint64{"homepage": 7, "update-check NZ 0.1.27 linux": 3})
	same(t, s.records("2026-11-17"), map[string]uint64{"homepage": 5})
	if early := s.read("2026-11-01"); early[0].Day != "2026-11-16" {
		t.Fatalf("a record of %s was read in from a file of another day", early[0].Day)
	}

	s.dayFile("2026-11-16", `[{"day":"2026-11-16","event":"homepage","country":"","version":"","system":"","count":1000}]`)
	s.restart()
	s.flush()
	same(t, s.records("2026-11-16"), map[string]uint64{"homepage": 7, "update-check NZ 0.1.27 linux": 3})
	same(t, s.records("2026-11-17"), map[string]uint64{"homepage": 5})
}

// A day file that is not a list of totals does not stop the program: it is
// passed over and the day starts from zero.
func TestBrokenDayFile(t *testing.T) {
	s := newServer(t)
	s.dayFile("2026-11-17", "{half")
	s.restart()
	s.Add(Homepage, none, "", "")
	s.flush()
	same(t, s.records("2026-11-17"), map[string]uint64{"homepage": 1})
}

// A database that is not one loses counts and nothing else: Add still
// returns and Flush says what went wrong.
func TestBrokenDatabase(t *testing.T) {
	s := newServer(t)
	s.dir = filepath.Join(t.TempDir(), "tally")
	if err := os.Mkdir(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, File), bytes.Repeat([]byte("not a database\n"), 500), 0o644); err != nil {
		t.Fatal(err)
	}
	s.restart()
	s.Add(Homepage, none, "", "")
	if err := s.Flush(); err == nil {
		t.Fatal("Flush wrote into a file that is not a database")
	}
}

// Read gives the days from the one asked for, in order, and takes nothing
// that is not a day; it reads while the server writes, and with the
// database held for reading the server still writes.
func TestRead(t *testing.T) {
	s := newServer(t)
	for range 3 {
		s.Add(Homepage, none, "", "")
		s.Add(WebApp, addr("192.0.2.7"), "", "")
		s.flush()
		s.clock = s.clock.Add(24 * time.Hour)
	}
	var got []string
	for _, r := range s.read("2026-11-18") {
		got = append(got, fmt.Sprintf("%s %s %s %d", r.Day, r.Event, r.Country, r.Count))
	}
	want := []string{"2026-11-18 homepage  1", "2026-11-18 web-app NZ 1", "2026-11-19 homepage  1", "2026-11-19 web-app NZ 1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
	for _, since := range []string{"", "today", "2026-11-18; rm", "2026-11-18 ", "2026-13-01", "2026-1-1", "' OR 1=1 --"} {
		if err := Read(s.dir, since, &bytes.Buffer{}); err == nil {
			t.Errorf("Read took %q for a day", since)
		}
	}
	if err := Read(filepath.Join(t.TempDir(), "nothing"), "2026-11-18", &bytes.Buffer{}); err == nil {
		t.Error("Read made a database where there was none")
	}

	before, err := os.ReadFile(filepath.Join(s.dir, File))
	if err != nil {
		t.Fatal(err)
	}
	s.read("2026-11-01")
	if after, _ := os.ReadFile(filepath.Join(s.dir, File)); !bytes.Equal(before, after) {
		t.Fatal("Read changed the database")
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 50 {
			s.Add(Homepage, none, "", "")
			if err := s.Flush(); err != nil {
				t.Errorf("a flush beside a reader: %v", err)
			}
		}
	})
	for range 50 {
		if err := Read(s.dir, "2026-11-01", &bytes.Buffer{}); err != nil {
			t.Errorf("a read beside the writer: %v", err)
		}
	}
	wg.Wait()

	// A reader needs no right to write: not to the file, not in the folder.
	for name, mode := range map[string]os.FileMode{File: 0o444, "": 0o555} {
		if err := os.Chmod(filepath.Join(s.dir, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { os.Chmod(s.dir, 0o755) })
	if _, err := os.Create(filepath.Join(s.dir, "probe")); err == nil {
		t.Skip("this user writes where it has no right to (root?)")
	}
	if n := len(s.read("2026-11-01")); n != 7 { // three days of two, and the writer's day
		t.Fatalf("a reader with no right to write read %d records, want 7", n)
	}
}

// A folder that cannot be written loses counts and nothing else: Add still
// returns, Flush says what went wrong, and the counts of today are written
// once the folder can be.
func TestFolderCannotBeWritten(t *testing.T) {
	s := newServer(t)
	// A file where the folder should be: nothing can be made in it, for
	// any user.
	s.dir = filepath.Join(t.TempDir(), "tally")
	if err := os.WriteFile(s.dir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s.restart()
	s.Add(Homepage, none, "", "")
	if err := s.Flush(); err == nil {
		t.Fatal("Flush wrote into a folder that is not there")
	}
	s.Add(Homepage, none, "", "")
	if err := os.Remove(s.dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s.flush()
	same(t, s.records("2026-11-17"), map[string]uint64{"homepage": 2})
}

// A version or system that is not in the known shape is not kept.
func TestMadeUpAppFields(t *testing.T) {
	s := newServer(t)
	for _, c := range []struct{ version, system string }{
		{"0.1.27", "linux"},
		{"1.2", "linux"},
		{"1.2.3.4", "linux"},
		{"v1.2.3", "linux"},
		{"1.2.3-beta", "linux"},
		{"1.2.3\n", "linux"},
		{"1234.1.1", "linux"},
		{strings.Repeat("9", 500), "linux"},
		{"0.1.27", "Linux"},
		{"0.1.27", "plan9"},
		{"0.1.27", "linux "},
		{"../../etc", `"}]`},
	} {
		s.Add(UpdateCheck, none, c.version, c.system)
	}
	s.flush()
	same(t, s.records("2026-11-17"), map[string]uint64{
		"update-check  0.1.27 linux": 1,
		"update-check   linux":       7,
		"update-check  0.1.27":       3,
		"update-check":               1,
	})
}

// What an app sends is what the server reads, and what Add keeps.
func TestAgent(t *testing.T) {
	for _, c := range []struct{ version, goos, agent, kept string }{
		{"0.1.27", "linux", "Yonder/0.1.27 (linux)", "update-check  0.1.27 linux"},
		{"0.1.27", "windows", "Yonder/0.1.27 (windows)", "update-check  0.1.27 windows"},
		{"0.1.27", "darwin", "Yonder/0.1.27 (macos)", "update-check  0.1.27 macos"},
		{"0.1.27-3-gabc dirty", "linux", "Yonder/dev (linux)", "update-check   linux"},
		{"", "darwin", "Yonder/dev (macos)", "update-check   macos"},
	} {
		name := agent.Name(c.version, c.goos)
		if name != c.agent {
			t.Errorf("Name(%q, %q) = %q, want %q", c.version, c.goos, name, c.agent)
		}
		s := newServer(t)
		version, system := agent.Parse(name)
		s.Add(UpdateCheck, netip.Addr{}, version, system)
		s.flush()
		same(t, s.records("2026-11-17"), map[string]uint64{c.kept: 1})
	}
}

// However many versions strangers make up, a day's rows stay few, and
// every count is still in it.
func TestRecordsAreCapped(t *testing.T) {
	s := newServer(t)
	const n = maxKeys + 500
	for i := range n {
		s.Add(UpdateCheck, addr("192.0.2.7"), fmt.Sprintf("%d.%d.%d", i/1000, i/10%100, i%10), "linux")
	}
	s.flush()
	records := s.records("2026-11-17")
	if len(records) > maxKeys+1 {
		t.Fatalf("the day holds %d records, more than %d", len(records), maxKeys+1)
	}
	var total uint64
	for _, c := range records {
		total += c
	}
	if total != n {
		t.Fatalf("the day's counts add up to %d, want %d", total, n)
	}
}

// Counting from many goroutines at once loses nothing (and, under -race,
// shares nothing unguarded), with flushes in between.
func TestManyAtOnce(t *testing.T) {
	s := newServer(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 100 {
				s.Add(WebApp, addr("192.0.2.7"), "", "")
			}
			s.Flush()
		})
	}
	wg.Wait()
	s.flush()
	same(t, s.records("2026-11-17"), map[string]uint64{"web-app NZ": 2000})
}

// A nil tally counts nothing and does not fail.
func TestNil(t *testing.T) {
	var s *Tally
	s.Add(Homepage, addr("192.0.2.7"), "", "")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
}

func TestCountries(t *testing.T) {
	s := newServer(t)
	c := s.countries
	for a, want := range map[string]string{
		"192.0.2.0":                              "NZ", // the first address of a range
		"192.0.2.99":                             "NZ",
		"192.0.2.255":                            "NZ", // and the last
		"192.0.1.255":                            "",   // one before
		"192.0.3.0":                              "",   // one after
		"198.51.100.127":                         "DE",
		"198.51.100.128":                         "",
		"203.0.113.5":                            "FR", // a range of one address
		"203.0.113.4":                            "",
		"203.0.113.6":                            "",
		"0.0.0.0":                                "",
		"255.255.255.255":                        "",
		"::ffff:192.0.2.9":                       "NZ", // an IPv4 address written as IPv6
		"2001:db8::":                             "JP",
		"2001:db8::1":                            "JP",
		"2001:db8:0:ffff:ffff:ffff:ffff:ffff":    "JP",
		"2001:db8:1::":                           "",
		"2001:db7:ffff:ffff:ffff:ffff:ffff:ffff": "",
		"2001:db8:2::ff":                         "BR",
		"2001:db8:2::100":                        "",
		"::1":                                    "",
	} {
		if got := c.Lookup(addr(a)); got != want {
			t.Errorf("the country of %s is %q, want %q", a, got, want)
		}
	}
	if got := c.Lookup(none); got != "" {
		t.Errorf("the country of no address is %q", got)
	}
}

// The list is read compressed, as DB-IP gives it.
func TestCountriesCompressed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "countries.csv.gz")
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	z.Write([]byte(list))
	z.Close()
	if err := os.WriteFile(file, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	c := LoadCountries(file)
	if got := c.Lookup(addr("192.0.2.9")); got != "NZ" {
		t.Fatalf("the country of 192.0.2.9 is %q, want NZ", got)
	}
	if got := c.Lookup(addr("2001:db8:2::1")); got != "BR" {
		t.Fatalf("the country of 2001:db8:2::1 is %q, want BR", got)
	}
}

// Without a list, or with one that cannot be read, every country is empty
// and counting goes on; a list that arrives later is picked up.
func TestCountriesMissing(t *testing.T) {
	var no *Countries
	if got := no.Lookup(addr("192.0.2.9")); got != "" {
		t.Fatalf("no list gave the country %q", got)
	}
	file := filepath.Join(t.TempDir(), "countries.csv")
	c := LoadCountries(file)
	if got := c.Lookup(addr("192.0.2.9")); got != "" {
		t.Fatalf("a missing list gave the country %q", got)
	}
	if err := os.WriteFile(file, []byte("nothing of use\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.Refresh()
	if got := c.Lookup(addr("192.0.2.9")); got != "" {
		t.Fatalf("a list with no ranges gave the country %q", got)
	}
	tl := open(filepath.Join(t.TempDir(), "tally"), c, time.Now)
	tl.Add(WebApp, addr("192.0.2.9"), "", "")
	if err := tl.Flush(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(file, []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	// The file's time is what says it is new; set it, so the test does
	// not lean on the clock's grain.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	c.Refresh()
	if got := c.Lookup(addr("192.0.2.9")); got != "NZ" {
		t.Fatalf("after the list arrived the country of 192.0.2.9 is %q, want NZ", got)
	}
}

// The real list, for the facts a made-up one cannot show: that DB-IP's file
// has the shape this package reads, and what holding it costs. Run it with
// YONDER_COUNTRIES set to a downloaded dbip-country-lite-YYYY-MM.csv.gz.
func TestRealCountryList(t *testing.T) {
	file := os.Getenv("YONDER_COUNTRIES")
	if file == "" {
		t.Skip("YONDER_COUNTRIES is not set")
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	c := LoadCountries(file)
	took := time.Since(start)
	runtime.GC()
	runtime.ReadMemStats(&after)
	l := c.lists.Load()
	if l == nil || len(l.v4) == 0 || len(l.v6) == 0 {
		t.Fatal("the list was not read, or holds no IPv4 or no IPv6 ranges")
	}
	t.Logf("%d IPv4 and %d IPv6 ranges, read in %v, %.1f MB held", len(l.v4), len(l.v6), took.Round(time.Millisecond), float64(after.HeapAlloc-before.HeapAlloc)/1e6)
	// Addresses the list of 2026-11 puts in the United States. The list is
	// not the truth about an address that is served from many places
	// (it has Google's 2001:4860:4860::8888 in Canada); these two are what
	// it says, read from its own lines.
	for _, a := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if got := c.Lookup(netip.MustParseAddr(a)); got != "US" {
			t.Errorf("%s is in %q, want US", a, got)
		}
	}
}
