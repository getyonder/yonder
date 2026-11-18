// The yonder-api command is Yonder's application server (the folder made by
// packaging/api.sh): the service that the apps and the web app call. It has
// the meeting point for nearby devices and rooms (package meeting) at
// /nearby, the list of relays and the update check. It also says whether
// browsers may use the meeting point (/status.json), which the website's
// homepage asks from its own name, and it takes the web app's word that it
// was opened (/opened). Nothing else is there.
//
// The web app and the website (the homepage, the privacy and donate pages)
// are no part of it: each is a folder of plain files (packaging/webapp.sh,
// packaging/website.sh), and this program works with both switched off.
//
// It answers only to the names in -api-hosts, and for testing on this
// machine and from other devices on the same network to localhost,
// 127.0.0.1 and the names in -hosts. With -cert and -key it serves HTTPS,
// which is how it runs on Yonder's server; there -proxy is also set, so it
// learns each visitor's address from nginx.
//
// With -limit-gb it also counts what the server sends in a month (package
// usage) and, once that reaches the limit, takes no browser at the meeting
// point until the month is over: see site.open.
//
// With -tally it counts, by day, how often the web app is opened, the apps
// ask for an update and devices pair (package tally): totals only, and of a
// visitor's address no more than the country.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	// The totals are a SQLite file; this is its driver, and the reason the
	// module is in go.mod.
	_ "modernc.org/sqlite"

	"yonder/internal/address"
	"yonder/internal/meeting"
	"yonder/internal/nearby"
	"yonder/internal/proxyline"
	"yonder/internal/tally"
	"yonder/internal/tally/agent"
	"yonder/internal/usage"
)

func main() {
	var (
		dir    = flag.String("dir", "dist/api", "folder made by packaging/api.sh")
		listen = flag.String("listen", "127.0.0.1:8377", "address to listen on; 0.0.0.0:8377 lets other devices in")
		api    = flag.String("api-hosts", "", "comma-separated host names the service answers to, such as api.getyonder.org")
		hosts  = flag.String("hosts", "", "comma-separated host names and addresses it also answers to, for testing, besides localhost")
		pages  = flag.String("app-origins", "", "comma-separated origins of copies of the web app hosted elsewhere, which may use the meeting point and are counted, besides "+webAppOrigin+"; such as http://127.0.0.1:8393")
		sites  = flag.String("website-origins", "", "comma-separated origins that may read /status.json from a page, for testing, besides "+websiteOrigin+"; such as http://127.0.0.1:8392")
		cert   = flag.String("cert", "", "certificate file; with -key, serve HTTPS")
		key    = flag.String("key", "", "private key file for -cert")
		proxy  = flag.Bool("proxy", false, "every connection starts with a PROXY protocol line naming the visitor (nginx's proxy_protocol)")
		logReq = flag.Bool("log-requests", false, "log each request with the visitor's address; for checking -proxy, not for normal running")
		limit  = flag.Float64("limit-gb", 0, "take no browser at the meeting point once the server has sent this many gigabytes in a month; 0 for no limit")
		iface  = flag.String("usage-iface", "", "with -limit-gb: the network interface whose sent bytes are counted, such as enp1s0")
		count  = flag.String("usage-file", "", "with -limit-gb: the file the month's count is kept in")
		totals = flag.String("tally", "", "folder for the totals (tally.db) of the web app's starts, update checks and pairings; empty to count nothing")
		places = flag.String("tally-places", "", "with -tally: the small file of places that -places-build makes, for the country and the state of a count; without it they are left empty")
		build  = flag.String("places-build", "", "make the small file of places from DB-IP's IP to City Lite list (the .csv.gz file) and stop: -places-build <list> -tally-places <small file>")
		since  = flag.String("tally-read", "", "with -tally: write the totals of this day (2026-11-17) and every later one as JSON lines, and stop; the database is only read")
		states = flag.Bool("tally-regions", false, "with -tally-read: write the counts by state or province in the place of the totals")
	)
	flag.Parse()
	if *build != "" {
		if *places == "" {
			log.Fatal("-places-build needs -tally-places, the file to make")
		}
		rep, err := tally.BuildPlaces(*build, *places)
		if err != nil {
			log.Fatalf("the list of places %s cannot be used: %v", *build, err)
		}
		log.Printf("places: %v", rep)
		return
	}
	if *since != "" {
		if err := tally.Read(*totals, *since, *states, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if _, err := os.Stat(filepath.Join(*dir, "relays.json")); err != nil {
		log.Fatalf("%s holds no release of the service; run packaging/api.sh first", *dir)
	}
	s := &site{dir: *dir, hosts: append(append(names(*api), "localhost", "127.0.0.1"), names(*hosts)...), origins: names(*sites), pages: append([]string{webAppOrigin}, names(*pages)...), logReq: *logReq, point: meeting.New(meeting.DefaultLimits)}
	if *limit > 0 {
		if *iface == "" || *count == "" {
			log.Fatal("-limit-gb needs -usage-iface and -usage-file")
		}
		meter, err := usage.Open(usage.CounterPath(*iface), *count, uint64(*limit*usage.GB))
		if err != nil {
			log.Fatal(err)
		}
		go meter.Run(context.Background(), time.Minute)
		s.meter = meter
		log.Printf("no browser at the meeting point from %g GB a month; %.1f GB sent so far", *limit, float64(meter.Sent())/usage.GB)
	}
	if *totals != "" {
		var list *tally.Places
		if *places != "" {
			list = tally.LoadPlaces(*places)
		}
		s.counts = tally.Open(*totals, list)
		// The totals are written every minute and once more when the
		// program is told to stop.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			s.counts.Run(ctx, time.Minute)
			os.Exit(0)
		}()
		log.Printf("counting into %s", *totals)
	}
	handler := s.handler()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	if *proxy {
		ln = proxyline.Listen(ln)
	}
	if *cert != "" || *key != "" {
		log.Printf("serving %s at https://%s/ (%s)", *dir, *listen, s.served())
		log.Fatal(newServer(handler, os.Stderr).ServeTLS(ln, *cert, *key))
	}
	log.Printf("serving %s at http://%s/ (%s)", *dir, *listen, s.served())
	log.Fatal(newServer(handler, os.Stderr).Serve(ln))
}

// names is the host names in a comma-separated list, in lower case.
func names(list string) []string {
	var out []string
	for h := range strings.SplitSeq(list, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, strings.ToLower(h))
		}
	}
	return out
}

// newServer is the http.Server both ways of serving use, with time limits so
// a connection that sends nothing, or very little, is let go. The meeting
// point's websocket is not cut off by them: it takes the connection over
// from the server, which clears the deadlines. What the server itself has
// to say (a handshake that failed, say) goes to errors without the
// visitor's address: see noAddress.
func newServer(h http.Handler, errors io.Writer) *http.Server {
	return &http.Server{
		Handler:           h,
		ErrorLog:          log.New(noAddress{log.New(errors, "", log.LstdFlags)}, "", 0),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second, // requests here have no body
		// The answers are small.
		WriteTimeout: time.Minute,
		IdleTimeout:  2 * time.Minute,
	}
}

// noAddress writes log lines with every address in them taken out, so the
// server's journal holds no visitor's address: Go's server names the other
// end in its own messages ("TLS handshake error from 1.2.3.4:5678: EOF").
// This is the one place that rule is kept. The only line that may carry an
// address is the one -log-requests asks for, which is off unless someone
// is checking -proxy by hand.
//
// It takes whole lines and hands them on to out, which puts the time before
// them: the time is added after the addresses are taken out, since a time
// looks much like a short IPv6 address.
type noAddress struct{ out *log.Logger }

// addresses matches an IPv4 address, an IPv6 address in brackets (as it is
// written before a port), and one that stands alone, each with its port.
var addresses = regexp.MustCompile(`\[[0-9A-Fa-f:.%\w]*\](:\d+)?|\b\d{1,3}(\.\d{1,3}){3}(:\d+)?\b|[0-9A-Fa-f]{0,4}(:[0-9A-Fa-f]{0,4}){2,7}`)

func (n noAddress) Write(line []byte) (int, error) {
	n.out.Print(string(addresses.ReplaceAll(line, []byte("(address)"))))
	return len(line), nil
}

// site is the application server: the service the apps and the web app
// call.
type site struct {
	// dir is the folder made by packaging/api.sh: the service's files.
	dir string
	// hosts is the host names it answers to.
	hosts []string
	// origins is the origins, besides the website's own, whose pages may
	// read the status: see readableBy.
	origins []string
	// pages is the origins of the web app's pages, which are hosted
	// apart: they may use the meeting point and say that they were
	// opened (openedPath).
	pages  []string
	logReq bool
	point  *meeting.Point
	// meter is the month's count of what the server has sent; nil when
	// there is no limit.
	meter *usage.Meter
	// counts is the day's totals; nil when nothing is counted.
	counts *tally.Tally
}

// pairings is what each thing the meeting point reports is counted as.
var pairings = map[meeting.Event]tally.Event{
	meeting.RoomOpened:      tally.RoomOpened,
	meeting.CodeStarted:     tally.CodeStarted,
	meeting.CodeCompleted:   tally.CodeCompleted,
	meeting.NearbyStarted:   tally.NearbyStarted,
	meeting.NearbyCompleted: tally.NearbyCompleted,
}

// served is the host names it answers to, for the log.
func (s *site) served() string {
	return "the service: " + strings.Join(s.hosts, ", ")
}

// known reports whether a host name is one this server answers to.
func (s *site) known(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return slices.Contains(s.hosts, strings.ToLower(host))
}

// latestPath is where an app asks whether there is an update, and
// serviceFiles the files the service answers with, from the top of dir.
const latestPath = "/latest.json"

var serviceFiles = []string{"/relays.json", latestPath}

// count adds a request to the day's totals, if it is one that is counted:
// the web app saying that it was opened, and an app asking whether there is
// an update, whether or not the answer is there. The address goes to the
// tally for the country and is kept nowhere.
func (s *site) count(r *http.Request) {
	if s.counts == nil || r.Method != http.MethodGet {
		return
	}
	var e tally.Event
	switch {
	case r.URL.Path == openedPath && s.fromApp(r):
		e = tally.WebApp
	case r.URL.Path == latestPath:
		e = tally.UpdateCheck
	default:
		return
	}
	var addr netip.Addr
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		addr = ap.Addr()
	}
	version, system := agent.Parse(r.UserAgent())
	s.counts.Add(e, addr, version, system)
}

// openedPath is what the web app asks for once, when its page starts, so
// that the day's totals have how often the web app was opened (and from
// which country) though this program does not serve its page. Only a request from one of the web app's origins is counted,
// and the answer is empty.
const openedPath = "/opened"

// webAppOrigin is the web app's own origin.
var webAppOrigin = strings.TrimSuffix(address.WebApp, "/")

// fromApp reports whether a request comes from a page of the web app.
func (s *site) fromApp(r *http.Request) bool {
	return slices.Contains(s.pages, strings.ToLower(r.Header.Get("Origin")))
}

// statusPath is where the service says whether browsers may use the meeting
// point. The website's homepage asks, to say when they may not
// (website/home.js).
const statusPath = "/status.json"

// websiteOrigin is the website as a browser names it in a request's Origin
// header: the one origin, besides those given for testing, whose pages may
// read the status.
var websiteOrigin = strings.TrimSuffix(address.Website, "/")

// readableBy lets a page of the website read the status, which a browser
// allows a page from another name only if the answer says so. The header
// goes out on the status alone, and only to a request that names the
// website (or an origin given for testing) as where it comes from: it
// repeats that one origin, never "*", so no other site's page can read
// anything here. The answer differs by Origin, which Vary tells a cache.
func (s *site) readableBy(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Origin")
	if o := r.Header.Get("Origin"); o == websiteOrigin || slices.Contains(s.origins, strings.ToLower(o)) {
		w.Header().Set("Access-Control-Allow-Origin", o)
	}
}

// status is what statusPath answers with.
type status struct {
	Open bool `json:"open"`
	// Until is the day browsers may use the meeting point again, as
	// "1 December"; empty while they may.
	Until string `json:"until,omitempty"`
}

// open reports whether browsers may use the meeting point: they may, until
// the server has sent the month's limit. This is the one place that
// decides; the status and the meeting point both ask here.
func (s *site) open() bool { return s.meter == nil || !s.meter.Over() }

func (s *site) status() status {
	if s.open() {
		return status{Open: true}
	}
	return status{Until: s.meter.Resets().Format("2 January")}
}

// securityHeaders go out on every response. The service has no page, so
// the policy allows a browser that opens an answer as one to load nothing.
// The web app's own policy is in server/nginx.conf.
var securityHeaders = map[string]string{
	"Content-Security-Policy":   "default-src 'none'; frame-ancestors 'none'",
	"Strict-Transport-Security": "max-age=31536000",
	"X-Content-Type-Options":    "nosniff",
	"Referrer-Policy":           "no-referrer",
}

func (s *site) handler() http.Handler {
	s.point.Browsers = s.open
	s.point.Origins = s.pages
	if s.counts != nil {
		s.point.Count = func(e meeting.Event) { s.counts.Add(pairings[e], netip.Addr{}, "", "") }
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range securityHeaders {
			w.Header().Set(k, v)
		}
		if !s.known(r.Host) {
			http.Error(w, "this server is not known by that name", http.StatusForbidden)
			return
		}
		if s.logReq {
			log.Printf("%s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		}
		s.count(r)
		if r.URL.Path == nearby.Path {
			s.point.ServeHTTP(w, r)
			return
		}
		// Always ask whether a file has changed, so a release is picked up.
		w.Header().Set("Cache-Control", "no-cache")
		switch {
		case slices.Contains(serviceFiles, r.URL.Path):
			http.ServeFile(w, r, filepath.Join(s.dir, r.URL.Path))
		case r.URL.Path == openedPath:
			// Counted above. The page may see that it was answered, and
			// no more.
			w.Header().Add("Vary", "Origin")
			if s.fromApp(r) {
				w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == statusPath:
			s.readableBy(w, r)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s.status())
		default:
			// The service has nothing else.
			http.NotFound(w, r)
		}
	})
}
