// Package tally counts what happens on Yonder's server: how many times a
// day the homepage was loaded, the web version started, an app asked for an
// update, and a pairing began and ended. It keeps totals only. A visitor's
// address is used to find a country (countries.go) and goes no further: it
// is not kept, written or logged.
//
// The totals are rows of one table in a SQLite file, tally.db, in a folder.
// The name is tally because usage already means the month's traffic count.
//
// This package names no SQLite driver: the program that keeps the totals
// imports one (modernc.org/sqlite, as "sqlite").
package tally

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"yonder/internal/tally/agent"
)

// Event is a thing that is counted.
type Event string

const (
	Homepage        Event = "homepage"
	WebApp          Event = "web-app"
	UpdateCheck     Event = "update-check"
	RoomOpened      Event = "room-opened"
	CodeStarted     Event = "code-pairing-started"
	CodeCompleted   Event = "code-pairing-completed"
	NearbyStarted   Event = "nearby-pairing-started"
	NearbyCompleted Event = "nearby-pairing-completed"
)

// hasCountry says which events are counted by country, and hasApp which by
// the app's version and system. Every other event is counted by day alone,
// whatever Add is given. An event in neither is still counted; one that is
// not listed here at all is not.
var (
	events     = []Event{Homepage, WebApp, UpdateCheck, RoomOpened, CodeStarted, CodeCompleted, NearbyStarted, NearbyCompleted}
	hasCountry = map[Event]bool{WebApp: true, UpdateCheck: true}
	hasApp     = map[Event]bool{UpdateCheck: true}
)

// maxKeys is the most records one day may hold. The version comes from a
// stranger's request, so even a strict shape leaves room for millions of
// made-up ones; past this many records a new one is counted without its
// version and system, and then without its country, which is a record that
// is already there or one of a handful.
const maxKeys = 4096

// Record is one total, as Read gives it: the first five fields are its key.
type Record struct {
	Day     string `json:"day"` // "2006-01-02", in UTC
	Event   Event  `json:"event"`
	Country string `json:"country"` // two letters, or empty
	Version string `json:"version"` // the app's version, or empty
	System  string `json:"system"`  // windows, macos or linux, or empty
	Count   uint64 `json:"count"`
}

// Valid says whether a record is one that Add could have made: a day, a
// known event, a country of two capital letters or none, and a version and
// a system in their known shapes or empty. A program that takes records
// from another machine keeps only these.
func (r Record) Valid() bool {
	country := r.Country == "" || len(r.Country) == 2 && isUpper(r.Country[0]) && isUpper(r.Country[1])
	return isDay(r.Day) && slices.Contains(events, r.Event) && country &&
		(r.Version == "" || agent.KnownVersion(r.Version)) && (r.System == "" || agent.KnownSystem(r.System))
}

func isUpper(c byte) bool { return 'A' <= c && c <= 'Z' }

// key is a record's key within its day.
type key struct {
	event                    Event
	country, version, system string
}

// Tally is the totals. A nil Tally counts nothing, so a server that is not
// asked to count needs no check of its own.
type Tally struct {
	dir       string
	countries *Countries
	// now is the clock; tests replace it.
	now func() time.Time

	mu sync.Mutex
	// days holds today's totals and, until the next flush has written its
	// last minute, yesterday's.
	days  map[string]map[key]uint64
	dirty map[string]bool // days with counts the table does not have

	// flushing keeps two flushes from writing at once, and guards db.
	flushing sync.Mutex
	db       *sql.DB // nil until the file could be opened
}

// File is the name of the database in the folder of the totals.
const File = "tally.db"

// schema is the one table. A day's rows are written again each minute
// while it is today and once more after its midnight, then never.
const schema = `CREATE TABLE IF NOT EXISTS totals (
	day TEXT NOT NULL, event TEXT NOT NULL, country TEXT NOT NULL,
	version TEXT NOT NULL, system TEXT NOT NULL, count INTEGER NOT NULL,
	PRIMARY KEY (day, event, country, version, system)
) WITHOUT ROWID`

const upsert = `INSERT INTO totals (day, event, country, version, system, count) VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT DO UPDATE SET count = excluded.count`

// connect opens the database in dir. A writer waits for a reader that holds
// the file, and a reader for a writer, for a few seconds at most. The
// journal is SQLite's own default (a rollback journal), so that a reader
// with no right to write in the folder can still read.
func connect(dir string, readOnly bool) (*sql.DB, error) {
	q := "_pragma=busy_timeout(5000)"
	if readOnly {
		q += "&mode=ro"
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(dir, File), RawQuery: q}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Open returns a tally that keeps its database in dir, carrying on from
// today's totals if there are any. countries finds the country of an
// address; nil for none. Open does not fail: counting must never keep the
// site down, so a database that cannot be made or read is logged, the
// counts of the day start from zero, and each flush tries to open it again.
func Open(dir string, countries *Countries) *Tally {
	return open(dir, countries, time.Now)
}

func open(dir string, countries *Countries, now func() time.Time) *Tally {
	t := &Tally{dir: dir, countries: countries, now: now, days: map[string]map[key]uint64{}, dirty: map[string]bool{}}
	db, err := t.store()
	if err != nil {
		log.Printf("tally: %v", err)
		return t
	}
	today := day(now())
	totals := map[key]uint64{}
	err = each(db, `SELECT day, event, country, version, system, count FROM totals WHERE day = ?`, today, func(r Record) error {
		totals[key{r.Event, r.Country, r.Version, r.System}] = r.Count
		return nil
	})
	if err != nil {
		log.Printf("tally: today's totals cannot be read, starting the day from zero: %v", err)
		return t
	}
	t.days[today] = totals
	return t
}

// store returns the database, opening it first if it is not open: the
// folder and the table are made, and the day files of the time before the
// database are read in. Flush calls it with flushing held.
func (t *Tally) store() (*sql.DB, error) {
	if t.db != nil {
		return t.db, nil
	}
	if err := os.MkdirAll(t.dir, 0o755); err != nil {
		return nil, err
	}
	db, err := connect(t.dir, false)
	if err == nil {
		_, err = db.Exec(schema)
	}
	if err != nil {
		if db != nil {
			db.Close()
		}
		return nil, err
	}
	readDayFiles(db, t.dir)
	t.db = db
	return db, nil
}

// readDayFiles puts the totals of the day files into the table: until
// 2026-11 a day's totals were a file, "2026-11-17.json". A day that already
// has rows is left alone, so each file is read in once and the files stay
// where they are. A file that cannot be read is logged and passed over.
func readDayFiles(db *sql.DB, dir string) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, f := range files {
		d := filepath.Base(f[:len(f)-len(".json")])
		if !isDay(d) {
			continue
		}
		if err := readDayFile(db, f, d); err != nil {
			log.Printf("tally: %s is not read in: %v", f, err)
		}
	}
}

func readDayFile(db *sql.DB, file, day string) error {
	var held int
	if err := db.QueryRow(`SELECT count(*) FROM totals WHERE day = ?`, day).Scan(&held); err != nil || held > 0 {
		return err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var records []Record
	if err := json.Unmarshal(b, &records); err != nil {
		return err
	}
	records = slices.DeleteFunc(records, func(r Record) bool { return r.Day != day })
	return write(db, records)
}

// write puts records into the table in one step: all of them or none.
func write(db *sql.DB, records []Record) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range records {
		if _, err := tx.Exec(upsert, r.Day, r.Event, r.Country, r.Version, r.System, r.Count); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// each calls f with every record a query gives.
func each(db *sql.DB, query, arg string, f func(Record) error) error {
	rows, err := db.Query(query, arg)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.Day, &r.Event, &r.Country, &r.Version, &r.System, &r.Count); err != nil {
			return err
		}
		if err := f(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// isDay says whether s is a day as the table names it, "2026-11-17".
func isDay(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	return err == nil && t.Format("2006-01-02") == s
}

// Read writes the totals of a day and of every day after it, from the
// database in dir, one record a line as JSON. It opens the database for
// reading only and changes nothing; since must be a day, "2026-11-17".
func Read(dir, since string, w io.Writer) error {
	if !isDay(since) {
		return fmt.Errorf("%q is not a day", since)
	}
	db, err := connect(dir, true)
	if err != nil {
		return err
	}
	defer db.Close()
	enc := json.NewEncoder(w)
	return each(db, `SELECT day, event, country, version, system, count FROM totals WHERE day >= ?
		ORDER BY day, event, country, version, system`, since, func(r Record) error { return enc.Encode(r) })
}

// Add adds 1 to today's total for an event. addr is the visitor's address,
// used only to find the country and only for an event that has one; the
// zero address gives none. version and system are what the app said of
// itself: they are kept only for an event that has them and only in their
// known shape, since they come from a stranger. Add writes nothing and is
// safe from many goroutines.
func (t *Tally) Add(e Event, addr netip.Addr, version, system string) {
	if t == nil || !slices.Contains(events, e) {
		return
	}
	k := key{event: e}
	if hasCountry[e] {
		k.country = t.countries.Lookup(addr)
	}
	if hasApp[e] {
		if agent.KnownVersion(version) {
			k.version = version
		}
		if agent.KnownSystem(system) {
			k.system = system
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// The day is read with the lock held, here and in Flush, so the two
	// agree on which day is today when midnight falls between them.
	today := day(t.now())
	totals := t.days[today]
	if totals == nil {
		totals = map[key]uint64{}
		t.days[today] = totals
	}
	if _, known := totals[k]; !known && len(totals) >= maxKeys {
		k.version, k.system = "", ""
		if _, known := totals[k]; !known {
			k.country = ""
		}
	}
	totals[k]++
	t.dirty[today] = true
}

// Flush writes the days that have new counts, all in one step, so a crash
// cannot leave half of it. A day that is over is written one last time,
// with its last minute, and then forgotten: its rows do not change again.
func (t *Tally) Flush() error {
	if t == nil {
		return nil
	}
	t.flushing.Lock()
	defer t.flushing.Unlock()
	t.mu.Lock()
	today := day(t.now())
	var records []Record
	for d := range t.dirty {
		for k, n := range t.days[d] {
			records = append(records, Record{d, k.event, k.country, k.version, k.system, n})
		}
	}
	changed := t.dirty[today]
	clear(t.dirty)
	for d := range t.days {
		if d != today {
			// Forgotten even if the write below fails: the count is
			// lost rather than kept for ever.
			delete(t.days, d)
		}
	}
	t.mu.Unlock()
	if len(records) == 0 {
		return nil
	}

	db, err := t.store()
	if err == nil {
		err = write(db, records)
	}
	if err != nil && changed {
		// Today's counts are still held, so the next flush tries again.
		t.mu.Lock()
		t.dirty[today] = true
		t.mu.Unlock()
	}
	return err
}

// Run writes the totals every so often, and reads the country list again
// when its file has changed, until ctx ends; then it writes them a last
// time and returns.
func (t *Tally) Run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := t.Flush(); err != nil {
				log.Printf("tally: %v", err)
			}
			return
		case <-tick.C:
			if err := t.Flush(); err != nil {
				log.Printf("tally: %v", err)
			}
			t.countries.Refresh()
		}
	}
}
