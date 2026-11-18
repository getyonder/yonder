package tally

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"sort"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Places finds the country and the state or province of an address. What it
// knows comes from DB-IP's "IP to City Lite" list, but it never reads that
// list: BuildPlaces, run in a program of its own, makes a small file from
// it, and this reads the small file. The small file holds nothing finer
// than a state: no city and no coordinates.
//
// The small file is:
//
//	"YONDERP1"
//	four numbers of 4 bytes: the places, the bytes of their table, the IPv4
//	entries, the IPv6 entries
//	the IPv4 entries, each the first address of a range (4 bytes) and the
//	number of its place (2 bytes), in the order of the addresses
//	the IPv6 entries, the same with 16 bytes for the address
//	the table: for each place its country (2 letters), the length of its
//	state's name (1 byte) and the name; place 0 is "no place"
//
// Numbers are big-endian. A range ends where the next begins; addresses that
// are in no place have an entry of their own, with place 0. So an address is
// in the place of the last entry that starts at or before it.
//
// The file is not read into the program's memory: it is mapped (places_map.go),
// so holding it costs the program nothing that it cannot give back, and a
// new month's file never stands in memory beside the old one.
//
// A nil Places, or one whose file is missing or cannot be read, knows no
// place: Lookup gives "", "".
type Places struct {
	file string
	// read is when the file that is loaded was last changed, so Refresh
	// reads it again only when the month's new one has arrived.
	read time.Time
	said bool // that there is no file, once and not every minute

	mu   sync.RWMutex // Lookup reads list; Refresh replaces it and lets the old one go
	list *placeList
}

// placeList is one reading of the file.
type placeList struct {
	places  []place
	v4, v6  []byte // the entries, as they are in the file
	release func() // gives the file's memory back
}

type place struct{ country, region string }

const (
	placesMagic  = "YONDERP1"
	placesHeader = len(placesMagic) + 4*4
	// maxRegion is the longest name of a state that is kept, in bytes. The
	// longest in the list of 2026-11 has 50.
	maxRegion = 64
)

// LoadPlaces reads the small file. It never fails: without it the server
// still runs, and every place is empty until Refresh finds one.
func LoadPlaces(file string) *Places {
	p := &Places{file: file}
	p.Refresh()
	return p
}

// Refresh reads the file again if it has changed since it was last read. A
// file that cannot be read leaves the places as they were.
func (p *Places) Refresh() {
	if p == nil {
		return
	}
	st, err := os.Stat(p.file)
	if err != nil {
		if !p.said {
			log.Printf("tally: no list of places, countries and states are left empty: %v", err)
			p.said = true
		}
		return
	}
	if st.ModTime().Equal(p.read) {
		return
	}
	p.read = st.ModTime()
	l, err := openPlaces(p.file)
	if err != nil {
		log.Printf("tally: the list of places %s cannot be read: %v", p.file, err)
		return
	}
	p.mu.Lock()
	old := p.list
	p.list = l
	p.mu.Unlock()
	if old != nil {
		old.release()
	}
	log.Printf("tally: list of places read, %d ranges in %d places", len(l.v4)/6+len(l.v6)/18, len(l.places)-1)
}

func openPlaces(file string) (*placeList, error) {
	data, release, err := mapFile(file)
	if err != nil {
		return nil, err
	}
	l, err := parsePlaces(data)
	if err != nil {
		release()
		return nil, err
	}
	l.release = release
	return l, nil
}

// parsePlaces reads the table of places and finds the two lists of entries
// in data, which it keeps.
func parsePlaces(data []byte) (*placeList, error) {
	bad := errors.New("it is not a list of places")
	if len(data) < placesHeader || string(data[:len(placesMagic)]) != placesMagic {
		return nil, bad
	}
	var n [4]int
	for i := range n {
		n[i] = int(binary.BigEndian.Uint32(data[len(placesMagic)+4*i:]))
	}
	places, table, v4, v6 := n[0], n[1], n[2]*6, n[3]*18
	if places < 1 || places > 1<<16 || len(data) != placesHeader+table+v4+v6 {
		return nil, bad
	}
	l := &placeList{places: make([]place, 0, places)}
	rest := data[placesHeader+v4+v6:]
	for range places {
		if len(rest) < 3 || len(rest) < 3+int(rest[2]) {
			return nil, bad
		}
		pl := place{string(rest[:2]), string(rest[3 : 3+int(rest[2])])}
		rest = rest[3+int(rest[2]):]
		// Only these shapes are ever counted, whatever the file holds.
		if len(l.places) == 0 {
			pl = place{}
		} else if !isCountry(pl.country) || !isRegion(pl.region) {
			return nil, bad
		}
		l.places = append(l.places, pl)
	}
	l.v4 = data[placesHeader:][:v4]
	l.v6 = data[placesHeader+v4:][:v6]
	return l, nil
}

// isCountry reports whether s has the shape of a country code.
func isCountry(s string) bool {
	return len(s) == 2 && isUpper(s[0]) && isUpper(s[1])
}

// isRegion reports whether s can be the name of a state or province, or
// none: text of no great length with no control characters.
func isRegion(s string) bool {
	if len(s) > maxRegion || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Lookup returns the two letters of the country an address is in and the
// name of its state or province. Either can be empty.
func (p *Places) Lookup(a netip.Addr) (country, region string) {
	if p == nil || !a.IsValid() {
		return "", ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	l := p.list
	if l == nil {
		return "", ""
	}
	var n int
	if a = a.Unmap(); a.Is4() {
		b := a.As4()
		n = findPlace(l.v4, b[:])
	} else {
		b := a.As16()
		n = findPlace(l.v6, b[:])
	}
	if n >= len(l.places) {
		return "", ""
	}
	return l.places[n].country, l.places[n].region
}

// findPlace gives the number of the place of the last entry that starts at
// or before addr.
func findPlace(entries, addr []byte) int {
	step := len(addr) + 2
	i := sort.Search(len(entries)/step, func(i int) bool { return bytes.Compare(entries[i*step:][:len(addr)], addr) > 0 })
	if i == 0 {
		return 0
	}
	return int(binary.BigEndian.Uint16(entries[i*step-2:]))
}

// PlacesReport is what BuildPlaces found in the list.
type PlacesReport struct {
	Rows    int // lines that are a range
	Skipped int // lines that are not
	Places  int // different countries and states
	V4, V6  int // entries written
	Gaps    int // stretches of addresses between two ranges that no line names
	Unnamed int // ranges with a country and no state
}

func (r PlacesReport) String() string {
	return fmt.Sprintf("rows %d, passed over %d, places %d, IPv4 entries %d, IPv6 entries %d, gaps %d, rows with no state %d",
		r.Rows, r.Skipped, r.Places, r.V4, r.V6, r.Gaps, r.Unnamed)
}

// BuildPlaces makes the small file out from DB-IP's "IP to City Lite" list,
// a CSV file with a line for each range of addresses:
// "first,last,continent,country,state,city,latitude,longitude", in the order
// of the addresses, IPv4 before IPv6. The list is read as it is downloaded,
// compressed with gzip; one that is not compressed is read too. Of each line
// only the range, the country and the state are used. Ranges that follow
// each other in one place become one entry. The list is never held in
// memory: each entry is written as its line is read.
//
// out is written beside itself and then given its name, so no program ever
// reads half of it; if the list cannot be used, out is left as it was.
func BuildPlaces(list, out string) (rep PlacesReport, err error) {
	f, err := os.Open(list)
	if err != nil {
		return rep, err
	}
	defer f.Close()
	var r io.Reader = bufio.NewReader(f)
	if magic, _ := r.(*bufio.Reader).Peek(2); bytes.Equal(magic, []byte{0x1f, 0x8b}) {
		if r, err = gzip.NewReader(r); err != nil {
			return rep, err
		}
	}
	file, err := os.OpenFile(out+".new", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return rep, err
	}
	defer func() {
		if err = cmp.Or(err, file.Close()); err == nil {
			err = os.Rename(out+".new", out)
		}
		if err != nil {
			os.Remove(out + ".new")
		}
	}()
	w := bufio.NewWriter(file)
	// The numbers at the head are not known yet; they are written last.
	w.Write(make([]byte, placesHeader))

	numbers := map[place]uint16{{}: 0}
	// For each family: the address after the last range read, the place of
	// the last entry written, and how many were written.
	type family struct {
		next    netip.Addr // not valid once the last address is passed
		place   uint16
		entries int
		done    bool
	}
	fam4, fam6 := &family{next: netip.IPv4Unspecified()}, &family{next: netip.IPv6Unspecified()}
	enter := func(f *family, first netip.Addr, n uint16) {
		if f.entries > 0 && f.place == n {
			return
		}
		w.Write(first.AsSlice())
		w.Write(binary.BigEndian.AppendUint16(nil, n))
		f.place = n
		f.entries++
	}
	// finish ends a family: the addresses after its last range are in no
	// place.
	finish := func(f *family) {
		if f.next.IsValid() && !f.done {
			enter(f, f.next, 0)
		}
		f.done = true
	}

	lines := csv.NewReader(r)
	lines.FieldsPerRecord = -1
	lines.ReuseRecord = true
	for {
		part, err := lines.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			// A line that is not a range is passed over: its addresses
			// are then in no place.
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				rep.Skipped++
				continue
			}
			return rep, err
		}
		if len(part) < 5 {
			rep.Skipped++
			continue
		}
		first, err1 := netip.ParseAddr(part[0])
		last, err2 := netip.ParseAddr(part[1])
		if err1 != nil || err2 != nil || first.Is4() != last.Is4() || last.Less(first) {
			rep.Skipped++
			continue
		}
		f := fam6
		if first.Is4() {
			f = fam4
		} else {
			finish(fam4)
		}
		if f.done || !f.next.IsValid() || first.Less(f.next) {
			return rep, fmt.Errorf("the range from %s is not after the one before it", first)
		}
		rep.Rows++
		if f.next.Less(first) {
			rep.Gaps++
			enter(f, f.next, 0)
		}
		// "ZZ" is the list's word for a range that is in no country.
		var pl place
		if isCountry(part[3]) && part[3] != "ZZ" {
			pl.country = part[3]
			if isRegion(part[4]) {
				pl.region = part[4]
			}
			if pl.region == "" {
				rep.Unnamed++
			}
		}
		n, known := numbers[pl]
		if !known {
			if len(numbers) == 1<<16 {
				return rep, errors.New("it has more places than the small file can number")
			}
			n = uint16(len(numbers))
			numbers[pl] = n
		}
		enter(f, first, n)
		f.next = last.Next()
	}
	if rep.Rows == 0 {
		return rep, errors.New("it holds no ranges")
	}
	finish(fam4)
	finish(fam6)

	// The table, in the order of the numbers.
	places := make([]place, len(numbers))
	for pl, n := range numbers {
		places[n] = pl
	}
	table := 0
	for i, pl := range places {
		if i == 0 {
			pl.country = "\x00\x00"
		}
		w.WriteString(pl.country)
		w.WriteByte(byte(len(pl.region)))
		w.WriteString(pl.region)
		table += 3 + len(pl.region)
	}
	rep.Places, rep.V4, rep.V6 = len(places)-1, fam4.entries, fam6.entries
	if err := w.Flush(); err != nil {
		return rep, err
	}
	head := []byte(placesMagic)
	for _, n := range []int{len(places), table, rep.V4, rep.V6} {
		head = binary.BigEndian.AppendUint32(head, uint32(n))
	}
	_, err = file.WriteAt(head, 0)
	return rep, err
}
