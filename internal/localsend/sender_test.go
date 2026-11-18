package localsend

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSendFiles(t *testing.T) {
	h := newHarness(t)
	var from string
	h.rc.Accept = func(ctx context.Context, o Offer) []string {
		from = o.Sender.Alias
		return acceptAll(ctx, o)
	}
	one, two := []byte("first"), bytes.Repeat([]byte("y"), 1<<20)
	paths := []string{writeFile(t, "one.txt", one), writeFile(t, "two.bin", two)}

	sent, err := newSender(t).Send(context.Background(), h.peer(), paths)
	if err != nil || sent != 2 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
	wantFile(t, filepath.Join(h.saveDir, "one.txt"), one)
	wantFile(t, filepath.Join(h.saveDir, "two.bin"), two)
	if from != "test sender" {
		t.Fatalf("receiver saw sender %q", from)
	}
}

func TestSendOnlyWhatWasAccepted(t *testing.T) {
	h := newHarness(t)
	h.rc.Accept = func(context.Context, Offer) []string { return []string{"1"} }
	paths := []string{writeFile(t, "no.txt", []byte("no")), writeFile(t, "yes.txt", []byte("yes"))}

	sent, err := newSender(t).Send(context.Background(), h.peer(), paths)
	if err != nil || sent != 1 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
	wantFile(t, filepath.Join(h.saveDir, "yes.txt"), []byte("yes"))
	if _, err := os.Stat(filepath.Join(h.saveDir, "no.txt")); err == nil {
		t.Fatal("a file the receiver did not accept was saved")
	}
}

func TestSendDeclinedAndBusy(t *testing.T) {
	h := newHarness(t)
	path := writeFile(t, "one.txt", []byte("x"))
	s := newSender(t)

	h.rc.Accept = func(context.Context, Offer) []string { return nil }
	if _, err := s.Send(context.Background(), h.peer(), []string{path}); !errors.Is(err, ErrDeclined) {
		t.Fatalf("declined offer: err %v", err)
	}

	h.rc.Accept = acceptAll
	h.prepare(meta("a", "held.txt", []byte("x"))) // an open session from someone else
	if _, err := s.Send(context.Background(), h.peer(), []string{path}); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy receiver: err %v", err)
	}
}

func TestSendRefusesAnImpostor(t *testing.T) {
	h := newHarness(t)
	offered := false
	h.rc.Accept = func(ctx context.Context, o Offer) []string {
		offered = true
		return acceptAll(ctx, o)
	}
	peer := h.peer()
	peer.Info.Fingerprint = "0000" // what we expected is not who answered

	if _, err := newSender(t).Send(context.Background(), peer, []string{writeFile(t, "secret.txt", []byte("x"))}); err == nil {
		t.Fatal("sent to a device whose certificate does not match")
	}
	if offered {
		t.Fatal("the impostor was told about the files")
	}
}

func TestSendRejectsFolders(t *testing.T) {
	h := newHarness(t)
	if _, err := newSender(t).Send(context.Background(), h.peer(), []string{t.TempDir()}); err == nil {
		t.Fatal("a folder was accepted for sending")
	}
}

func TestAsk(t *testing.T) {
	h := newHarness(t)
	got, err := newSender(t).Ask(context.Background(), h.peer())
	if err != nil || got.Fingerprint != h.rc.Info.Fingerprint {
		t.Fatalf("got %+v, err %v", got, err)
	}
	impostor := h.peer()
	impostor.Info.Fingerprint = "0000"
	if _, err := newSender(t).Ask(context.Background(), impostor); err == nil {
		t.Fatal("a device with the wrong certificate answered for another")
	}
}

func TestRegister(t *testing.T) {
	h := newHarness(t)
	var seen Info
	h.rc.Registered = func(peer Info, _ string) { seen = peer }
	got, err := newSender(t).Register(context.Background(), h.peer())
	if err != nil {
		t.Fatal(err)
	}
	if got.Alias != "test receiver" || seen.Alias != "test sender" {
		t.Fatalf("reply %+v, receiver saw %+v", got, seen)
	}
}

// The stock LocalSend app demands a client certificate and names the issuers
// it would like, which ours is not among. The sender must present its
// certificate anyway.
func TestSendToAReceiverThatDemandsACertificate(t *testing.T) {
	h := newHarness(t, func(c *tls.Config) {
		// Any certificate with a different issuer name than the sender's.
		other := httptest.NewTLSServer(nil)
		defer other.Close()
		c.ClientAuth = tls.RequireAnyClientCert
		c.ClientCAs = x509.NewCertPool()
		c.ClientCAs.AddCert(other.Certificate())
	})
	data := []byte("for a picky receiver")
	sent, err := newSender(t).Send(context.Background(), h.peer(), []string{writeFile(t, "one.txt", data)})
	if err != nil || sent != 1 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
	wantFile(t, filepath.Join(h.saveDir, "one.txt"), data)
}

// Another machine on the network that announces a device's fingerprint,
// saying it takes files without TLS on a port of its own, gets none of the
// files sent to that device.
func TestSendIgnoresAnAnnounceWithoutTLSForADeviceSeenWithIt(t *testing.T) {
	var mu sync.Mutex
	asked := 0
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		mu.Unlock()
		io.WriteString(w, `{"sessionId":"s","files":{"0":"t"}}`)
	}))
	port := ln.Addr().(*net.TCPAddr).Port

	h := newHarness(t)
	real := h.peer()
	reg := &Registry{Self: "me"}
	reg.Seen(real.Info, real.Host)
	impostor := real.Info
	impostor.Protocol, impostor.Port = "http", port
	reg.Seen(impostor, "127.0.0.2")

	peers := reg.List()
	if len(peers) != 1 || peers[0].Info.Port != real.Info.Port {
		t.Fatalf("the announce changed the list: %+v", peers)
	}
	data := []byte("the secret")
	sent, err := newSender(t).Send(context.Background(), peers[0], []string{writeFile(t, "secret.txt", data)})
	if err != nil || sent != 1 {
		t.Fatalf("sent %d, %v; want the file sent to the real device", sent, err)
	}
	wantFile(t, filepath.Join(h.saveDir, "secret.txt"), data)
	mu.Lock()
	defer mu.Unlock()
	if asked != 0 {
		t.Fatalf("the impostor was asked %d times", asked)
	}
}

// A refusal's text from the other end stays out of the error, which goes
// into the send log; the status code is kept.
func TestRefusalIsItsStatusCodeOnly(t *testing.T) {
	body := []byte("oops\n2026-01-01 00:00:00.000 a line of its own " + strings.Repeat("B", 1<<16))
	_, err := answered(http.StatusInternalServerError, body)
	if err == nil || err.Error() != "offer refused: HTTP 500" {
		t.Fatalf("error %q, want the status code only", err)
	}
	path := writeFile(t, "one.txt", []byte("x"))
	// An upload refused with a text of its own.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, string(body), http.StatusInternalServerError)
	}))
	defer srv.Close()
	err = upload(context.Background(), srv.Client(), srv.URL, path, 1, func(int) {})
	if err == nil || err.Error() != "upload: HTTP 500" {
		t.Fatalf("upload error %q, want the status code only", err)
	}
}

// An error on one file names it; Unnamed, for a log, does not.
func TestFailedFileIsNamedOutsideTheLog(t *testing.T) {
	h := newHarness(t)
	h.rc.Accept = acceptAll
	h.rc.SaveDir = writeFile(t, "not a folder", nil)
	path := writeFile(t, "private name.txt", []byte("x"))
	_, err := newSender(t).Send(context.Background(), h.peer(), []string{path})
	if err == nil || !strings.Contains(err.Error(), "private name.txt") {
		t.Fatalf("err %v, want one that names the file", err)
	}
	if text := Unnamed(err).Error(); strings.Contains(text, "private name") || !strings.Contains(text, "HTTP") {
		t.Fatalf("for the log: %q", text)
	}
	_, err = newSender(t).Send(context.Background(), h.peer(), []string{filepath.Join(t.TempDir(), "gone.txt")})
	if err == nil || !strings.Contains(err.Error(), "gone.txt") || strings.Contains(Unnamed(err).Error(), "gone") {
		t.Fatalf("a missing file: err %v, for the log %v", err, Unnamed(err))
	}
	// A file that goes after it was offered fails where it is opened, and
	// that error holds its whole path.
	_, err = os.Open(filepath.Join(t.TempDir(), "gone.txt"))
	err = &FileError{Name: "gone.txt", Err: err}
	if text := Unnamed(err).Error(); strings.Contains(text, "gone") || !strings.HasPrefix(text, "open: ") {
		t.Fatalf("a file that went, for the log: %q", text)
	}
}

// An answer that is not JSON gives an error without the answer's text, which
// the JSON error would repeat.
func TestBadAnswerIsNotRepeated(t *testing.T) {
	_, err := answered(http.StatusOK, []byte(`{"files": {"a\nforged line": 5}}`))
	if err == nil || strings.Contains(err.Error(), "forged") || strings.Contains(err.Error(), "\n") {
		t.Fatalf("err %q, want one without the answer's text", err)
	}
}
