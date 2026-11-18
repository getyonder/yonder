package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"yonder/internal/meeting"
	"yonder/internal/nearby"
	"yonder/internal/tally"
	"yonder/internal/usage"
)

// The names the test site answers to: apiName, and 127.0.0.1 (where the
// test server listens), as a local run does. appName is the web app's name,
// which the site does not answer to: the web app is hosted apart.
const apiName, appName = "api.yonder.example", "app.yonder.example"

// testSite is a site over a made-up build, with a limit of 1000 bytes a
// month and a counter file the test sets.
type testSite struct {
	t       *testing.T
	url     string
	counter string
	meter   *usage.Meter
	site    *site
	handler http.Handler
}

func newTestSite(t *testing.T) *testSite { return newCountingSite(t, nil) }

// newCountingSite is newTestSite with the day's totals kept in counts.
func newCountingSite(t *testing.T, counts *tally.Tally) *testSite {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "relays.json"), []byte("the list of relays"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := &testSite{t: t, counter: filepath.Join(t.TempDir(), "tx_bytes")}
	ts.sent(0)
	meter, err := usage.Open(ts.counter, filepath.Join(t.TempDir(), "usage.json"), 1000)
	if err != nil {
		t.Fatal(err)
	}
	ts.meter = meter
	s := &site{dir: dir, hosts: []string{apiName, "127.0.0.1"}, pages: []string{webAppOrigin}, point: meeting.New(meeting.DefaultLimits), meter: meter, counts: counts}
	ts.site = s
	ts.handler = s.handler()
	srv := httptest.NewServer(ts.handler)
	t.Cleanup(srv.Close)
	ts.url = srv.URL
	return ts
}

// sent sets what the server has sent in all, and has the meter read it.
func (ts *testSite) sent(n int) {
	ts.t.Helper()
	if err := os.WriteFile(ts.counter, []byte(strconv.Itoa(n)), 0o644); err != nil {
		ts.t.Fatal(err)
	}
	if ts.meter != nil {
		if err := ts.meter.Update(); err != nil {
			ts.t.Fatal(err)
		}
	}
}

func (ts *testSite) get(path string) (int, string) {
	ts.t.Helper()
	return ts.getAs("", path)
}

// getAs is get under a host name; "" is the test server's own address.
func (ts *testSite) getAs(host, path string) (int, string) {
	ts.t.Helper()
	req, err := http.NewRequest("GET", ts.url+path, nil)
	if err != nil {
		ts.t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ts.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		ts.t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func (ts *testSite) status() status {
	ts.t.Helper()
	code, body := ts.get(statusPath)
	var st status
	if err := json.Unmarshal([]byte(body), &st); code != http.StatusOK || err != nil {
		ts.t.Fatalf("status: HTTP %d, %q", code, body)
	}
	return st
}

func TestOpenBelowTheLimit(t *testing.T) {
	ts := newTestSite(t)
	ts.sent(999)
	if st := ts.status(); !st.Open || st.Until != "" {
		t.Fatalf("status %+v, want open", st)
	}
}

// At the limit the status says until when browsers are not taken at the
// meeting point (package meeting has the refusal itself). The service is
// there as always.
func TestStatusAtTheLimit(t *testing.T) {
	ts := newTestSite(t)
	ts.sent(1000)
	until := time.Now().UTC().AddDate(0, 1, -time.Now().UTC().Day()+1).Format("2 January")
	if st := ts.status(); st.Open || st.Until != until {
		t.Fatalf("status %+v, want closed until %s", st, until)
	}
	if code, body := ts.getAs(apiName, "/relays.json"); code != 200 || body != "the list of relays" {
		t.Errorf("/relays.json at the limit: HTTP %d %q", code, body)
	}
}

// Only the service's own paths answer: the meeting point, the service's two
// files, the status and the web app's word that it was opened. Nothing of
// the web app or the website is there, and a name the server was not given
// (the web app's is one) gets nothing.
func TestOnlyTheServicesPaths(t *testing.T) {
	ts := newTestSite(t)
	const none = http.StatusNotFound
	for _, c := range []struct {
		path string
		code int
	}{
		{"/relays.json", 200},
		{"/latest.json", none}, // not in this build
		{statusPath, 200},
		{openedPath, http.StatusNoContent},
		{"/", none},
		{"/index.html", none},
		{"/app.css", none},
		{"/yonder.wasm", none},
		{"/closed.html", none},
		{"/app/", none},
		{"/app/relays.json", none},
		{"//relays.json", none},
		{"/x/../relays.json", none},
		{"/privacy/", none},
		{"/donate/", none},
		// A plain request is not a websocket.
		{nearby.Path, http.StatusUpgradeRequired},
	} {
		for _, host := range []string{apiName, ""} {
			if code, _ := ts.getAs(host, c.path); code != c.code {
				t.Errorf("%s at %q: HTTP %d, want %d", c.path, host, code, c.code)
			}
		}
		for _, other := range []string{appName, "getyonder.org", "evil.example", apiName + ".evil.example"} {
			if code, _ := ts.getAs(other, c.path); code != http.StatusForbidden {
				t.Errorf("%s at %s: HTTP %d, want 403", c.path, other, code)
			}
		}
	}
	// The names are read without their port and in any case.
	if code, _ := ts.getAs(strings.ToUpper(apiName)+":443", "/relays.json"); code != 200 {
		t.Errorf("the service's name in capitals with a port: HTTP %d", code)
	}
}

// A page of the website may read the status, and nothing else may be read
// by a page from another name: the header that allows it is on the status
// only, and only for the website's origin or one given for testing.
func TestOnlyTheWebsiteReadsTheStatus(t *testing.T) {
	ts := newTestSite(t)
	const header, local = "Access-Control-Allow-Origin", "http://127.0.0.1:8392"
	ask := func(host, path, origin string) http.Header {
		t.Helper()
		req, err := http.NewRequest("GET", ts.url+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header
	}
	for _, host := range []string{apiName} {
		h := ask(host, statusPath, websiteOrigin)
		if got := h.Get(header); got != "https://getyonder.org" {
			t.Errorf("the status at %s for the website: %s is %q", host, header, got)
		}
		if h.Get("Vary") != "Origin" {
			t.Errorf("the status at %s: Vary is %q, want Origin", host, h.Get("Vary"))
		}
		for _, origin := range []string{"", "null", "https://evil.example", "http://getyonder.org", "https://getyonder.org.evil.example", "https://www.getyonder.org", "https://app.getyonder.org", local} {
			if got := ask(host, statusPath, origin).Get(header); got != "" {
				t.Errorf("the status at %s for origin %q: %s is %q, want none", host, origin, header, got)
			}
		}
		for _, path := range []string{"/", "/relays.json", "/latest.json", "/app.css", "/no-such-file", nearby.Path} {
			if got := ask(host, path, websiteOrigin).Get(header); got != "" {
				t.Errorf("%s at %s for the website: %s is %q, want none", path, host, header, got)
			}
		}
	}
	// A name the server is not known by gets no header, on the status too.
	if got := ask("evil.example", statusPath, websiteOrigin).Get(header); got != "" {
		t.Errorf("the status at a refused name: %s is %q", header, got)
	}
	// An origin given for testing (-website-origins) is allowed as well.
	ts.site.origins = []string{local}
	if got := ask(apiName, statusPath, local).Get(header); got != local {
		t.Errorf("the status for the test origin: %s is %q", header, got)
	}
}

// A connection that never finishes its request header is closed by the server.
func TestSlowHeaderIsClosed(t *testing.T) {
	srv := newServer(http.NotFoundHandler(), io.Discard)
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("a time limit is missing: %+v", srv)
	}
	srv.ReadHeaderTimeout = 50 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")) // and no end of the header
	// The read returns when the server closes the connection; the deadline
	// is only the limit for a failing test.
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("the server held the connection: %v", err)
	}
}

// Every kind of answer carries the security headers.
func TestSecurityHeaders(t *testing.T) {
	ts := newTestSite(t)
	for _, host := range []string{"", apiName} {
		for _, path := range []string{"/", "/relays.json", "/status.json", openedPath, "/no-such-file", nearby.Path} {
			req, _ := http.NewRequest("GET", ts.url+path, nil)
			if host != "" {
				req.Host = host
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			for k, want := range securityHeaders {
				if got := resp.Header.Get(k); got != want {
					t.Errorf("%s at %q: %s is %q, want %q", path, host, k, got, want)
				}
			}
			if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe-inline") {
				t.Errorf("%s at %q: policy %q", path, host, csp)
			}
		}
	}
	// A refused host name gets them too.
	req, _ := http.NewRequest("GET", ts.url, nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("refused host: HTTP %d, nosniff %q", resp.StatusCode, resp.Header.Get("X-Content-Type-Options"))
	}
}
