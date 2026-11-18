package localsend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yonder/internal/names"
)

func wantStatus(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: HTTP %d, want %d", what, got, want)
	}
}

func wantFile(t *testing.T, path string, data []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("%s: contents differ", path)
	}
}

func TestInfo(t *testing.T) {
	h := newHarness(t)
	status, body := h.do("GET", "/info", nil)
	wantStatus(t, "info", status, http.StatusOK)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["alias"] != "test receiver" || got["fingerprint"] != h.rc.Info.Fingerprint {
		t.Fatalf("info reply: %s", body)
	}
	if _, has := got["port"]; has {
		t.Fatalf("info reply should not carry a port: %s", body)
	}
}

func TestRegisterTellsTheOwnerAndReplies(t *testing.T) {
	h := newHarness(t)
	var seen Info
	h.rc.Registered = func(peer Info, _ string) { seen = peer }
	status, body := h.do("POST", "/register", []byte(`{"alias":"Kind Pineapple","version":"2.2","fingerprint":"F","port":53317,"protocol":"https"}`))
	wantStatus(t, "register", status, http.StatusOK)
	if seen.Alias != "Kind Pineapple" || seen.Port != 53317 {
		t.Fatalf("registered peer: %+v", seen)
	}
	if !bytes.Contains(body, []byte(`"test receiver"`)) {
		t.Fatalf("register reply: %s", body)
	}
}

func TestReceiveFiles(t *testing.T) {
	h := newHarness(t)
	var received []string
	h.rc.Received = func(path string, _ FileMeta) { received = append(received, path) }

	one, two := []byte("first file"), bytes.Repeat([]byte("x"), 1<<20)
	status, s := h.prepare(meta("a", "one.txt", one), meta("b", "folder/two.bin", two))
	wantStatus(t, "prepare-upload", status, http.StatusOK)
	wantStatus(t, "upload a", h.upload(s.SessionID, "a", s.Files["a"], one), http.StatusOK)
	wantStatus(t, "upload b", h.upload(s.SessionID, "b", s.Files["b"], two), http.StatusOK)

	wantFile(t, filepath.Join(h.saveDir, "one.txt"), one)
	wantFile(t, filepath.Join(h.saveDir, "folder", "two.bin"), two)
	if len(received) != 2 {
		t.Fatalf("Received called %d times, want 2", len(received))
	}

	// The session ended with its last file, so a new offer is welcome.
	status, _ = h.prepare(meta("c", "three.txt", one))
	wantStatus(t, "second offer", status, http.StatusOK)
}

func TestExistingFileIsNotOverwritten(t *testing.T) {
	h := newHarness(t)
	old, fresh := []byte("old"), []byte("fresh")
	if err := os.WriteFile(filepath.Join(h.saveDir, "note.txt"), old, 0o644); err != nil {
		t.Fatal(err)
	}
	_, s := h.prepare(meta("a", "note.txt", fresh))
	wantStatus(t, "upload", h.upload(s.SessionID, "a", s.Files["a"], fresh), http.StatusOK)
	wantFile(t, filepath.Join(h.saveDir, "note.txt"), old)
	wantFile(t, filepath.Join(h.saveDir, "note (1).txt"), fresh)
}

func TestFileNameCannotLeaveTheSaveFolder(t *testing.T) {
	h := newHarness(t)
	data := []byte("contained")
	_, s := h.prepare(meta("a", `..\../../escape.txt`, data))
	wantStatus(t, "upload", h.upload(s.SessionID, "a", s.Files["a"], data), http.StatusOK)
	wantFile(t, filepath.Join(h.saveDir, "escape.txt"), data)
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.saveDir), "escape.txt")); err == nil {
		t.Fatal("file was written outside the save folder")
	}
}

func TestDeclinedOffer(t *testing.T) {
	h := newHarness(t)
	h.rc.Accept = func(context.Context, Offer) []string { return nil }
	status, _ := h.prepare(meta("a", "one.txt", []byte("x")))
	wantStatus(t, "declined offer", status, http.StatusForbidden)
	// Declining frees the slot.
	h.rc.Accept = acceptAll
	status, _ = h.prepare(meta("a", "one.txt", []byte("x")))
	wantStatus(t, "next offer", status, http.StatusOK)
}

func TestPartlyAcceptedOffer(t *testing.T) {
	h := newHarness(t)
	h.rc.Accept = func(context.Context, Offer) []string { return []string{"b"} }
	status, s := h.prepare(meta("a", "one.txt", []byte("x")), meta("b", "two.txt", []byte("y")))
	wantStatus(t, "prepare-upload", status, http.StatusOK)
	if _, ok := s.Files["a"]; ok || s.Files["b"] == "" {
		t.Fatalf("tokens: %v, want only b", s.Files)
	}
}

func TestSecondOfferIsBlockedWhileOneIsOpen(t *testing.T) {
	h := newHarness(t)
	status, _ := h.prepare(meta("a", "one.txt", []byte("x")))
	wantStatus(t, "first offer", status, http.StatusOK)
	status, _ = h.prepare(meta("b", "two.txt", []byte("y")))
	wantStatus(t, "second offer", status, http.StatusConflict)
}

func TestCancelFreesTheSlot(t *testing.T) {
	h := newHarness(t)
	_, s := h.prepare(meta("a", "one.txt", []byte("x")))
	status, _ := h.do("POST", "/cancel?"+url.Values{"sessionId": {s.SessionID}}.Encode(), nil)
	wantStatus(t, "cancel", status, http.StatusOK)
	wantStatus(t, "upload after cancel", h.upload(s.SessionID, "a", s.Files["a"], []byte("x")), http.StatusForbidden)
	status, _ = h.prepare(meta("b", "two.txt", []byte("y")))
	wantStatus(t, "offer after cancel", status, http.StatusOK)
}

func TestBadUploads(t *testing.T) {
	h := newHarness(t)
	data := []byte("the real contents")
	_, s := h.prepare(meta("a", "one.txt", data))

	wantStatus(t, "missing parameters", h.upload(s.SessionID, "", "", data), http.StatusBadRequest)
	wantStatus(t, "wrong token", h.upload(s.SessionID, "a", "nope", data), http.StatusForbidden)
	wantStatus(t, "wrong session", h.upload("nope", "a", s.Files["a"], data), http.StatusForbidden)
	wantStatus(t, "wrong size", h.upload(s.SessionID, "a", s.Files["a"], append(data, '!')), http.StatusBadRequest)
	wantStatus(t, "wrong contents", h.upload(s.SessionID, "a", s.Files["a"], []byte("the fake contents")), http.StatusUnprocessableEntity)

	entries, err := os.ReadDir(h.saveDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed uploads left %d file(s) behind, first %q", len(entries), entries[0].Name())
	}

	// A failed upload can be retried, but a finished one cannot be repeated.
	wantStatus(t, "retry", h.upload(s.SessionID, "a", s.Files["a"], data), http.StatusOK)
	wantStatus(t, "repeat", h.upload(s.SessionID, "a", s.Files["a"], data), http.StatusForbidden)
}

func TestInvalidOffers(t *testing.T) {
	h := newHarness(t)
	status, _ := h.do("POST", "/prepare-upload", []byte(`not json`))
	wantStatus(t, "not json", status, http.StatusBadRequest)
	status, _ = h.prepare()
	wantStatus(t, "no files", status, http.StatusBadRequest)
	status, _ = h.prepare(FileMeta{ID: "a", FileName: "", Size: 1})
	wantStatus(t, "empty name", status, http.StatusBadRequest)
}

func TestOversizedOfferIsRefusedAndFreesTheSlot(t *testing.T) {
	h := newHarness(t)
	big := FileMeta{ID: "a", FileName: "one.txt", Size: 1, Preview: strings.Repeat("p", 2*maxOfferBody)}
	status, _ := h.prepare(big)
	wantStatus(t, "oversized offer", status, http.StatusBadRequest)

	many := make([]FileMeta, maxOfferFiles+1)
	for i := range many {
		many[i] = FileMeta{ID: fmt.Sprint(i), FileName: fmt.Sprintf("f%d.txt", i), Size: 1}
	}
	status, _ = h.prepare(many...)
	wantStatus(t, "too many files", status, http.StatusBadRequest)

	status, _ = h.prepare(meta("a", "one.txt", []byte("x")))
	wantStatus(t, "next offer", status, http.StatusOK)
}

func TestBusyReceiverAnswersBeforeReadingTheOffer(t *testing.T) {
	h := newHarness(t)
	status, _ := h.prepare(meta("a", "one.txt", []byte("x")))
	wantStatus(t, "first offer", status, http.StatusOK)
	// Were the body read first, this would be a 400.
	status, _ = h.do("POST", "/prepare-upload", []byte(`not json`))
	wantStatus(t, "offer while busy", status, http.StatusConflict)
}

func TestLargePreviewIsDropped(t *testing.T) {
	h := newHarness(t)
	var got []FileMeta
	h.rc.Accept = func(ctx context.Context, o Offer) []string {
		got = o.Files
		return acceptAll(ctx, o)
	}
	small := meta("a", "one.txt", []byte("x"))
	small.Preview = "tiny"
	large := meta("b", "two.txt", []byte("y"))
	large.Preview = strings.Repeat("p", maxPreview+1)
	status, _ := h.prepare(small, large)
	wantStatus(t, "offer", status, http.StatusOK)
	for _, f := range got {
		if f.ID == "a" && f.Preview != "tiny" || f.ID == "b" && f.Preview != "" {
			t.Fatalf("preview of %s: %d bytes", f.ID, len(f.Preview))
		}
	}
	if len(got) != 2 {
		t.Fatalf("offer had %d files, want 2", len(got))
	}
}

func TestUnansweredQuestionEnds(t *testing.T) {
	h := newHarness(t)
	h.rc.AcceptTimeout = 50 * time.Millisecond
	h.rc.Accept = func(ctx context.Context, o Offer) []string {
		<-ctx.Done() // a person who never answers
		return acceptAll(ctx, o)
	}
	status, _ := h.prepare(meta("a", "one.txt", []byte("x")))
	wantStatus(t, "unanswered offer", status, http.StatusForbidden)
	// The slot is free again.
	h.rc.Accept = acceptAll
	status, _ = h.prepare(meta("a", "one.txt", []byte("x")))
	wantStatus(t, "next offer", status, http.StatusOK)
}

func TestOffersFromOneAddressAreLimited(t *testing.T) {
	h := newHarness(t)
	asked := 0
	h.rc.Accept = func(context.Context, Offer) []string { asked++; return nil }
	for i := range offerBurst {
		status, _ := h.prepare(meta("a", "one.txt", []byte("x")))
		wantStatus(t, fmt.Sprintf("offer %d", i+1), status, http.StatusForbidden)
	}
	status, _ := h.prepare(meta("a", "one.txt", []byte("x")))
	wantStatus(t, "offer over the limit", status, http.StatusTooManyRequests)
	if asked != offerBurst {
		t.Fatalf("person asked %d times, want %d", asked, offerBurst)
	}
}

func TestOfferSourcesAreBounded(t *testing.T) {
	rc := &Receiver{}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for i := range maxOfferSources {
		if !rc.allowOffer(fmt.Sprint("10.0.", i/250, ".", i%250)) {
			t.Fatalf("address %d refused early", i)
		}
	}
	if rc.allowOffer("192.168.1.1") {
		t.Fatal("a new address was tracked beyond the limit")
	}
	if len(rc.offers) > maxOfferSources {
		t.Fatalf("%d addresses tracked", len(rc.offers))
	}
}

// A registration is small: a large one is refused unread, and of one that
// fits, the names are cut and cleaned before the owner is told.
func TestRegistrationIsSmallAndClean(t *testing.T) {
	h := newHarness(t)
	var seen []Info
	h.rc.Registered = func(peer Info, _ string) { seen = append(seen, peer) }
	big, _ := json.Marshal(Info{Alias: strings.Repeat("n", 1<<20), Fingerprint: "F"})
	status, _ := h.do("POST", "/register", big)
	wantStatus(t, "a registration of 1 MB", status, http.StatusBadRequest)
	long, _ := json.Marshal(Info{Alias: " evil\u202e\n" + strings.Repeat("n", 1000), DeviceModel: strings.Repeat("m", 1000), Fingerprint: "F"})
	status, _ = h.do("POST", "/register", long)
	wantStatus(t, "a registration with long names", status, http.StatusOK)
	odd, _ := json.Marshal(Info{Alias: "x", Fingerprint: strings.Repeat("F", maxFingerprint+1)})
	status, _ = h.do("POST", "/register", odd)
	wantStatus(t, "a registration with a long fingerprint", status, http.StatusBadRequest)
	if len(seen) != 1 {
		t.Fatalf("the owner was told of %d registrations, want 1", len(seen))
	}
	if want := "evil" + strings.Repeat("n", names.Max-4); seen[0].Alias != want || len(seen[0].DeviceModel) != names.Max {
		t.Errorf("registered as %q with %d bytes of model, want %q and %d", seen[0].Alias, len(seen[0].DeviceModel), want, names.Max)
	}
}

// The name in an offer is what the question shows for a sender that is not
// paired: it reaches the question cut and clean.
func TestOffererNameIsCleaned(t *testing.T) {
	h := newHarness(t)
	var asked Offer
	h.rc.Accept = func(_ context.Context, o Offer) []string { asked = o; return nil }
	body, _ := json.Marshal(prepareRequest{
		Info:  Info{Alias: "Your\u202e bank\x1b[2J\n" + strings.Repeat("k", 1000), Fingerprint: "F"},
		Files: map[string]FileMeta{"a": meta("a", "a.txt", []byte("x"))},
	})
	h.do("POST", "/prepare-upload", body)
	if want := "Your bank[2J" + strings.Repeat("k", names.Max-12); asked.Sender.Alias != want {
		t.Errorf("the question would show %q, want %q", asked.Sender.Alias, want)
	}
}

// A file name is shown whole in the question, so one longer than a disk
// takes is refused.
func TestLongFileNamesAreRefused(t *testing.T) {
	h := newHarness(t)
	for what, name := range map[string]string{
		"a long name": strings.Repeat("a", maxNamePart+1) + ".txt",
		"a long path": strings.Repeat(strings.Repeat("d", 200)+"/", 6) + "a.txt",
	} {
		status, _ := h.prepare(meta("a", name, []byte("x")))
		wantStatus(t, what, status, http.StatusBadRequest)
	}
	status, _ := h.prepare(meta("a", strings.Repeat("d", 200)+"/"+strings.Repeat("a", maxNamePart-4)+".txt", []byte("x")))
	wantStatus(t, "the longest name a disk takes", status, http.StatusOK)
}

func TestRefusedFileNames(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{
		"a:b", "file.txt:stream", "file.", "file ", "dir./x",
		"CON", "nul.txt", "Aux.tar.gz", "com1", "LPT9.log", "dir/prn.txt", "CON .txt",
		"photo‮gpj.exe", "a⁦b", "a⁩b", "a\nb", "a\x1bb",
	} {
		status, _ := h.prepare(meta("a", name, []byte("x")))
		wantStatus(t, fmt.Sprintf("name %q", name), status, http.StatusBadRequest)
	}
	var ok []FileMeta
	for i, name := range []string{"console.txt", "com10.txt", "COM0", "a.b.c", "folder/file.txt", "naïve 文件.txt", "lpt.txt"} {
		ok = append(ok, meta(fmt.Sprint(i), name, []byte("x")))
	}
	status, _ := h.prepare(ok...)
	wantStatus(t, "plain names", status, http.StatusOK)
}

func TestHiddenNamesLoseTheirDot(t *testing.T) {
	for name, want := range map[string]string{
		".bashrc":                     "_bashrc",
		".config/autostart/x.desktop": "_config/autostart/x.desktop",
		"folder/.hidden/x":            "folder/_hidden/x",
		`a\.ssh\key`:                  "a/_ssh/key",
		"plain.txt":                   "plain.txt",
	} {
		if got, ok := safeRel(name); !ok || filepath.ToSlash(got) != want {
			t.Errorf("safeRel(%q) = %q, %v; want %q", name, got, ok, want)
		}
	}
}

// A sender that stalls while it sends its offer holds no slot: the others
// are still asked. Only its own address waits for it.
func TestStalledOfferDoesNotBlockOthers(t *testing.T) {
	h := newHarness(t)
	h.rc.mu.Lock()
	h.rc.reading = map[string]bool{"192.0.2.7": true}
	h.rc.mu.Unlock()
	status, _ := h.prepare(meta("a", "a.txt", []byte("x")))
	wantStatus(t, "offer while another address stalls", status, http.StatusOK)

	h.rc.mu.Lock()
	h.rc.end(h.rc.session, "the test ended it")
	h.rc.reading = map[string]bool{"127.0.0.1": true}
	h.rc.mu.Unlock()
	status, _ = h.prepare(meta("a", "a.txt", []byte("x")))
	wantStatus(t, "offer while the same address stalls", status, http.StatusTooManyRequests)
}

// The name another device gives itself is cut before it is logged.
func TestOffererNameIsCutInTheLog(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var lines []string
	h.rc.Trace = func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	alias := "x\n2026-01-01 00:00:00.000 a line of its own" + strings.Repeat("A", 100<<10)
	req := prepareRequest{Info: Info{Alias: alias, Version: Version, Fingerprint: "s"}, Files: map[string]FileMeta{"a": meta("a", "a.txt", []byte("hi"))}}
	b, _ := json.Marshal(req)
	h.do("POST", "/prepare-upload", b)
	mu.Lock()
	defer mu.Unlock()
	if len(lines) == 0 || strings.Contains(lines[0], "\n") || len(lines[0]) > 300 {
		t.Fatalf("first line %d bytes, want one short line", len(lines[0]))
	}
	if got := Clip(strings.Repeat("é", 150)); got != strings.Repeat("é", 100)+"…" {
		t.Fatalf("Clip cut to %q", got)
	}
	if got := Clip("short"); got != "short" {
		t.Fatalf("Clip changed a short value to %q", got)
	}
}

// slowUpload starts an upload of size bytes whose body the test writes by
// hand through the returned pipe; the status (0 if the request failed)
// comes on the channel.
func (h *harness) slowUpload(session, fileID, token string, size int) (*io.PipeWriter, <-chan int) {
	h.t.Helper()
	pr, pw := io.Pipe()
	q := url.Values{"sessionId": {session}, "fileId": {fileID}, "token": {token}}
	req, err := http.NewRequest("POST", h.url+apiPrefix+"/upload?"+q.Encode(), pr)
	if err != nil {
		h.t.Fatal(err)
	}
	req.ContentLength = int64(size)
	status := make(chan int, 1)
	go func() {
		resp, err := h.client.Do(req)
		if err != nil {
			status <- 0
			return
		}
		resp.Body.Close()
		status <- resp.StatusCode
	}()
	h.t.Cleanup(func() { pw.CloseWithError(io.ErrClosedPipe) })
	return pw, status
}

// partFiles is how many unfinished files lie in the save folder.
func (h *harness) partFiles() int {
	parts, _ := filepath.Glob(filepath.Join(h.saveDir, ".yonder-*.part"))
	return len(parts)
}

// A sender that stops with no word, such as a web page frozen by a phone's
// locked screen, does not hold the receiver for good.
func TestQuietUploadEndsTheTransfer(t *testing.T) {
	h := newHarness(t)
	h.rc.QuietWait = 50 * time.Millisecond
	quiet, finished := make(chan bool, 1), make(chan bool, 1)
	h.rc.Quiet = func() { quiet <- true }
	h.rc.Finished = func(complete bool) { finished <- complete }
	data := []byte("ten bytes!")
	_, s := h.prepare(meta("a", "one.txt", data))
	pw, status := h.slowUpload(s.SessionID, "a", s.Files["a"], len(data))
	if _, err := pw.Write(data[:4]); err != nil {
		t.Fatal(err)
	}
	// Nothing more is sent. The limits below are only against a hang.
	select {
	case complete := <-finished:
		if complete {
			t.Fatal("the transfer ended as complete")
		}
	case <-time.After(wait):
		t.Fatalf("waited %v for the quiet transfer to end", wait)
	}
	select {
	case <-quiet:
	default:
		t.Fatal("Quiet was not called before Finished")
	}
	select {
	case got := <-status:
		if got == http.StatusOK {
			t.Fatal("the quiet upload was answered 200")
		}
	case <-time.After(wait):
		t.Fatalf("waited %v for the quiet upload to be answered", wait)
	}
	until(t, "the unfinished file to go", func() bool { return h.partFiles() == 0 })
	// The slot is free again.
	status2, _ := h.prepare(meta("b", "two.txt", []byte("y")))
	wantStatus(t, "offer after a quiet transfer", status2, http.StatusOK)
}

// A slow sender that keeps sending is not stopped: the quiet time starts
// again with each piece. The whole upload takes longer than the quiet time
// (5 gaps of 300 ms against 1 s); a gap would have to stretch by 700 ms for
// this to fail on a loaded machine.
func TestSlowUploadIsNotQuiet(t *testing.T) {
	h := newHarness(t)
	h.rc.QuietWait = time.Second
	data := []byte("six pieces of text!!")[:18]
	_, s := h.prepare(meta("a", "one.txt", data))
	pw, status := h.slowUpload(s.SessionID, "a", s.Files["a"], len(data))
	for i := 0; i < len(data); i += 3 {
		if i > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		if _, err := pw.Write(data[i : i+3]); err != nil {
			t.Fatal(err)
		}
	}
	pw.Close()
	select {
	case got := <-status:
		wantStatus(t, "slow upload", got, http.StatusOK)
	case <-time.After(wait):
		t.Fatalf("waited %v for the slow upload to be answered", wait)
	}
	wantFile(t, filepath.Join(h.saveDir, "one.txt"), data)
}
