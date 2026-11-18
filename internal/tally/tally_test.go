package tally

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"yonder/internal/tally/agent"
)

// list is a made-up list of places, in the form of DB-IP's file. Its first
// two lines are one place.
const list = `192.0.2.0,192.0.2.127,OC,NZ,Auckland,Auckland,-36.8485,174.763
192.0.2.128,192.0.2.255,OC,NZ,Auckland,"Manukau City",-36.9928,174.88
198.51.100.0,198.51.100.127,EU,DE,Bavaria,Munich,48.1351,11.582
not,a,line
203.0.113.5,203.0.113.5,EU,FR,"Provence-Alpes-Côte d'Azur",Nice,43.7102,7.26195
203.0.113.9,203.0.113.9,ZZ,ZZ,,,0,0
2001:db8::,2001:db8:0:ffff:ffff:ffff:ffff:ffff,AS,JP,Tokyo,Tokyo,35.6895,139.692
2001:db8:2::,2001:db8:2::ff,SA,BR,,,-10,-55
`

// build makes the small file of places from a list, as the monthly run does.
func build(t *testing.T, list, out string) PlacesReport {
	t.Helper()
	csv := filepath.Join(t.TempDir(), "city.csv")
	if err := os.WriteFile(csv, []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := BuildPlaces(csv, out)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// server is a made-up machine: a folder for the totals, a list of places, a
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
	s := &server{t: t, dir: filepath.Join(t.TempDir(), "tally"), list: filepath.Join(t.TempDir(), "places"),
		clock: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	build(t, list, s.list)
	s.restart()
	return s
}

// restart is the program starting again: what was not written is gone.
func (s *server) restart() {
	s.Tally = open(s.dir, LoadPlaces(s.list), func() time.Time { return s.clock })
}

func (s *server) flush() {
	s.t.Helper()
	if err := s.Flush(); err != nil {
		s.t.Fatal(err)
	}
}

// read is what Read gives of the totals from a day on.
func (s *server) read(since string) []Record { return s.readTable(since, false) }

// regions is what the database holds of a day by state, each as "event
// country region" with its count.
func (s *server) regions(day string) map[string]uint64 {
	s.t.Helper()
	out := map[string]uint64{}
	for _, r := range s.readTable(day, true) {
		if r.Day == day {
			if !r.Valid() || r.Region == "" {
				s.t.Errorf("the record by state %+v is not one", r)
			}
			out[fmt.Sprintf("%s %s %s", r.Event, r.Country, r.Region)] += r.Count
		}
	}
	return out
}

func (s *server) readTable(since string, regions bool) []Record {
	s.t.Helper()
	var b bytes.Buffer
	if err := Read(s.dir, since, regions, &b); err != nil {
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
			if !r.Valid() || r.Region != "" {
				s.t.Errorf("the total %+v is not one", r)
			}
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
	// By state: only the events that have a country, only where the list
	// names a state, and nothing of the app.
	same(t, s.regions("2026-11-17"), map[string]uint64{
		"web-app NZ Auckland":      1,
		"web-app DE Bavaria":       1,
		"web-app JP Tokyo":         1,
		"update-check NZ Auckland": 4,
		"update-check DE Bavaria":  1,
		"update-check JP Tokyo":    1,
	})
	s.Add(WebApp, addr("2001:db8:2::1"), "", "") // a country with no state
	s.flush()
	if got := s.records("2026-11-17")["web-app BR"]; got != 1 {
		t.Fatalf("web-app BR is %d, want 1", got)
	}
	if got := s.regions("2026-11-17"); len(got) != 6 {
		t.Fatalf("a country with no state is counted by state: %v", got)
	}
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
		for _, regions := range []bool{false, true} {
			if err := Read(s.dir, since, regions, &bytes.Buffer{}); err == nil {
				t.Errorf("Read took %q for a day", since)
			}
		}
	}
	got = nil
	for _, r := range s.readTable("2026-11-18", true) {
		got = append(got, fmt.Sprintf("%s %s %s %s %d", r.Day, r.Event, r.Country, r.Region, r.Count))
	}
	want = []string{"2026-11-18 web-app NZ Auckland 1", "2026-11-19 web-app NZ Auckland 1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("by state: got %q, want %q", got, want)
	}
	if err := Read(filepath.Join(t.TempDir(), "nothing"), "2026-11-18", false, &bytes.Buffer{}); err == nil {
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
		if err := Read(s.dir, "2026-11-01", false, &bytes.Buffer{}); err != nil {
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

// The records by state of a day have a limit too; past it a new state is in
// the totals only, and a state that is known is still counted.
func TestRegionsAreCapped(t *testing.T) {
	s := newServer(t)
	var list strings.Builder
	const n = maxRegions + 10
	for i := range n {
		fmt.Fprintf(&list, "10.%d.%d.0,10.%d.%d.255,OC,NZ,State %d,City,0,0\n", i/256, i%256, i/256, i%256, i)
	}
	build(t, list.String(), s.list)
	later := time.Now().Add(time.Hour)
	os.Chtimes(s.list, later, later)
	s.places.Refresh()
	for i := range n {
		s.Add(WebApp, netip.AddrFrom4([4]byte{10, byte(i / 256), byte(i % 256), 1}), "", "")
	}
	s.Add(WebApp, addr("10.0.0.1"), "", "")
	s.flush()
	if got := s.records("2026-11-17")["web-app NZ"]; got != n+1 {
		t.Fatalf("web-app NZ is %d, want %d", got, n+1)
	}
	regions := s.regions("2026-11-17")
	if len(regions) != maxRegions {
		t.Fatalf("the day holds %d records by state, want %d", len(regions), maxRegions)
	}
	if got := regions["web-app NZ State 0"]; got != 2 {
		t.Fatalf("a state that was known is counted %d times past the limit, want 2", got)
	}
}

// A record from another machine is kept only in the shapes Add makes.
func TestValid(t *testing.T) {
	ok := Record{Day: "2026-11-17", Event: UpdateCheck, Country: "NZ", Version: "0.1.41", System: "linux", Count: 1}
	state := Record{Day: "2026-11-17", Event: WebApp, Country: "NZ", Region: "Auckland", Count: 1}
	if !ok.Valid() || !state.Valid() {
		t.Fatal("a record that Add makes is not taken")
	}
	for name, change := range map[string]func(*Record){
		"a state with a version":       func(r *Record) { r.Version = "0.1.41" },
		"a state with a system":        func(r *Record) { r.System = "linux" },
		"a state with no country":      func(r *Record) { r.Country = "" },
		"a state of a pairing":         func(r *Record) { r.Event = RoomOpened },
		"a state with a line break":    func(r *Record) { r.Region = "Auck\nland" },
		"a state of 65 bytes":          func(r *Record) { r.Region = strings.Repeat("a", maxRegion+1) },
		"a state that is not UTF-8":    func(r *Record) { r.Region = "Auck\xffland" },
		"a country in small letters":   func(r *Record) { r.Country = "nz" },
		"a day that is not one":        func(r *Record) { r.Day = "2026-13-01" },
		"an event that is not counted": func(r *Record) { r.Event = "made-up" },
	} {
		r := state
		if change(&r); r.Valid() {
			t.Errorf("%s is taken: %+v", name, r)
		}
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

func TestPlaces(t *testing.T) {
	s := newServer(t)
	p := s.places
	for a, want := range map[string]place{
		"192.0.2.0":                              {"NZ", "Auckland"}, // the first address of a range
		"192.0.2.99":                             {"NZ", "Auckland"},
		"192.0.2.128":                            {"NZ", "Auckland"}, // the next line, the same place
		"192.0.2.255":                            {"NZ", "Auckland"}, // and the last
		"192.0.1.255":                            {},                 // one before
		"192.0.3.0":                              {},                 // one after
		"198.51.100.127":                         {"DE", "Bavaria"},
		"198.51.100.128":                         {},
		"203.0.113.5":                            {"FR", "Provence-Alpes-Côte d'Azur"}, // a range of one address
		"203.0.113.4":                            {},
		"203.0.113.6":                            {},
		"203.0.113.9":                            {}, // a range that is in no country
		"0.0.0.0":                                {},
		"255.255.255.255":                        {},
		"::ffff:192.0.2.9":                       {"NZ", "Auckland"}, // an IPv4 address written as IPv6
		"2001:db8::":                             {"JP", "Tokyo"},
		"2001:db8::1":                            {"JP", "Tokyo"},
		"2001:db8:0:ffff:ffff:ffff:ffff:ffff":    {"JP", "Tokyo"},
		"2001:db8:1::":                           {},
		"2001:db7:ffff:ffff:ffff:ffff:ffff:ffff": {},
		"2001:db8:2::ff":                         {"BR", ""}, // a country with no state
		"2001:db8:2::100":                        {},
		"::1":                                    {},
		"ffff:ffff:ffff:ffff:ffff:ffff:ffff:ffff": {},
	} {
		if country, region := p.Lookup(addr(a)); (place{country, region}) != want {
			t.Errorf("the place of %s is %q %q, want %q %q", a, country, region, want.country, want.region)
		}
	}
	if country, region := p.Lookup(none); country != "" || region != "" {
		t.Errorf("the place of no address is %q %q", country, region)
	}
}

// What the monthly run says of a list, and that the small file holds
// nothing finer than a state: no city, and no coordinate.
func TestBuildPlaces(t *testing.T) {
	out := filepath.Join(t.TempDir(), "places")
	rep := build(t, list, out)
	// Entries: for IPv4 a start in no place, NZ, none, DE, none, FR, none;
	// for IPv6 a start in no place, JP, none, BR, none.
	want := PlacesReport{Rows: 7, Skipped: 1, Places: 5, V4: 7, V6: 5, Gaps: 6, Unnamed: 1}
	if rep != want {
		t.Fatalf("the report is %+v, want %+v", rep, want)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, finer := range []string{"Manukau", "Munich", "Nice", "36.8485", "174.763", "11.582", "7.26195", "139.692"} {
		if bytes.Contains(b, []byte(finer)) {
			t.Errorf("the small file holds %q", finer)
		}
	}
	for _, kept := range []string{"NZ\x08Auckland", "DE\x07Bavaria", "FR\x1bProvence-Alpes-Côte d'Azur"} {
		if !bytes.Contains(b, []byte(kept)) {
			t.Errorf("the small file does not hold %q", kept)
		}
	}
}

// A list that cannot be used leaves the small file as it was.
func TestBuildPlacesBadList(t *testing.T) {
	out := filepath.Join(t.TempDir(), "places")
	build(t, list, out)
	before, _ := os.ReadFile(out)
	for name, bad := range map[string]string{
		"no ranges":       "nothing of use\n",
		"out of order":    "198.51.100.0,198.51.100.127,EU,DE,Bavaria,Munich,0,0\n192.0.2.0,192.0.2.255,OC,NZ,Auckland,Auckland,0,0\n",
		"IPv4 after IPv6": "2001:db8::,2001:db8::ffff,AS,JP,Tokyo,Tokyo,0,0\n192.0.2.0,192.0.2.255,OC,NZ,Auckland,Auckland,0,0\n",
		"overlapping":     "192.0.2.0,192.0.2.255,OC,NZ,Auckland,Auckland,0,0\n192.0.2.255,192.0.3.9,EU,DE,Bavaria,Munich,0,0\n",
	} {
		csv := filepath.Join(t.TempDir(), "city.csv")
		if err := os.WriteFile(csv, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildPlaces(csv, out); err == nil {
			t.Errorf("a list with %s was taken", name)
		}
		if after, _ := os.ReadFile(out); !bytes.Equal(before, after) {
			t.Errorf("a list with %s changed the small file", name)
		}
		if _, err := os.Stat(out + ".new"); err == nil {
			t.Errorf("a list with %s left a half-made file", name)
		}
	}
}

// The list is read compressed, as DB-IP gives it.
func TestBuildPlacesCompressed(t *testing.T) {
	csv := filepath.Join(t.TempDir(), "city.csv.gz")
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	z.Write([]byte(list))
	z.Close()
	if err := os.WriteFile(csv, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "places")
	if _, err := BuildPlaces(csv, out); err != nil {
		t.Fatal(err)
	}
	p := LoadPlaces(out)
	if country, region := p.Lookup(addr("192.0.2.9")); country != "NZ" || region != "Auckland" {
		t.Fatalf("the place of 192.0.2.9 is %q %q, want NZ Auckland", country, region)
	}
	if country, _ := p.Lookup(addr("2001:db8:2::1")); country != "BR" {
		t.Fatalf("the country of 2001:db8:2::1 is %q, want BR", country)
	}
}

// Without a small file, or with one that cannot be read, every place is
// empty and counting goes on; a file that arrives later is picked up, and
// so is the one that replaces it.
func TestPlacesMissing(t *testing.T) {
	var no *Places
	if country, _ := no.Lookup(addr("192.0.2.9")); country != "" {
		t.Fatalf("no list gave the country %q", country)
	}
	file := filepath.Join(t.TempDir(), "places")
	p := LoadPlaces(file)
	if country, _ := p.Lookup(addr("192.0.2.9")); country != "" {
		t.Fatalf("a missing list gave the country %q", country)
	}
	whole, err := os.ReadFile(newServer(t).list)
	if err != nil {
		t.Fatal(err)
	}
	// The file's time is what says it is new; set it, so the test does
	// not lean on the clock's grain.
	later := time.Now()
	put := func(b []byte) {
		t.Helper()
		if err := os.WriteFile(file+".new", b, 0o644); err != nil {
			t.Fatal(err)
		}
		later = later.Add(time.Hour)
		if err := os.Chtimes(file+".new", later, later); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(file+".new", file); err != nil {
			t.Fatal(err)
		}
		p.Refresh()
	}
	for name, bad := range map[string][]byte{
		"is something else":   []byte("nothing of use\n"),
		"is cut short":        whole[:len(whole)-1],
		"has a byte too many": append(slices.Clone(whole), 0),
		"names a bad country": bytes.Replace(whole, []byte("NZ\x08Auckland"), []byte("nz\x08Auckland"), 1),
		"names a bad state":   bytes.Replace(whole, []byte("NZ\x08Auckland"), []byte("NZ\x08Auck\x00and"), 1),
	} {
		put(bad)
		if country, _ := p.Lookup(addr("192.0.2.9")); country != "" {
			t.Fatalf("a file that %s gave the country %q", name, country)
		}
	}
	tl := open(filepath.Join(t.TempDir(), "tally"), p, time.Now)
	tl.Add(WebApp, addr("192.0.2.9"), "", "")
	if err := tl.Flush(); err != nil {
		t.Fatal(err)
	}

	put(whole)
	if country, _ := p.Lookup(addr("192.0.2.9")); country != "NZ" {
		t.Fatalf("after the list arrived the country of 192.0.2.9 is %q, want NZ", country)
	}
	// The next month's list, with the address somewhere else.
	next := filepath.Join(t.TempDir(), "places")
	build(t, "192.0.2.0,192.0.2.255,EU,DE,Bavaria,Munich,48.1351,11.582\n", next)
	b, _ := os.ReadFile(next)
	put(b)
	if country, region := p.Lookup(addr("192.0.2.9")); country != "DE" || region != "Bavaria" {
		t.Fatalf("after the new list the place of 192.0.2.9 is %q %q, want DE Bavaria", country, region)
	}
	// A file that cannot be read leaves the places as they were.
	put([]byte("nothing of use\n"))
	if country, _ := p.Lookup(addr("192.0.2.9")); country != "DE" {
		t.Fatalf("after a bad file the country of 192.0.2.9 is %q, want DE", country)
	}
}

// Lookups go on while the list is replaced.
func TestPlacesReplacedWhileRead(t *testing.T) {
	s := newServer(t)
	p := s.places
	whole, _ := os.ReadFile(s.list)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if country, region := p.Lookup(addr("192.0.2.9")); country != "NZ" || region != "Auckland" {
					t.Errorf("the place of 192.0.2.9 is %q %q", country, region)
					return
				}
			}
		})
	}
	later := time.Now()
	for range 50 {
		if err := os.WriteFile(s.list+".new", whole, 0o644); err != nil {
			t.Fatal(err)
		}
		later = later.Add(time.Hour)
		os.Chtimes(s.list+".new", later, later)
		os.Rename(s.list+".new", s.list)
		p.Refresh()
	}
	close(stop)
	wg.Wait()
}

// The real list, for the facts a made-up one cannot show: that DB-IP's file
// has the shape this package reads, whether it has gaps, and how large the
// small file is. Run it with YONDER_CITIES set to a downloaded
// dbip-city-lite-YYYY-MM.csv.gz.
func TestRealCityList(t *testing.T) {
	file := os.Getenv("YONDER_CITIES")
	if file == "" {
		t.Skip("YONDER_CITIES is not set")
	}
	out := filepath.Join(t.TempDir(), "places")
	start := time.Now()
	rep, err := BuildPlaces(file, out)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(out)
	t.Logf("%v, made in %v, %.1f MB", rep, time.Since(start).Round(time.Millisecond), float64(st.Size())/1e6)
	p := LoadPlaces(out)
	// Addresses the list of 2026-11 puts in these places, read from its own
	// lines. The list is not the truth about an address that is served from
	// many places.
	for a, want := range map[string]place{
		"1.0.0.1":         {"AU", "Queensland"},
		"113.186.58.9":    {"VN", "Ninh Binh"},
		"2601:18e:c000::": {"US", "Massachusetts"},
		"127.0.0.1":       {},
	} {
		if country, region := p.Lookup(netip.MustParseAddr(a)); (place{country, region}) != want {
			t.Errorf("%s is in %q %q, want %q %q", a, country, region, want.country, want.region)
		}
	}
}
