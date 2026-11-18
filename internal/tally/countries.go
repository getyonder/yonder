package tally

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Countries finds the country of an address in DB-IP's "IP to Country Lite"
// list: a CSV file with a line for each range of addresses, "first,last,CC",
// IPv4 and IPv6 together. The file is read as it is downloaded, compressed
// with gzip; one that is not compressed is read too. The list is held in
// memory, sorted, and searched; nothing is asked of another service.
//
// A nil Countries, or one whose file is missing or cannot be read, knows no
// country: Lookup gives "".
type Countries struct {
	file string
	// read is when the file that is loaded was last changed, so Refresh
	// reads it again only when the month's new one has arrived.
	read  time.Time
	lists atomic.Pointer[lists]
}

// lists is one reading of the file. IPv4 ranges are kept as numbers, which
// is a third of the size.
type lists struct {
	v4 []range4
	v6 []range6
}

type range4 struct {
	first, last uint32
	country     [2]byte
}

type range6 struct {
	first, last [16]byte
	country     [2]byte
}

// LoadCountries reads the list in file. It never fails: without a list the
// server still runs, and every country is empty until Refresh finds one.
func LoadCountries(file string) *Countries {
	c := &Countries{file: file}
	c.Refresh()
	return c
}

// Refresh reads the file again if it has changed since it was last read. A
// file that cannot be read leaves the list as it was.
func (c *Countries) Refresh() {
	if c == nil {
		return
	}
	st, err := os.Stat(c.file)
	if err != nil {
		if c.read.IsZero() && c.lists.Load() == nil {
			log.Printf("tally: no country list, countries are left empty: %v", err)
			c.lists.Store(&lists{}) // said once, not every minute
		}
		return
	}
	if st.ModTime().Equal(c.read) {
		return
	}
	c.read = st.ModTime()
	l, err := readLists(c.file)
	if err != nil {
		log.Printf("tally: the country list %s cannot be read: %v", c.file, err)
		return
	}
	c.lists.Store(l)
	log.Printf("tally: country list read, %d ranges", len(l.v4)+len(l.v6))
}

func readLists(file string) (*lists, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = bufio.NewReader(f)
	if magic, _ := r.(*bufio.Reader).Peek(2); bytes.Equal(magic, []byte{0x1f, 0x8b}) {
		if r, err = gzip.NewReader(r); err != nil {
			return nil, err
		}
	}
	l := &lists{}
	lines := bufio.NewScanner(r)
	for lines.Scan() {
		// A line that is not a range is passed over: one bad line is
		// no reason to lose the rest.
		part := strings.Split(strings.TrimSpace(lines.Text()), ",")
		// "ZZ" is the list's word for a range that is in no country.
		if len(part) != 3 || !isCountry(part[2]) || part[2] == "ZZ" {
			continue
		}
		first, err1 := netip.ParseAddr(part[0])
		last, err2 := netip.ParseAddr(part[1])
		if err1 != nil || err2 != nil || first.Is4() != last.Is4() || last.Less(first) {
			continue
		}
		country := [2]byte{part[2][0], part[2][1]}
		if first.Is4() {
			a, b := first.As4(), last.As4()
			l.v4 = append(l.v4, range4{binary.BigEndian.Uint32(a[:]), binary.BigEndian.Uint32(b[:]), country})
		} else {
			l.v6 = append(l.v6, range6{first.As16(), last.As16(), country})
		}
	}
	if err := lines.Err(); err != nil {
		return nil, err
	}
	if len(l.v4)+len(l.v6) == 0 {
		return nil, errors.New("it holds no ranges")
	}
	l.v4 = slices.Clip(l.v4)
	l.v6 = slices.Clip(l.v6)
	slices.SortFunc(l.v4, func(a, b range4) int { return cmp.Compare(a.first, b.first) })
	slices.SortFunc(l.v6, func(a, b range6) int { return bytes.Compare(a.first[:], b.first[:]) })
	return l, nil
}

// isCountry reports whether s has the shape of a country code. Only this
// shape is ever counted, whatever the file holds.
func isCountry(s string) bool {
	return len(s) == 2 && s[0] >= 'A' && s[0] <= 'Z' && s[1] >= 'A' && s[1] <= 'Z'
}

// Lookup returns the two letters of the country an address is in, or "" if
// no range holds it.
func (c *Countries) Lookup(a netip.Addr) string {
	if c == nil || !a.IsValid() {
		return ""
	}
	l := c.lists.Load()
	if l == nil {
		return ""
	}
	// The range that could hold the address is the last one that starts
	// at or before it.
	if a = a.Unmap(); a.Is4() {
		b := a.As4()
		n := binary.BigEndian.Uint32(b[:])
		i := sort.Search(len(l.v4), func(i int) bool { return l.v4[i].first > n })
		if i == 0 || l.v4[i-1].last < n {
			return ""
		}
		return string(l.v4[i-1].country[:])
	}
	b := a.As16()
	i := sort.Search(len(l.v6), func(i int) bool { return bytes.Compare(l.v6[i].first[:], b[:]) > 0 })
	if i == 0 || bytes.Compare(l.v6[i-1].last[:], b[:]) < 0 {
		return ""
	}
	return string(l.v6[i-1].country[:])
}
