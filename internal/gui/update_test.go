package gui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"

	"yonder/internal/settings"
	"yonder/internal/tally/agent"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.1.8", "0.1.7", true},
		{"0.1.7", "0.1.7", false},
		{"0.1.6", "0.1.7", false},
		{"0.1.10", "0.1.9", true},
		{"0.2", "0.1.9", true},
		{"0.1.7.1", "0.1.7", true},
		{"0.1.7", "0.1.7.0", false},
		{"1.0.0", "0.9.9", true},
		{"0.1.8", "", true},
		{"", "0.1.7", false},
		{"0.1.8-beta", "0.1.7", false},
		{"latest", "0.1.7", false},
	} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// site serves body as the site's file about the newest version, and counts
// the times it was asked.
func site(t *testing.T, body string) (url string, asked *atomic.Int32) {
	t.Helper()
	asked = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, asked
}

func TestUpdateNoticeAndNotNow(t *testing.T) {
	url, _ := site(t, `{"version":"0.1.8","page":"https://getyonder.org/get"}`)
	s, w, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url})
	if st := s.State(); st.Update != nil || !st.UpdateCheck {
		t.Fatalf("before asking: update %+v, check %v", st.Update, st.UpdateCheck)
	}
	s.checkUpdate(context.Background())
	st := w.until(t, "the notice", func(st State) bool { return st.Update != nil })
	if *st.Update != (Update{Version: "0.1.8", Have: "0.1.7"}) {
		t.Fatalf("notice: %+v", *st.Update)
	}
	if s.latestPage != "https://getyonder.org/get" {
		t.Fatalf("page: %q", s.latestPage)
	}

	// "Not now" puts this version away for good, and a newer one shows.
	if err := s.HideUpdate(); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.Update != nil {
		t.Fatalf("after Not now: %+v", *st.Update)
	}
	if set, err := settings.Load(s.dir); err != nil || set.SkipVersion != "0.1.8" {
		t.Fatalf("saved: %+v, %v", set, err)
	}
	s.mu.Lock()
	s.latest = "0.1.9"
	s.mu.Unlock()
	if st := s.State(); st.Update == nil || st.Update.Version != "0.1.9" {
		t.Fatalf("a newer version after Not now: %+v", st.Update)
	}
}

func TestNoNoticeForTheSameOrAnOlderVersion(t *testing.T) {
	for _, body := range []string{`{"version":"0.1.7"}`, `{"version":"0.1.6"}`, `{"version":"soon"}`, `{"version":"0.1.8.9.9.9"}`, `{"version":"0.2"}`, `{"version":"+0.1.9"}`, `not json`} {
		url, asked := site(t, body)
		s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url})
		s.checkUpdate(context.Background())
		if asked.Load() != 1 {
			t.Fatalf("%s: asked %d times", body, asked.Load())
		}
		if st := s.State(); st.Update != nil {
			t.Fatalf("%s: notice %+v", body, *st.Update)
		}
	}
}

// The request names the version and the system as the server reads them,
// and carries nothing else: the only other header is the one Go's own
// transport adds.
func TestUpdateCheckNamesVersionAndSystem(t *testing.T) {
	system := map[string]string{"windows": "windows", "darwin": "macos", "linux": "linux"}[runtime.GOOS]
	if system == "" {
		t.Skip("not a system Yonder's app is built for: " + runtime.GOOS)
	}
	for have, want := range map[string]string{"0.1.7": "0.1.7", "0.1.7-dev": "dev"} {
		var header atomic.Pointer[http.Header]
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header.Store(&r.Header)
			w.Write([]byte(`{"version":"0.1.8"}`))
		}))
		t.Cleanup(srv.Close)
		s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: have, UpdateURL: srv.URL})
		s.checkUpdate(context.Background())
		h := header.Load()
		if h == nil {
			t.Fatalf("%s: the site was not asked", have)
		}
		if v, sys := agent.Parse(h.Get("User-Agent")); v != want || sys != system {
			t.Errorf("%s: User-Agent %q read as %q, %q; want %q, %q", have, h.Get("User-Agent"), v, sys, want, system)
		}
		for name := range *h {
			if name != "User-Agent" && name != "Accept-Encoding" {
				t.Errorf("%s: the request has the header %s: %q", have, name, h.Values(name))
			}
		}
	}
}

func TestUpdateCheckOffAsksNothing(t *testing.T) {
	url, asked := site(t, `{"version":"0.1.8"}`)
	s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url})
	s.checkUpdate(context.Background())
	if st := s.State(); st.Update == nil {
		t.Fatal("no notice with the setting on")
	}
	if err := s.SetUpdateCheck(false); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.Update != nil || st.UpdateCheck {
		t.Fatalf("with the setting off: update %+v, check %v", st.Update, st.UpdateCheck)
	}
	s.checkUpdate(context.Background())
	if asked.Load() != 1 {
		t.Fatalf("asked %d times, the second with the setting off", asked.Load())
	}
	if set, err := settings.Load(s.dir); err != nil || !set.NoUpdateCheck {
		t.Fatalf("saved: %+v, %v", set, err)
	}
}

func TestAPageThatIsNotAWebPageIsNotOpened(t *testing.T) {
	url, _ := site(t, `{"version":"0.1.8","page":"file:///etc/passwd"}`)
	s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url})
	s.checkUpdate(context.Background())
	if s.latestPage != downloadPage {
		t.Fatalf("page: %q", s.latestPage)
	}
}

func TestOwnPageIsOnlyYondersSite(t *testing.T) {
	for page, want := range map[string]bool{
		"https://getyonder.org/":                      true,
		"https://getyonder.org/#download":             true,
		"https://GETYONDER.org/get/1":                 true,
		"https://example.org/get":                     false,
		"http://getyonder.org/":                       false,
		"https://getyonder.org.evil.example/":         false,
		"https://getyonder.org@evil.example/":         false,
		"https://evil.example/https://getyonder.org/": false,
		"https://getyonder.org:8443/":                 false,
		"":                                            false,
	} {
		if got := ownPage(page); got != want {
			t.Errorf("ownPage(%q) = %v, want %v", page, got, want)
		}
	}
}

func TestAPageOnAnotherSiteIsNotOpened(t *testing.T) {
	url, _ := site(t, `{"version":"0.1.8","page":"https://example.org/get"}`)
	s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url})
	s.checkUpdate(context.Background())
	if s.latestPage != downloadPage {
		t.Fatalf("page: %q", s.latestPage)
	}
}
