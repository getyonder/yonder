package gui

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yonder/internal/app"
	"yonder/internal/localsend"
	"yonder/internal/withdrawn"
)

// signer makes site files with signed lists of withdrawn versions.
type signer struct {
	t    *testing.T
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return signer{t, pub, priv}
}

// file is the site's file that says newest is the newest version, with a
// list made at made that withdraws entries.
func (k signer) file(newest string, made time.Time, entries ...withdrawn.Entry) string {
	k.t.Helper()
	signed, err := withdrawn.Sign(withdrawn.List{Made: made, Versions: entries}, k.priv)
	if err != nil {
		k.t.Fatal(err)
	}
	b, err := json.Marshal(siteFile{Version: newest, Page: "https://getyonder.org/get", Signed: signed})
	if err != nil {
		k.t.Fatal(err)
	}
	return string(b)
}

// changing serves the site's file, which the test can change.
func changing(t *testing.T, body string) (url string, set func(string)) {
	t.Helper()
	var now atomic.Pointer[string]
	now.Store(&body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(*now.Load()))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func(b string) { now.Store(&b) }
}

var listMade = time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)

func TestWithdrawnNoticeNamesTheVersionToGet(t *testing.T) {
	k := newSigner(t)
	url, _ := site(t, k.file("0.1.8", listMade, withdrawn.Entry{Version: "0.1.7"}))
	s, w, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url, WithdrawKey: k.pub})
	if st := s.State(); st.Withdrawn != nil {
		t.Fatalf("before asking: %+v", *st.Withdrawn)
	}
	s.checkUpdate(context.Background())
	st := w.until(t, "the notice", func(st State) bool { return st.Withdrawn != nil })
	if *st.Withdrawn != (Update{Version: "0.1.8", Have: "0.1.7"}) {
		t.Fatalf("notice: %+v", *st.Withdrawn)
	}
	// One card says both: the notice that a newer version is out gives way.
	if st.Update != nil {
		t.Fatalf("the notice that 0.1.8 is out shows too: %+v", *st.Update)
	}
	// "Not now" belongs to the other notice and takes nothing away here.
	if err := s.HideUpdate(); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.Withdrawn == nil {
		t.Fatal("the notice was put away")
	}
}

func TestWithdrawnWithNoVersionToGet(t *testing.T) {
	k := newSigner(t)
	for name, body := range map[string]string{
		"none newer":           k.file("0.1.7", listMade, withdrawn.Entry{Version: "0.1.7"}),
		"the newer one is too": k.file("0.1.8", listMade, withdrawn.Entry{Below: "0.1.9"}),
	} {
		url, _ := site(t, body)
		s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url, WithdrawKey: k.pub})
		s.checkUpdate(context.Background())
		st := s.State()
		if st.Withdrawn == nil || *st.Withdrawn != (Update{Have: "0.1.7"}) {
			t.Errorf("%s: notice %+v", name, st.Withdrawn)
		}
		if st.Update != nil {
			t.Errorf("%s: the other notice shows: %+v", name, *st.Update)
		}
	}
}

func TestListsThatAreNotTaken(t *testing.T) {
	k, other := newSigner(t), newSigner(t)
	good := k.file("0.1.8", listMade, withdrawn.Entry{Version: "0.1.7"})
	var f siteFile
	if err := json.Unmarshal([]byte(good), &f); err != nil {
		t.Fatal(err)
	}
	b, _ := base64.StdEncoding.DecodeString(f.Withdrawn)
	b[len(b)-3] ^= 1
	f.Withdrawn = base64.StdEncoding.EncodeToString(b)
	changed, _ := json.Marshal(f)
	notThisSystem := "windows"
	if system() == "windows" {
		notThisSystem = "linux"
	}
	for name, c := range map[string]struct {
		body, have string
	}{
		"a changed byte":            {string(changed), "0.1.7"},
		"another key":               {other.file("0.1.8", listMade, withdrawn.Entry{Version: "0.1.7"}), "0.1.7"},
		"no list":                   {`{"version":"0.1.8"}`, "0.1.7"},
		"another version":           {k.file("0.1.8", listMade, withdrawn.Entry{Version: "0.1.6"}), "0.1.7"},
		"below this one":            {k.file("0.1.8", listMade, withdrawn.Entry{Below: "0.1.7"}), "0.1.7"},
		"another system":            {k.file("0.1.8", listMade, withdrawn.Entry{Version: "0.1.7", Systems: []string{notThisSystem}}), "0.1.7"},
		"a development build":       {k.file("0.1.8", listMade, withdrawn.Entry{Below: "9.0.0"}), "0.1.7-dev"},
		"a build with four numbers": {k.file("0.1.8", listMade, withdrawn.Entry{Below: "9.0.0"}), "0.1.7.1"},
	} {
		url, asked := site(t, c.body)
		s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: c.have, UpdateURL: url, WithdrawKey: k.pub})
		s.checkUpdate(context.Background())
		if asked.Load() != 1 {
			t.Fatalf("%s: asked %d times", name, asked.Load())
		}
		if st := s.State(); st.Withdrawn != nil {
			t.Errorf("%s: notice %+v", name, *st.Withdrawn)
		}
	}

	// The key in the app, and none given: while no key is made, no list is
	// taken.
	if withdrawn.Key() == nil {
		url, _ := site(t, good)
		s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url})
		s.checkUpdate(context.Background())
		if st := s.State(); st.Withdrawn != nil {
			t.Errorf("with no key: notice %+v", *st.Withdrawn)
		}
	}
}

func TestWithdrawnForThisSystem(t *testing.T) {
	k := newSigner(t)
	url, _ := site(t, k.file("0.1.8", listMade, withdrawn.Entry{Version: "0.1.7", Systems: []string{system()}}))
	s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url, WithdrawKey: k.pub})
	s.checkUpdate(context.Background())
	if st := s.State(); st.Withdrawn == nil {
		t.Fatal("no notice for a list that names this system")
	}
}

// A list goes out, then a newer one that lets the version back, then the
// first is served again: the newest one that was accepted holds.
func TestANewerListHoldsAndAnOlderIsNotTaken(t *testing.T) {
	k := newSigner(t)
	first := k.file("0.1.8", listMade, withdrawn.Entry{Version: "0.1.7"})
	url, set := changing(t, first)
	s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url, WithdrawKey: k.pub})
	s.checkUpdate(context.Background())
	if st := s.State(); st.Withdrawn == nil {
		t.Fatal("no notice")
	}

	// A file with no list changes nothing.
	set(`{"version":"0.1.8"}`)
	s.checkUpdate(context.Background())
	if st := s.State(); st.Withdrawn == nil {
		t.Fatal("a file with no list took the notice away")
	}

	set(k.file("0.1.8", listMade.Add(time.Hour)))
	s.checkUpdate(context.Background())
	st := s.State()
	if st.Withdrawn != nil {
		t.Fatalf("after a newer list without the version: %+v", *st.Withdrawn)
	}
	if st.Update == nil || st.Update.Version != "0.1.8" {
		t.Fatalf("the notice that 0.1.8 is out did not come back: %+v", st.Update)
	}

	set(first)
	s.checkUpdate(context.Background())
	if st := s.State(); st.Withdrawn != nil {
		t.Fatalf("an older list served again was taken: %+v", *st.Withdrawn)
	}
}

// The notice is there after a restart with no network, and names the
// version to get; an older list served to the restarted Yonder is not
// taken, because the date was kept too.
func TestWithdrawnAfterARestartWithNoNetwork(t *testing.T) {
	k := newSigner(t)
	url, set := changing(t, k.file("0.1.8", listMade, withdrawn.Entry{Version: "0.1.7"}))
	s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url, WithdrawKey: k.pub})
	s.checkUpdate(context.Background())
	if st := s.State(); st.Withdrawn == nil {
		t.Fatal("no notice")
	}
	dir := s.dir
	s.Close()

	start := func(o Options) *Service {
		t.Helper()
		o.Dir, o.Listen, o.Offline, o.UpdateURL = dir, "127.0.0.1:0", true, url
		again, err := Start(o, &window{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(again.Close)
		return again
	}
	again := start(Options{Version: "0.1.7", WithdrawKey: k.pub})
	st := again.State()
	if st.Withdrawn == nil || *st.Withdrawn != (Update{Version: "0.1.8", Have: "0.1.7"}) {
		t.Fatalf("after a restart: %+v", st.Withdrawn)
	}
	if again.latestPage != "https://getyonder.org/get" {
		t.Fatalf("page: %q", again.latestPage)
	}
	again.Close()

	// The new version, started in the same folder, has no notice of either
	// kind before it asks.
	if st := start(Options{Version: "0.1.8", WithdrawKey: k.pub}).State(); st.Withdrawn != nil || st.Update != nil {
		t.Fatalf("the new version: withdrawn %+v, update %+v", st.Withdrawn, st.Update)
	}

	// A kept copy that was changed, or checked with another key, is not
	// taken.
	if st := start(Options{Version: "0.1.7", WithdrawKey: newSigner(t).pub}).State(); st.Withdrawn != nil {
		t.Fatalf("a kept copy under another key: %+v", *st.Withdrawn)
	}

	// The list lets the version back; after the next restart an older list
	// is still refused.
	s3 := start(Options{Version: "0.1.7", WithdrawKey: k.pub})
	set(k.file("0.1.8", listMade.Add(time.Hour)))
	s3.checkUpdate(context.Background())
	if st := s3.State(); st.Withdrawn != nil {
		t.Fatalf("after a newer list: %+v", *st.Withdrawn)
	}
	s3.Close()
	s4 := start(Options{Version: "0.1.7", WithdrawKey: k.pub})
	if st := s4.State(); st.Withdrawn != nil {
		t.Fatalf("after the restart: %+v", *st.Withdrawn)
	}
	set(k.file("0.1.8", listMade, withdrawn.Entry{Version: "0.1.7"}))
	s4.checkUpdate(context.Background())
	if st := s4.State(); st.Withdrawn != nil {
		t.Fatalf("an older list after the restart: %+v", *st.Withdrawn)
	}
}

func TestAKeptCopyThatCannotBeReadIsNoList(t *testing.T) {
	k := newSigner(t)
	dir := t.TempDir()
	for _, body := range []string{"", "not json", `{"version":"0.1.8","withdrawn":"e30=","signature":"AAAA"}`} {
		if err := os.WriteFile(keptPath(dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		s := &Service{dir: dir, version: "0.1.7", withdrawKey: k.pub}
		s.restoreKept()
		if !s.list.Made.IsZero() || s.latest != "" {
			t.Errorf("%q: list %+v, latest %q", body, s.list, s.latest)
		}
	}
}

// With the update check off there is no request, and no notice from a list
// that was kept before.
func TestUpdateCheckOffNoWithdrawnNotice(t *testing.T) {
	k := newSigner(t)
	url, _ := site(t, k.file("0.1.8", listMade, withdrawn.Entry{Version: "0.1.7"}))
	s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url, WithdrawKey: k.pub})
	s.checkUpdate(context.Background())
	if err := s.SetUpdateCheck(false); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.Withdrawn != nil || st.Update != nil {
		t.Fatalf("with the setting off: withdrawn %+v, update %+v", st.Withdrawn, st.Update)
	}
	if err := s.SetUpdateCheck(true); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.Withdrawn == nil {
		t.Fatal("no notice with the setting on again")
	}
}

// The file may be longer than the 4 KB an older Yonder reads, up to 64 KB.
func TestALongFileIsRead(t *testing.T) {
	k := newSigner(t)
	var entries []withdrawn.Entry
	for range 400 {
		entries = append(entries, withdrawn.Entry{Version: "0.1.7", Systems: []string{"windows", "macos", "linux"}})
	}
	body := k.file("0.1.8", listMade, entries...)
	if len(body) < 1<<14 || len(body) > 1<<16 {
		t.Fatalf("the file is %d bytes", len(body))
	}
	url, _ := site(t, body)
	s, _, _ := deviceWith(t, "Kind Pineapple", Options{Version: "0.1.7", UpdateURL: url, WithdrawKey: k.pub})
	s.checkUpdate(context.Background())
	if st := s.State(); st.Withdrawn == nil {
		t.Fatal("no notice from a long file")
	}
	if !strings.Contains(body, `"withdrawn"`) {
		t.Fatal("the file has no list")
	}
}

func TestOlderNote(t *testing.T) {
	const none, plain, named = "", " The other device has an older Yonder. Updating it may help.", " The other device has an older Yonder (0.1.31). Updating it may help."
	for _, c := range []struct {
		own, theirs string
		heard       bool
		want        string
	}{
		{"0.1.32", "0.1.31", true, named},
		{"0.1.32", "", true, plain}, // a Yonder from before versions were told
		{"0.1.32", "", false, none}, // it has said nothing yet: nothing is known
		{"0.1.32", "0.1.31", false, none},
		{"0.1.32", "0.1.32", true, none},
		{"0.1.32", "0.1.33", true, none},
		{"0.1.32", "dev", true, none},
		{"", "0.1.31", true, none}, // this one is a development build
		{"0.1.32-test", "", true, none},
	} {
		if got := olderNote(c.own, c.theirs, c.heard); got != c.want {
			t.Errorf("olderNote(%q, %q, %v) = %q, want %q", c.own, c.theirs, c.heard, got, c.want)
		}
	}
	for err, want := range map[error]bool{nil: false, localsend.ErrDeclined: false, localsend.ErrBusy: false, context.Canceled: false, app.ErrUnreachable: true, app.ErrNoDirectPath: true} {
		if failed(err) != want {
			t.Errorf("failed(%v) = %v", err, !want)
		}
	}
}
