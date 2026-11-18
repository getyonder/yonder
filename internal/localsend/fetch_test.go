package localsend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A file that the receiver fetches is saved as an uploaded one is, with the
// same reports at both ends.
func TestFetchedFilesAreSavedLikeUploadedOnes(t *testing.T) {
	h := newHarness(t)
	l, from := newLender(t)
	h.fetchFrom(from)
	var mu sync.Mutex
	var got []Progress
	var finished []bool
	var saved []string
	var offer Offer
	h.rc.Accept = func(ctx context.Context, o Offer) []string {
		offer = o
		return acceptAll(ctx, o)
	}
	h.rc.Progress = func(p Progress) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	}
	h.rc.Received = func(path string, f FileMeta) {
		mu.Lock()
		saved = append(saved, filepath.Base(path))
		mu.Unlock()
	}
	h.rc.Finished = func(complete bool) {
		mu.Lock()
		finished = append(finished, complete)
		mu.Unlock()
	}
	// A name is taken already, so the fetched file must not overwrite it.
	if err := os.WriteFile(filepath.Join(h.saveDir, "one.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	one, two := []byte("first"), bytes.Repeat([]byte("y"), 1<<20)
	total := int64(len(one) + len(two))
	paths := []string{writeFile(t, "one.txt", one), writeFile(t, "two.bin", two)}
	var sentMu sync.Mutex
	var sent []Progress
	ctx := WithProgress(context.Background(), func(p Progress) {
		sentMu.Lock()
		sent = append(sent, p)
		sentMu.Unlock()
	})
	s := newSender(t)
	if n, err := s.Lend(ctx, h.peer(), paths, l, allowAll); err != nil || n != 2 {
		t.Fatalf("lent %d, err %v", n, err)
	}

	wantFile(t, filepath.Join(h.saveDir, "one.txt"), []byte("mine"))
	wantFile(t, filepath.Join(h.saveDir, "one (1).txt"), one)
	wantFile(t, filepath.Join(h.saveDir, "two.bin"), two)
	if offer.Fetch == "" || len(offer.Files) != 2 || offer.Sender.Alias != "test sender" {
		t.Fatalf("the person was asked about %+v, want a marked offer of two files from the sender", offer)
	}
	if want := s.Info.Fingerprint; offer.CertFingerprint != want {
		t.Fatalf("offer carried certificate fingerprint %q, want the sender's %q", offer.CertFingerprint, want)
	}
	sentMu.Lock()
	defer sentMu.Unlock()
	if first, last := sent[0], sent[len(sent)-1]; first != (Progress{Total: total}) || last != (Progress{File: "two.bin", Done: total, Total: total}) {
		t.Fatalf("sender reported %+v first and %+v last", first, last)
	}
	mu.Lock()
	defer mu.Unlock()
	if first, last := got[0], got[len(got)-1]; first != (Progress{Total: total}) || last.Done != total || last.Total != total {
		t.Fatalf("receiver reported %+v first and %+v last", first, last)
	}
	if len(saved) != 2 || saved[0] != "one (1).txt" || saved[1] != "two.bin" {
		t.Fatalf("received = %v, want the two files in turn", saved)
	}
	if len(finished) != 1 || !finished[0] {
		t.Fatalf("finished = %v, want one complete transfer", finished)
	}
}

// The limits on what is lent: the list once; a file only if it was taken,
// only once, and only under the id of its offer; and nothing to a request
// that is not allowed.
func TestLenderGivesOnlyWhatWasTakenAndOnlyOnce(t *testing.T) {
	h := newHarness(t)
	l, from := newLender(t)
	h.fetchFrom(from)
	// ask puts a request of a fetching receiver to the lender.
	ask := func(method, path string, q url.Values) int {
		req, err := http.NewRequest(method, from.baseURL()+path+"?"+q.Encode(), nil)
		if err != nil {
			t.Error(err)
			return 0
		}
		resp, err := h.client.Do(req)
		if err != nil {
			t.Error(err)
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	var mark string
	var allowed atomic.Bool
	allowed.Store(true)
	var early, late map[string]int
	h.rc.Accept = func(_ context.Context, o Offer) []string {
		mark = o.Fetch
		early = map[string]int{
			"the list a second time":         ask("POST", apiPrefix+"/prepare-download", url.Values{"sessionId": {mark}}),
			"the list under another id":      ask("POST", apiPrefix+"/prepare-download", url.Values{"sessionId": {"0123"}}),
			"a file under another id":        ask("GET", apiPrefix+"/download", url.Values{"sessionId": {"0123"}, "fileId": {"1"}}),
			"a file with no id of the offer": ask("GET", apiPrefix+"/download", url.Values{"fileId": {"1"}}),
		}
		return []string{"1"}
	}
	h.rc.Received = func(string, FileMeta) {
		// The loan is still open: the closing word has not gone out.
		late = map[string]int{
			"a file that was not taken":     ask("GET", apiPrefix+"/download", url.Values{"sessionId": {mark}, "fileId": {"0"}}),
			"the taken file a second time":  ask("GET", apiPrefix+"/download", url.Values{"sessionId": {mark}, "fileId": {"1"}}),
			"a file that was never offered": ask("GET", apiPrefix+"/download", url.Values{"sessionId": {mark}, "fileId": {"7"}}),
		}
		allowed.Store(false)
		late["the closing word, not allowed"] = ask("POST", fetchedPath, url.Values{"sessionId": {mark}, "complete": {"0"}})
		allowed.Store(true)
	}
	paths := []string{writeFile(t, "no.txt", []byte("no")), writeFile(t, "yes.txt", []byte("yes"))}

	sent, err := newSender(t).Lend(context.Background(), h.peer(), paths, l, func(*http.Request) bool { return allowed.Load() })
	if err != nil || sent != 1 {
		t.Fatalf("lent %d, err %v", sent, err)
	}
	wantFile(t, filepath.Join(h.saveDir, "yes.txt"), []byte("yes"))
	if _, err := os.Stat(filepath.Join(h.saveDir, "no.txt")); err == nil {
		t.Fatal("a file the receiver did not accept was saved")
	}
	for _, asked := range []map[string]int{early, late} {
		if len(asked) == 0 {
			t.Fatal("the requests were not made")
		}
		for what, status := range asked {
			wantStatus(t, what, status, http.StatusNotFound)
		}
	}
	// With the send over, nothing of it is served.
	wantStatus(t, "a file after the send", ask("GET", apiPrefix+"/download", url.Values{"sessionId": {mark}, "fileId": {"1"}}), http.StatusNotFound)
	wantStatus(t, "the list after the send", ask("POST", apiPrefix+"/prepare-download", url.Values{"sessionId": {mark}}), http.StatusNotFound)
}

// A lender that allows nothing gives no list, so the offer fails before the
// person is asked.
func TestLenderThatAllowsNothingGivesNoList(t *testing.T) {
	h := newHarness(t)
	l, from := newLender(t)
	h.fetchFrom(from)
	var asked atomic.Bool
	h.rc.Accept = func(ctx context.Context, o Offer) []string {
		asked.Store(true)
		return acceptAll(ctx, o)
	}
	path := writeFile(t, "one.txt", []byte("x"))
	// The sender's "no" to the list means that it sees no way the files may
	// take, and the offers count for nothing against its address as offers:
	// none raised a question. They have a limit of their own, noPathBurst.
	s := newSender(t)
	for i := range noPathBurst {
		sent, err := s.Lend(context.Background(), h.peer(), []string{path}, l, func(*http.Request) bool { return false })
		if !errors.Is(err, ErrFetchRefused) || sent != 0 {
			t.Fatalf("offer %d: lent %d, err %v, want ErrFetchRefused", i+1, sent, err)
		}
	}
	if asked.Load() {
		t.Fatal("the person was asked about an offer whose list could not be fetched")
	}
	// The slot is free again.
	if status, _ := h.prepare(meta("a", "a.txt", []byte("a"))); status != http.StatusOK {
		t.Fatalf("an offer after the failed one: HTTP %d", status)
	}
}

// Offers refused for want of a path have a limit of their own for each
// address: a few tries pass, a stream of them is refused with 429 before
// this device looks for the way again, and the ordinary limit is untouched.
func TestOffersWithoutAPathHaveALimit(t *testing.T) {
	h := newHarness(t)
	var lists atomic.Int32
	from := standIn(t, func(w http.ResponseWriter, r *http.Request) {
		lists.Add(1)
		http.NotFound(w, r)
	})
	h.fetchFrom(from)
	for i := range noPathBurst {
		if s, _ := h.offerMarked("mark"); s != http.StatusForbidden {
			t.Fatalf("try %d: HTTP %d, want 403 no-path", i+1, s)
		}
	}
	for i := range 2 * offerBurst {
		if s, _ := h.offerMarked("mark"); s != http.StatusTooManyRequests {
			t.Fatalf("offer %d past the limit: HTTP %d, want 429", i+1, s)
		}
	}
	if n := int(lists.Load()); n != noPathBurst {
		t.Fatalf("list asked %d times, want %d", n, noPathBurst)
	}
	h.rc.Accept = func(context.Context, Offer) []string { return nil }
	for i := range offerBurst {
		if s, _ := h.prepare(meta("a", "a.txt", []byte("a"))); s != http.StatusForbidden {
			t.Fatalf("ordinary offer %d: HTTP %d, want 403 declined", i+1, s)
		}
	}
}

func TestFetchDeclinedAndWithoutAWay(t *testing.T) {
	h := newHarness(t)
	l, from := newLender(t)
	path := writeFile(t, "one.txt", []byte("x"))
	s := newSender(t)

	h.fetchFrom(from)
	h.rc.Accept = func(context.Context, Offer) []string { return nil }
	if _, err := s.Lend(context.Background(), h.peer(), []string{path}, l, allowAll); !errors.Is(err, ErrDeclined) {
		t.Fatalf("declined: err %v, want ErrDeclined", err)
	}

	// The receiver has no way to the sender that the files may take: it
	// says so, which is not a "no" from its person, who is not asked.
	var asked atomic.Bool
	h.rc.Accept = func(ctx context.Context, o Offer) []string {
		asked.Store(true)
		return acceptAll(ctx, o)
	}
	h.rc.Source = func(context.Context, Offer) (*Source, error) { return nil, errors.New("no way") }
	if _, err := s.Lend(context.Background(), h.peer(), []string{path}, l, allowAll); !errors.Is(err, ErrFetchRefused) {
		t.Fatalf("no way to the sender: err %v, want ErrFetchRefused", err)
	}
	if asked.Load() {
		t.Fatal("the person was asked about an offer that could not be fetched")
	}

	// A receiver that does not fetch takes a marked offer for an empty one.
	h.rc.Source = nil
	if _, err := s.Lend(context.Background(), h.peer(), []string{path}, l, allowAll); err == nil || errors.Is(err, ErrDeclined) || errors.Is(err, ErrFetchRefused) {
		t.Fatalf("a receiver that does not fetch: err %v, want the offer refused as invalid", err)
	}
	if asked.Load() {
		t.Fatal("the person was asked about an offer with no files")
	}
}

// A file that is shorter than the offer said is not saved, and the send
// ends as stopped at both ends.
func TestFetchedFileOfAnotherSizeFails(t *testing.T) {
	h := newHarness(t)
	l, from := newLender(t)
	h.fetchFrom(from)
	path := writeFile(t, "shrinks.bin", bytes.Repeat([]byte("z"), 4096))
	finished := make(chan bool, 1)
	h.rc.Finished = func(complete bool) { finished <- complete }
	h.rc.Accept = func(ctx context.Context, o Offer) []string {
		// The list is in; the file changes before it is fetched.
		if err := os.Truncate(path, 100); err != nil {
			t.Error(err)
		}
		return acceptAll(ctx, o)
	}
	sent, err := newSender(t).Lend(context.Background(), h.peer(), []string{path}, l, allowAll)
	if !errors.Is(err, ErrFetchStopped) || sent != 0 {
		t.Fatalf("lent %d, err %v, want ErrFetchStopped", sent, err)
	}
	if complete := <-finished; complete {
		t.Fatal("the receiver called the transfer complete")
	}
	wantEmpty(t, h.saveDir)
}

// wantEmpty fails if anything was left in a save folder, a part of a file
// included.
func wantEmpty(t *testing.T, dir string) {
	t.Helper()
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d entries were left in the save folder, the first %q", len(left), left[0].Name())
	}
}

// The receiver's person cancels in the middle of a file: the fetch stops,
// nothing is kept, and the sender is told.
func TestCancelAtTheReceiverStopsAFetch(t *testing.T) {
	h := newHarness(t)
	l, from := newLender(t)
	h.fetchFrom(from)
	data := bytes.Repeat([]byte("0123456789abcdef"), 1<<19) // 8 MiB
	path := writeFile(t, "big.bin", data)
	finished := make(chan bool, 1)
	h.rc.Finished = func(complete bool) { finished <- complete }
	var once sync.Once
	h.rc.Progress = func(p Progress) {
		if p.Done > 0 {
			once.Do(h.rc.Cancel) // with the first bytes in
		}
	}
	sent, err := newSender(t).Lend(context.Background(), h.peer(), []string{path}, l, allowAll)
	if !errors.Is(err, ErrFetchStopped) || sent != 0 {
		t.Fatalf("lent %d, err %v, want ErrFetchStopped", sent, err)
	}
	if complete := <-finished; complete {
		t.Fatal("the receiver called the transfer complete")
	}
	wantEmpty(t, h.saveDir)
}

// The sender's person cancels in the middle of a file: the receiver stops
// fetching and keeps nothing.
func TestCancelAtTheSenderStopsAFetch(t *testing.T) {
	h := newHarness(t)
	l, from := newLender(t)
	h.fetchFrom(from)
	path := writeFile(t, "big.bin", bytes.Repeat([]byte("0123456789abcdef"), 1<<19))
	finished := make(chan bool, 1)
	h.rc.Finished = func(complete bool) { finished <- complete }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = WithProgress(ctx, func(p Progress) {
		if p.Done > 0 {
			cancel() // with the first bytes out
		}
	})
	sent, err := newSender(t).Lend(ctx, h.peer(), []string{path}, l, allowAll)
	if !errors.Is(err, context.Canceled) || sent != 0 {
		t.Fatalf("lent %d, err %v, want it cancelled", sent, err)
	}
	if complete := <-finished; complete {
		t.Fatal("the receiver called the transfer complete")
	}
	wantEmpty(t, h.saveDir)
	// The slot is free again at once, as after a cancelled upload.
	if status, _ := h.prepare(meta("a", "a.txt", []byte("a"))); status != http.StatusOK {
		t.Fatalf("an offer after the cancelled one: HTTP %d", status)
	}
}

// The sender goes quiet in the middle of a file that the receiver fetches:
// the receiver stops, keeps nothing and is free again.
func TestQuietFetchEndsTheTransfer(t *testing.T) {
	h := newHarness(t)
	h.rc.QuietWait = 50 * time.Millisecond
	l, from := newLender(t)
	h.fetchFrom(from)
	path := writeFile(t, "big.bin", bytes.Repeat([]byte("0123456789abcdef"), 1<<19))
	finished := make(chan bool, 1)
	h.rc.Finished = func(complete bool) { finished <- complete }
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release) // before the servers close, which wait for their handlers
	var once sync.Once
	ctx := WithProgress(context.Background(), func(p Progress) {
		if p.Done > 0 {
			once.Do(func() { <-hold }) // the sender stops here, with the first bytes out
		}
	})
	lent := make(chan error, 1)
	go func() {
		_, err := newSender(t).Lend(ctx, h.peer(), []string{path}, l, allowAll)
		lent <- err
	}()
	select {
	case complete := <-finished:
		if complete {
			t.Fatal("the receiver called the transfer complete")
		}
	case <-time.After(wait):
		t.Fatalf("waited %v for the quiet fetch to end", wait)
	}
	until(t, "the unfinished file to go", func() bool { return h.partFiles() == 0 })
	if status, _ := h.prepare(meta("a", "a.txt", []byte("a"))); status != http.StatusOK {
		t.Fatalf("an offer after the quiet one: HTTP %d", status)
	}
	release()
	select {
	case err := <-lent:
		if err == nil {
			t.Fatal("the send that went quiet ended without an error")
		}
	case <-time.After(wait):
		t.Fatalf("waited %v for the send to end", wait)
	}
}

// A receiver that says yes and then asks for nothing: the send gives up,
// and tells the receiver so.
func TestLendGivesUpOnAQuietReceiver(t *testing.T) {
	cancelled := make(chan string, 1)
	quiet := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPrefix + "/prepare-upload":
			w.Header().Set(FetchHeader, fetchYes)
			json.NewEncoder(w).Encode(prepareResponse{SessionID: "theirs", Files: map[string]string{"0": ""}})
		case apiPrefix + "/cancel":
			cancelled <- r.URL.Query().Get("sessionId")
		}
	}))
	t.Cleanup(quiet.Close)
	// No fingerprint: this stand-in has no identity to check.
	to := Peer{Host: "127.0.0.1", Info: Info{Port: quiet.Listener.Addr().(*net.TCPAddr).Port, Protocol: "https"}}
	l := &Lender{IdleTime: 40 * time.Millisecond}
	path := writeFile(t, "one.txt", []byte("x"))
	sent, err := newSender(t).Lend(context.Background(), to, []string{path}, l, allowAll)
	if !errors.Is(err, ErrFetchQuiet) || sent != 0 {
		t.Fatalf("lent %d, err %v, want ErrFetchQuiet", sent, err)
	}
	if got := <-cancelled; got != "theirs" {
		t.Fatalf("the receiver was told to stop session %q, want its own", got)
	}
}

// An ordinary offer is unchanged by a receiver that can fetch.
func TestOrdinaryOfferToAReceiverThatCanFetch(t *testing.T) {
	h := newHarness(t)
	_, from := newLender(t)
	h.fetchFrom(from)
	var offer Offer
	h.rc.Accept = func(ctx context.Context, o Offer) []string {
		offer = o
		return acceptAll(ctx, o)
	}
	data := []byte("uploaded")
	if sent, err := newSender(t).Send(context.Background(), h.peer(), []string{writeFile(t, "up.txt", data)}); err != nil || sent != 1 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
	wantFile(t, filepath.Join(h.saveDir, "up.txt"), data)
	if offer.Fetch != "" {
		t.Fatalf("an ordinary offer carried the mark %q", offer.Fetch)
	}
}

// standIn starts a device that answers a fetching receiver with handler, and
// returns it as the receiver reaches it. It has no identity to check.
func standIn(t *testing.T, handler http.HandlerFunc) Peer {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	return Peer{Host: "127.0.0.1", Info: Info{Port: srv.Listener.Addr().(*net.TCPAddr).Port, Protocol: "https"}}
}

// offerMarked makes a marked offer to the harness's receiver, as a sender
// that lends does, and returns the status and, on 200, the session.
func (h *harness) offerMarked(mark string) (int, prepareResponse) {
	h.t.Helper()
	body, _ := json.Marshal(prepareRequest{Info: Info{Alias: "test sender", Version: Version, Fingerprint: "sender"}, Files: map[string]FileMeta{}})
	req, err := http.NewRequest("POST", h.url+apiPrefix+"/prepare-upload", bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set(FetchHeader, mark)
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var session prepareResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
			h.t.Fatalf("prepare-upload reply: %v", err)
		}
	}
	return resp.StatusCode, session
}

// The files of a fetched session come only the way the receiver fetches
// them: the sender cannot upload one, though it holds the session's id.
func TestFetchedSessionTakesNoUploads(t *testing.T) {
	h := newHarness(t)
	one, two := []byte("first"), []byte("second")
	asked := make(chan struct{}, 1)
	release := make(chan struct{})
	from := standIn(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPrefix + "/prepare-download":
			json.NewEncoder(w).Encode(downloadList{Files: map[string]FileMeta{"0": meta("0", "one.txt", one), "1": meta("1", "two.txt", two)}})
		case apiPrefix + "/download":
			// The first file stays on its way, so the second is not yet
			// asked for.
			asked <- struct{}{}
			<-release
		}
	})
	t.Cleanup(func() { close(release) })
	h.fetchFrom(from)

	status, session := h.offerMarked("mark")
	if status != http.StatusOK {
		t.Fatalf("marked offer: HTTP %d", status)
	}
	<-asked
	wantStatus(t, "an upload into a fetched session", h.upload(session.SessionID, "1", session.Files["1"], two), http.StatusForbidden)
	if _, err := os.Stat(filepath.Join(h.saveDir, "two.txt")); err == nil {
		t.Fatal("an uploaded file was saved in a fetched session")
	}
	h.rc.Cancel()
}

// A receiver that has every file and whose closing word does not arrive:
// the send still counts as made.
func TestLostClosingWordStillCountsAsSent(t *testing.T) {
	h := newHarness(t)
	l, from := newLender(t)
	l.IdleTime = 300 * time.Millisecond
	h.fetchFrom(from)
	data := []byte("arrived")
	path := writeFile(t, "one.txt", data)
	sent, err := newSender(t).Lend(context.Background(), h.peer(), []string{path}, l, func(r *http.Request) bool {
		return r.URL.Path != fetchedPath
	})
	if err != nil || sent != 1 {
		t.Fatalf("lent %d, err %v, want the one file sent", sent, err)
	}
	wantFile(t, filepath.Join(h.saveDir, "one.txt"), data)
}

// A sender that does not give its list keeps the receiver only for a limited
// time, and nobody is asked.
func TestFetchingTheListHasALimit(t *testing.T) {
	h := newHarness(t)
	h.rc.BorrowTime = 50 * time.Millisecond
	release := make(chan struct{})
	from := standIn(t, func(http.ResponseWriter, *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	h.fetchFrom(from)
	var asked atomic.Bool
	h.rc.Accept = func(ctx context.Context, o Offer) []string {
		asked.Store(true)
		return acceptAll(ctx, o)
	}
	// The stand-in is still silent when the answer comes.
	status, _ := h.offerMarked("mark")
	wantStatus(t, "an offer whose list does not come", status, http.StatusBadGateway)
	if asked.Load() {
		t.Fatal("the person was asked about an offer whose list did not come")
	}
	if status, _ := h.prepare(meta("a", "a.txt", []byte("a"))); status != http.StatusOK {
		t.Fatalf("an offer after the silent one: HTTP %d", status)
	}
}

// heldBack puts a device in front of the harness's receiver that passes
// every request on, but keeps the answer to an offer back until the test
// ends: the sender's own connection at its slowest.
func (h *harness) heldBack() Peer {
	release := make(chan struct{})
	to := standIn(h.t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != apiPrefix+"/prepare-upload" {
			h.rc.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		h.rc.ServeHTTP(rec, r)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	h.t.Cleanup(func() { close(release) })
	return to
}

// The receiver's yes also comes on its own connection, and the send goes on
// from it without the answer to the offer.
func TestYesOnTheReceiversOwnConnection(t *testing.T) {
	h := newHarness(t)
	l, from := newLender(t)
	h.fetchFrom(from)
	data := []byte("fetched")
	path := writeFile(t, "one.txt", data)
	sent, err := newSender(t).Lend(context.Background(), h.heldBack(), []string{path}, l, allowAll)
	if err != nil || sent != 1 {
		t.Fatalf("lent %d, err %v, want the one file sent", sent, err)
	}
	wantFile(t, filepath.Join(h.saveDir, "one.txt"), data)
}

// And its no.
func TestNoOnTheReceiversOwnConnection(t *testing.T) {
	h := newHarness(t)
	l, from := newLender(t)
	h.fetchFrom(from)
	h.rc.Accept = func(context.Context, Offer) []string { return nil }
	path := writeFile(t, "one.txt", []byte("x"))
	if _, err := newSender(t).Lend(context.Background(), h.heldBack(), []string{path}, l, allowAll); !errors.Is(err, ErrDeclined) {
		t.Fatalf("err %v, want ErrDeclined", err)
	}
}

// A sender from before 0.1.20 does not take the answer on the receiver's
// connection: the fetch goes on with the answer that the offer got.
func TestSenderThatTakesNoAnswerOnTheReceiversConnection(t *testing.T) {
	h := newHarness(t)
	data := []byte("from an older sender")
	var closing atomic.Value
	from := standIn(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case apiPrefix + "/prepare-download":
			json.NewEncoder(w).Encode(downloadList{Files: map[string]FileMeta{"0": meta("0", "old.txt", data)}})
		case apiPrefix + "/download":
			w.Write(data)
		case fetchedPath:
			closing.Store(r.URL.Query().Get("complete"))
		default:
			http.NotFound(w, r)
		}
	})
	h.fetchFrom(from)
	finished := make(chan bool, 1)
	h.rc.Finished = func(complete bool) { finished <- complete }
	if status, _ := h.offerMarked("mark"); status != http.StatusOK {
		t.Fatalf("marked offer: HTTP %d", status)
	}
	if !<-finished {
		t.Fatal("the fetch did not complete")
	}
	wantFile(t, filepath.Join(h.saveDir, "old.txt"), data)
	h.rc.Cancel()
}

// What Yonder devices tell each other with an offer goes with both kinds:
// one whose files are uploaded and one whose files are fetched.
func TestOfferHeaderGoesWithEveryOffer(t *testing.T) {
	got := make(chan http.Header, 2)
	declines := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == apiPrefix+"/prepare-upload" {
			got <- r.Header
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	t.Cleanup(declines.Close)
	to := Peer{Host: "127.0.0.1", Info: Info{Port: declines.Listener.Addr().(*net.TCPAddr).Port, Protocol: "https"}}
	path := writeFile(t, "one.txt", []byte("x"))
	s := newSender(t)
	s.OfferHeader = http.Header{"Yonder-Version": {"0.1.31"}}
	if _, err := s.Send(context.Background(), to, []string{path}); !errors.Is(err, ErrDeclined) {
		t.Fatalf("send: %v, want declined", err)
	}
	if _, err := s.Lend(context.Background(), to, []string{path}, &Lender{}, allowAll); !errors.Is(err, ErrDeclined) {
		t.Fatalf("lend: %v, want declined", err)
	}
	for _, kind := range []string{"upload", "fetch"} {
		if h := <-got; h.Get("Yonder-Version") != "0.1.31" || h.Get("Content-Type") != "application/json" {
			t.Errorf("%s offer: headers %v", kind, h)
		}
	}
}
