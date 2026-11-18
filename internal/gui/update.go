package gui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"yonder/internal/address"
	"yonder/internal/tally/agent"
)

// The notice that a newer Yonder is out. Yonder asks its site for one small
// file, which says the newest version and the page to get it from, and
// compares that with its own. It only tells: the person downloads and
// installs the new version as they did the first. The request says which
// version of Yonder asks and on which system (windows, macos or linux) and
// nothing else of this device, and none is made while the person has the
// setting off.

const (
	// updateFirst is how long after starting the site is first asked, and
	// updateEvery how long after each time it was.
	updateFirst = 10 * time.Second
	updateEvery = 24 * time.Hour
)

// latestURL is where the service says which version is the newest, and
// downloadPage where a newer one is found if the file names no page.
const (
	latestURL    = "https://" + address.Service + "/latest.json"
	downloadPage = address.Website + "#download"
)

// Update is the notice that a newer version is out.
type Update struct {
	// Version is the newest version, and Have this one.
	Version string `json:"version"`
	Have    string `json:"have"`
}

// update is the notice to show, if any: the newest version known, when it
// is newer than this one and than the one the person put away. s.mu must be
// held.
func (s *Service) update() *Update {
	if s.set.NoUpdateCheck || !newer(s.latest, s.version) || !newer(s.latest, s.set.SkipVersion) {
		return nil
	}
	// The notice that this version is withdrawn says which one to get.
	if s.withdrawnNotice() != nil {
		return nil
	}
	return &Update{Version: s.latest, Have: s.version}
}

// watchUpdates asks the site which version is the newest, soon after the
// start and then once a day, until ctx ends.
func (s *Service) watchUpdates(ctx context.Context) {
	for {
		s.mu.Lock()
		wait := updateFirst
		if !s.updateAt.IsZero() {
			wait = max(updateEvery-time.Since(s.updateAt), 0)
		}
		off := s.set.NoUpdateCheck
		s.mu.Unlock()
		var due <-chan time.Time
		if !off {
			due = time.After(wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-s.updateKick:
			// The setting changed: work out again whether to ask, and when.
			continue
		case <-due:
		}
		s.checkUpdate(ctx)
	}
}

// checkUpdate asks the site once, unless the person has the setting off,
// and shows what it says. A failure is not told, and is not tried again
// until tomorrow.
func (s *Service) checkUpdate(ctx context.Context) {
	s.mu.Lock()
	off, url, own := s.set.NoUpdateCheck, s.updateURL, s.version
	s.mu.Unlock()
	if off {
		return
	}
	file, err := latest(ctx, url, agent.Name(own, runtime.GOOS))
	s.mu.Lock()
	s.updateAt = time.Now()
	if err == nil {
		s.latest, s.latestPage = file.Version, file.Page
		// The list of withdrawn versions, if the file has one that holds;
		// it is kept for the next start. See withdrawn.go.
		if s.takeList(file) {
			saveKept(s.dir, file)
		}
	}
	s.mu.Unlock()
	if err == nil {
		s.changed()
	}
}

// latest reads the site's file at url: the newest version, the page to get
// it from, and the list of withdrawn versions if it has one. asks is the
// User-Agent it asks with.
func latest(ctx context.Context, url, asks string) (file siteFile, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return siteFile{}, err
	}
	req.Header.Set("User-Agent", asks)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return siteFile{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return siteFile{}, errors.New("update check: " + resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&file); err != nil {
		return siteFile{}, err
	}
	// The version is shown to the person, and nothing signs it: only three
	// numbers are a version.
	if !agent.KnownVersion(file.Version) {
		file.Version = ""
	}
	// The page is opened in the person's browser, so it must be one on
	// Yonder's own site, whatever the file says.
	if !ownPage(file.Page) {
		file.Page = downloadPage
	}
	return file, nil
}

// ownPage reports whether page is on Yonder's own site: a plain https
// address with the site's host, no user name and no port.
func ownPage(page string) bool {
	site, err := url.Parse(address.Website)
	if err != nil {
		return false
	}
	u, err := url.Parse(page)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.User == nil && u.Port() == "" && strings.EqualFold(u.Hostname(), site.Hostname())
}

// newer reports whether version a is newer than b, both written as numbers
// with dots between ("0.1.8"). A version that cannot be read is never
// newer, and any that can is newer than one that cannot.
func newer(a, b string) bool {
	x, ok := numbers(a)
	if !ok {
		return false
	}
	y, _ := numbers(b)
	for i := range max(len(x), len(y)) {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p > q
		}
	}
	return len(y) == 0
}

// numbers is the parts of a version; false if it is not numbers and dots.
func numbers(v string) ([]int, bool) {
	var out []int
	for _, part := range strings.Split(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// GetUpdate is the button of the notice that a newer version is out, and
// of the notice that this one is withdrawn: it opens the page the newer
// version is on.
func (s *Service) GetUpdate() error {
	s.mu.Lock()
	page := s.latestPage
	s.mu.Unlock()
	if page == "" {
		page = downloadPage
	}
	return s.shell.Browse(page)
}

// HideUpdate is "Not now": the notice goes until a version newer than this
// one is out.
func (s *Service) HideUpdate() error {
	s.mu.Lock()
	s.set.SkipVersion = s.latest
	err := s.set.Save(s.dir)
	s.mu.Unlock()
	s.changed()
	return err
}

// SetUpdateCheck turns asking the site for the newest version on or off.
// Off means no request at all.
func (s *Service) SetUpdateCheck(on bool) error {
	s.mu.Lock()
	if on == !s.set.NoUpdateCheck {
		s.mu.Unlock()
		return nil
	}
	s.set.NoUpdateCheck = !on
	err := s.set.Save(s.dir)
	s.mu.Unlock()
	select {
	case s.updateKick <- struct{}{}:
	default:
	}
	s.changed()
	return err
}
