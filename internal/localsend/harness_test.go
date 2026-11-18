package localsend

// The one test harness for this package: a Receiver served over TLS on a
// free port that the test server holds from the start, with its own save
// folder and identity. Nothing here is shared between tests.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"yonder/internal/identity"
)

type harness struct {
	t       *testing.T
	rc      *Receiver
	url     string
	client  *http.Client
	saveDir string
	port    int
}

// acceptAll takes every offered file.
func acceptAll(_ context.Context, o Offer) []string {
	var ids []string
	for _, f := range o.Files {
		ids = append(ids, f.ID)
	}
	return ids
}

// wait is the most a test waits for something that another goroutine does;
// it is only against a hang.
const wait = 10 * time.Second

// until waits, at most wait, for cond to hold.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v for %s", wait, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// newHarness starts a receiver. tune, if given, adjusts its TLS settings
// before it starts listening.
func newHarness(t *testing.T, tune ...func(*tls.Config)) *harness {
	t.Helper()
	id, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, saveDir: t.TempDir()}
	h.rc = &Receiver{
		Info:    Info{Alias: "test receiver", Version: Version, Fingerprint: id.Fingerprint},
		SaveDir: h.saveDir,
		Accept:  acceptAll,
	}
	srv := httptest.NewUnstartedServer(h.rc)
	srv.TLS = id.TLSConfig()
	for _, f := range tune {
		f(srv.TLS)
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	h.url = srv.URL
	h.port = srv.Listener.Addr().(*net.TCPAddr).Port
	h.client = &http.Client{Transport: &http.Transport{
		// Peers are self-signed; a sender checks the fingerprint instead.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	t.Cleanup(h.client.CloseIdleConnections)
	return h
}

// do sends a request and returns the status and body.
func (h *harness) do(method, path string, body []byte) (int, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.url+apiPrefix+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// meta describes data as a file offer, with a correct hash.
func meta(id, name string, data []byte) FileMeta {
	sum := sha256.Sum256(data)
	return FileMeta{ID: id, FileName: name, Size: int64(len(data)), FileType: "application/octet-stream", SHA256: hex.EncodeToString(sum[:])}
}

// prepare offers files and returns the status and, on 200, the session.
func (h *harness) prepare(files ...FileMeta) (int, prepareResponse) {
	h.t.Helper()
	req := prepareRequest{Info: Info{Alias: "test sender", Version: Version, Fingerprint: "sender"}, Files: map[string]FileMeta{}}
	for _, f := range files {
		req.Files[f.ID] = f
	}
	body, _ := json.Marshal(req)
	status, b := h.do("POST", "/prepare-upload", body)
	var resp prepareResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(b, &resp); err != nil {
			h.t.Fatalf("prepare-upload reply: %v", err)
		}
	}
	return status, resp
}

// upload sends one file's bytes and returns the status.
func (h *harness) upload(session, fileID, token string, data []byte) int {
	h.t.Helper()
	q := url.Values{"sessionId": {session}, "fileId": {fileID}, "token": {token}}
	status, _ := h.do("POST", fmt.Sprintf("/upload?%s", q.Encode()), data)
	return status
}

// peer is the harness receiver as a sender or discovery would have found it.
func (h *harness) peer() Peer {
	info := h.rc.Info
	info.Port, info.Protocol = h.port, "https"
	return Peer{Info: info, Host: "127.0.0.1"}
}

// newSender makes a sender with its own identity.
func newSender(t *testing.T) *Sender {
	t.Helper()
	id, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Sender{
		Info: Info{Alias: "test sender", Version: Version, Fingerprint: id.Fingerprint, Port: 1, Protocol: "https"},
		Cert: id.Cert,
	}
}

// writeFile puts data in a fresh folder and returns the file's path.
func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// newLender starts a second device, one that lends files: a Lender served as
// the harness's receiver is. It returns the Lender and the device as one
// that fetches from it reaches it.
func newLender(t *testing.T) (*Lender, Peer) {
	t.Helper()
	id, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := &Lender{}
	info := Info{Alias: "test lender", Version: Version, Fingerprint: id.Fingerprint}
	srv := httptest.NewUnstartedServer(&Receiver{Info: info, Lender: l})
	srv.TLS = id.TLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	info.Port, info.Protocol = srv.Listener.Addr().(*net.TCPAddr).Port, "https"
	return l, Peer{Info: info, Host: "127.0.0.1"}
}

// fetchFrom makes the harness's receiver fetch the files of marked offers
// from the device at from.
func (h *harness) fetchFrom(from Peer) {
	h.rc.Source = func(context.Context, Offer) (*Source, error) {
		return &Source{Sender: newSender(h.t), Peer: from}, nil
	}
}

// allowAll lets a Lender serve every request for a loan.
func allowAll(*http.Request) bool { return true }
