package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"yonder/internal/nearby"
	"yonder/internal/proxyline"
	"yonder/internal/tally"
)

// wait is the longest any step of these tests may take; nothing sleeps.
const wait = 10 * time.Second

// cities is a made-up list of places, in the form of DB-IP's file.
const cities = "203.0.113.0,203.0.113.255,OC,NZ,Auckland,Auckland,-36.8485,174.763\n2001:db8::,2001:db8::ffff,AS,JP,Tokyo,Tokyo,35.6895,139.692\n"

// counting is a site that counts, behind a listener that takes each
// visitor's address from a PROXY line as on Yonder's server, so a test can
// arrive from anywhere.
type counting struct {
	*testSite
	dir    string // the folder of the totals
	counts *tally.Tally
	addr   string // where the site listens
}

func newCounting(t *testing.T) *counting {
	t.Helper()
	list := filepath.Join(t.TempDir(), "city.csv")
	if err := os.WriteFile(list, []byte(cities), 0o644); err != nil {
		t.Fatal(err)
	}
	places := filepath.Join(t.TempDir(), "places")
	if _, err := tally.BuildPlaces(list, places); err != nil {
		t.Fatal(err)
	}
	c := &counting{dir: filepath.Join(t.TempDir(), "tally")}
	c.counts = tally.Open(c.dir, tally.LoadPlaces(places))
	c.testSite = newCountingSite(t, c.counts)
	srv := httptest.NewUnstartedServer(c.handler)
	srv.Listener = proxyline.Listen(srv.Listener)
	srv.Start()
	t.Cleanup(srv.Close)
	c.addr = srv.Listener.Addr().String()
	return c
}

// proxyLine is the line nginx puts before a visitor's connection.
func proxyLine(from string) string {
	family := "TCP4"
	if strings.Contains(from, ":") {
		family = "TCP6"
	}
	return fmt.Sprintf("PROXY %s %s %s 51234 443\r\n", family, from, from)
}

// visitor is an HTTP client whose connections arrive as from an address.
func (c *counting) visitor(from string) *http.Client {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, c.addr)
			if err != nil {
				return nil, err
			}
			if _, err := conn.Write([]byte(proxyLine(from))); err != nil {
				conn.Close()
				return nil, err
			}
			return conn, nil
		},
	}}
	c.t.Cleanup(client.CloseIdleConnections)
	return client
}

// ask makes one request as from an address and returns the status.
func (c *counting) ask(from, method, path, agent string) int {
	c.t.Helper()
	req, err := http.NewRequest(method, "http://"+c.addr+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("User-Agent", agent)
	resp, err := c.visitor(from).Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// totals writes the totals and returns the one file they are in, as it is,
// and what the read command gives of it, as "event country version system"
// with its count.
func (c *counting) totals() ([]byte, map[string]uint64) {
	c.t.Helper()
	if err := c.counts.Flush(); err != nil {
		c.t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(c.dir, "*"))
	if err != nil || len(files) != 1 || filepath.Base(files[0]) != tally.File {
		c.t.Fatalf("the folder of totals holds %v (%v), want the database alone", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		c.t.Fatal(err)
	}
	return b, c.read(false)
}

// read is what the read command gives: the totals, each as "event country
// version system", or the counts by state, each as "event country region".
func (c *counting) read(regions bool) map[string]uint64 {
	c.t.Helper()
	var lines bytes.Buffer
	if err := tally.Read(c.dir, "2000-01-01", regions, &lines); err != nil {
		c.t.Fatal(err)
	}
	out := map[string]uint64{}
	for dec := json.NewDecoder(&lines); dec.More(); {
		var r tally.Record
		if err := dec.Decode(&r); err != nil {
			c.t.Fatal(err)
		}
		if regions {
			out[fmt.Sprintf("%s %s %s", r.Event, r.Country, r.Region)] += r.Count
		} else {
			out[strings.TrimSpace(fmt.Sprintf("%s %s %s %s", r.Event, r.Country, r.Version, r.System))] += r.Count
		}
	}
	return out
}

// The requests that are counted, each with the fields its event has, and
// those that are not; and no address in what is written. (The web app's
// start has a test of its own below.)
func TestCounts(t *testing.T) {
	c := newCounting(t)
	const nz, jp, nowhere = "203.0.113.7", "2001:db8::1234", "198.51.100.9"
	const browser = "Mozilla/5.0 (X11; Linux x86_64)"
	for _, r := range []struct {
		from, method, path, agent string
		status                    int
	}{
		// latest.json is not in this build: the check is counted all the same.
		{nz, "GET", "/latest.json", "Yonder/0.1.27 (linux)", 404},
		{nz, "GET", "/latest.json", "Yonder/0.1.27 (linux)", 404},
		{jp, "GET", "/latest.json", "Yonder/0.1.28 (windows)", 404},
		{nz, "GET", "/latest.json", "Go-http-client/1.1", 404}, // an app from before the header
		{nowhere, "GET", "/latest.json", "Yonder/<script> (plan9)", 404},
		// Not counted.
		{nz, "HEAD", "/latest.json", "Yonder/0.1.27 (linux)", 404},
		{nz, "GET", "/", browser, 404},
		{nz, "GET", "/relays.json", browser, 200},
		{nz, "GET", statusPath, browser, 200},
		{nz, "GET", "/nothing", browser, 404},
	} {
		if got := c.ask(r.from, r.method, r.path, r.agent); got != r.status {
			t.Fatalf("%s %s: HTTP %d, want %d", r.method, r.path, got, r.status)
		}
	}
	// A request under a name the site does not answer to is not counted.
	for _, host := range []string{"elsewhere.example", appName} {
		req, _ := http.NewRequest("GET", "http://"+c.addr+"/latest.json", nil)
		req.Host = host
		resp, err := c.visitor(nz).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("/latest.json at %s: HTTP %d, want 403", host, resp.StatusCode)
		}
	}

	file, got := c.totals()
	want := map[string]uint64{
		"update-check NZ 0.1.27 linux":   2,
		"update-check JP 0.1.28 windows": 1,
		"update-check NZ":                1,
		"update-check":                   1,
	}
	if len(got) != len(want) {
		t.Fatalf("counted %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Fatalf("counted %v, want %v", got, want)
		}
	}
	// By state the checks are counted once more, with nothing of the app.
	states := c.read(true)
	if len(states) != 2 || states["update-check NZ Auckland"] != 3 || states["update-check JP Tokyo"] != 1 {
		t.Fatalf("counted by state %v, want 3 in Auckland and 1 in Tokyo", states)
	}
	// No address is kept, and nothing finer than a state.
	for _, a := range []string{nz, jp, nowhere, "203.0.113", "2001:db8", "198.51.100", "51234", "36.8485", "174.763", "139.692"} {
		if strings.Contains(string(file), a) {
			t.Fatalf("the totals hold %s:\n%s", a, file)
		}
	}
}

// The web app says once that it was opened, and that is counted with its
// country: but only when the request comes from a page of the web app, and
// the address is not kept.
func TestWebAppOpenedIsCounted(t *testing.T) {
	c := newCounting(t)
	ask := func(origin string) *http.Response {
		t.Helper()
		req, err := http.NewRequest("GET", "http://"+c.addr+openedPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := c.visitor("203.0.113.7").Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	const allow = "Access-Control-Allow-Origin"
	if resp := ask(webAppOrigin); resp.StatusCode != http.StatusNoContent || resp.Header.Get(allow) != webAppOrigin {
		t.Fatalf("from the web app: HTTP %d, %s %q", resp.StatusCode, allow, resp.Header.Get(allow))
	}
	for _, origin := range []string{"", "https://getyonder.org", "https://app.getyonder.org.example", "null"} {
		if resp := ask(origin); resp.Header.Get(allow) != "" {
			t.Errorf("from %q: %s %q, want none", origin, allow, resp.Header.Get(allow))
		}
	}
	file, got := c.totals()
	if len(got) != 1 || got["web-app NZ"] != 1 {
		t.Fatalf("counted %v, want the web app once", got)
	}
	if strings.Contains(string(file), "203.0.113") {
		t.Fatalf("the totals hold the address:\n%s", file)
	}
}

// The meeting point takes a browser's page only from the web app's origin
// (or its own host): a page of any other site is refused, and an app, which
// names no origin, is let in as before.
func TestMeetingPointTakesOnlyTheWebAppsPages(t *testing.T) {
	c := newCounting(t)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	dial := func(origin string) error {
		opts := &websocket.DialOptions{HTTPClient: c.visitor("203.0.113.7")}
		if origin != "" {
			opts.HTTPHeader = http.Header{"Origin": {origin}}
		}
		conn, _, err := websocket.Dial(ctx, "ws://"+c.addr+nearby.Path, opts)
		if err == nil {
			conn.CloseNow()
		}
		return err
	}
	for _, origin := range []string{"", webAppOrigin} {
		if err := dial(origin); err != nil {
			t.Errorf("origin %q: %v, want it let in", origin, err)
		}
	}
	for _, origin := range []string{"https://getyonder.org", "https://evil.example", "http://app.getyonder.org", "https://app.getyonder.org.evil.example"} {
		if err := dial(origin); err == nil {
			t.Errorf("origin %q was let in to the meeting point", origin)
		}
	}
}

// What the meeting point reports is counted: here, a room that is opened
// through the site.
func TestPairingsAreCounted(t *testing.T) {
	c := newCounting(t)
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+c.addr+nearby.Path, &websocket.DialOptions{HTTPClient: c.visitor("203.0.113.7")})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	hello, _ := json.Marshal(nearby.Message{Type: nearby.TypeHello, Room: nearby.RoomNew, Peer: &nearby.Peer{
		Name: "A", Kind: nearby.KindBrowser, Salt: strings.Repeat("0a", 16), Tag: strings.Repeat("1b", 32),
	}})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatal(err)
	}
	// The welcome says the room is open, and it is counted before that.
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	file, got := c.totals()
	if len(got) != 1 || got["room-opened"] != 1 {
		t.Fatalf("counted %v, want one room opened", got)
	}
	if strings.Contains(string(file), "203.0.113") {
		t.Fatalf("the totals hold the address:\n%s", file)
	}
	for e, as := range pairings {
		if as == "" {
			t.Errorf("the meeting point's event %d is counted as nothing", e)
		}
	}
	if len(pairings) != 5 {
		t.Errorf("%d of the meeting point's 5 events are counted", len(pairings))
	}
}

// With no folder for the totals nothing is counted and nothing is written.
func TestNotCounting(t *testing.T) {
	ts := newTestSite(t)
	if code, _ := ts.get(openedPath); code != http.StatusNoContent {
		t.Fatalf("the web app's start: HTTP %d", code)
	}
	if code, _ := ts.get("/latest.json"); code != http.StatusNotFound {
		t.Fatalf("latest.json: HTTP %d", code)
	}
}

// lines hands on each line a logger writes.
type lines chan string

func (l lines) Write(b []byte) (int, error) {
	l <- string(b)
	return len(b), nil
}

// What the server itself logs of a visitor's connection has no address in
// it: here a connection that is not TLS, from an IPv4 and an IPv6 address.
func TestErrorLogHasNoAddress(t *testing.T) {
	logged := make(lines, 16)
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config = newServer(http.NotFoundHandler(), logged)
	srv.Listener = proxyline.Listen(srv.Listener)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	for _, from := range []string{"203.0.113.7", "2001:db8::1234"} {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte(proxyLine(from) + "GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		select {
		case line := <-logged:
			if !strings.Contains(line, "TLS handshake error") {
				t.Fatalf("expected the server to log the failed handshake, got %q", line)
			}
			for _, a := range []string{from, "203.0.113", "2001:db8", "51234", "127.0.0.1"} {
				if strings.Contains(line, a) {
					t.Fatalf("the log holds the address %s: %q", a, line)
				}
			}
		case <-time.After(wait):
			t.Fatal("the server logged nothing of a failed handshake")
		}
	}
	// A real visitor is still served.
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if _, err := conn.Write([]byte(proxyLine("203.0.113.7"))); err != nil {
				return nil, err
			}
			tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
			return tc, tc.HandshakeContext(ctx)
		}}}
	defer client.CloseIdleConnections()
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// Addresses in every form a log line may carry them are taken out, and the
// rest of the line is left.
func TestNoAddress(t *testing.T) {
	for line, want := range map[string]string{
		"http: TLS handshake error from 203.0.113.7:51234: EOF":                                "http: TLS handshake error from (address): EOF",
		"http: TLS handshake error from [2001:db8::1234]:51234: EOF":                           "http: TLS handshake error from (address): EOF",
		"read tcp 127.0.0.1:8444->198.51.100.9:443: i/o timeout":                               "read tcp (address)->(address): i/o timeout",
		"http: panic serving [fe80::1%eth0]:1: oops":                                           "http: panic serving (address): oops",
		"from 2001:db8::1234 and 2001:db8:0:1:2:3:4:5 and ::1 and fe80:: and 198.51.100.9 too": "from (address) and (address) and (address) and (address) and (address) too",
		"http: Accept error: too many open files; retrying in 5ms":                             "http: Accept error: too many open files; retrying in 5ms",
	} {
		logged := make(lines, 1)
		newServer(nil, logged).ErrorLog.Print(line)
		got := <-logged
		// The time the line was logged at comes first and is left as it is.
		if !strings.HasSuffix(got, " "+want+"\n") {
			t.Errorf("%q was logged as %q, want it to end %q", line, got, want)
		}
	}
}
