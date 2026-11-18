package gui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A second Yonder that hands over brings a notice only when it is newer.
func TestStartedNewer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler()) // no update known
	defer srv.Close()
	quit := make(chan struct{}, 1)
	s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.22", UpdateURL: srv.URL, Quit: func() { quit <- struct{}{} }})

	for _, v := range []string{"", "0.1.22", "0.1.9", "not a version"} {
		s.Started(v)
		if n := s.State().Started; n != nil {
			t.Fatalf("started %q: notice %+v, want none", v, n)
		}
	}
	s.Started("0.1.28")
	s.Started("0.1.27") // an older one after it does not replace it
	if n := s.State().Started; n == nil || n.Version != "0.1.28" || n.Have != "0.1.22" {
		t.Fatalf("notice %+v, want 0.1.28 over 0.1.22", n)
	}
	if err := s.HideStarted(); err != nil || s.State().Started != nil {
		t.Fatalf("after HideStarted: %v, %+v", err, s.State().Started)
	}
	if err := s.Quit(); err != nil {
		t.Fatal(err)
	}
	<-quit
}
