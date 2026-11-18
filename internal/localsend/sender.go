package localsend

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"yonder/internal/identity"
)

var (
	// ErrDeclined means the receiver said no to the offer.
	ErrDeclined = errors.New("the receiver declined")
	// ErrBusy means the receiver is in the middle of another transfer.
	ErrBusy = errors.New("the receiver is busy with another transfer")
	// ErrNotReached means no connection to the receiver could be made at
	// any of its addresses, so nothing was sent. The error that carries it
	// reads as the failure at the best address.
	ErrNotReached = errors.New("the receiver could not be reached")
)

// notReached marks the failure at a peer's best address as ErrNotReached.
type notReached struct{ error }

func (e notReached) Unwrap() error      { return e.error }
func (notReached) Is(target error) bool { return target == ErrNotReached }

// Sender is the sending half of the Upload API.
type Sender struct {
	// Info is how this device describes itself.
	Info Info
	// Cert is presented to receivers, which LocalSend 1.18 insists on.
	Cert tls.Certificate
	// Dial, if set, replaces the ordinary network dialler; the tunnel
	// uses it to reach devices on other networks.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Reached, if set, is told how each connection to one of a peer's
	// addresses went, so the best one is tried first next time. It is not
	// called when Dial is set.
	Reached func(fingerprint, host string, ok bool)
	// AskHeader, if set, goes with each Ask: what Yonder devices tell each
	// other that LocalSend has no word for.
	AskHeader http.Header
	// OfferHeader, if set, goes with each offer of files, for the same.
	OfferHeader http.Header
}

// How long one address of a peer gets to take a connection: tryWait while
// another is still to be tried, lastWait for the last.
const (
	tryWait = 1500 * time.Millisecond
	// A firewall that drops what comes in never answers, and the system
	// would wait some two minutes.
	lastWait = 10 * time.Second
)

// client makes an HTTP client that only talks to one peer, at whichever of
// its addresses takes the connection. Peers are self-signed, so the
// fingerprint is the check. An empty fingerprint means no check, which is
// only right when Dial already guarantees who is at the other end, as the
// tunnel does.
func (s *Sender) client(to Peer) *http.Client {
	fingerprint := to.Info.Fingerprint
	conf := &tls.Config{
		// Always present our certificate. With Certificates, Go holds it
		// back when the receiver names issuers it would accept, as the
		// stock LocalSend app does, and the receiver then hangs up.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &s.Cert, nil
		},
		InsecureSkipVerify: true, // replaced by the fingerprint check below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if fingerprint == "" {
				return nil
			}
			if len(cs.PeerCertificates) == 0 || !strings.EqualFold(identity.Fingerprint(cs.PeerCertificates[0].Raw), fingerprint) {
				return errors.New("the device's certificate does not match its fingerprint")
			}
			return nil
		},
	}
	t := &http.Transport{DialContext: s.Dial, TLSClientConfig: conf}
	if s.Dial == nil {
		t.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) { return s.connect(ctx, to, addr, nil) }
		t.DialTLSContext = func(ctx context.Context, _, addr string) (net.Conn, error) { return s.connect(ctx, to, addr, conf) }
	}
	return &http.Client{Transport: t}
}

// connect opens a connection to a peer at the first of its addresses that
// takes one, best first; addr only gives the port. With conf the address
// must also answer with the peer's certificate, so another machine at an
// address the peer was wrongly seen at is passed over like a dead one. If
// none works, the error is the best address's, as ErrNotReached.
func (s *Sender) connect(ctx context.Context, to Peer, addr string, conf *tls.Config) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	hosts := to.Hosts()
	var first error
	for i, host := range hosts {
		wait := tryWait
		if i == len(hosts)-1 {
			wait = lastWait
		}
		actx, cancel := context.WithTimeout(ctx, wait)
		var c net.Conn
		if conf == nil {
			c, err = (&net.Dialer{}).DialContext(actx, "tcp", net.JoinHostPort(host, port))
		} else {
			c, err = (&tls.Dialer{Config: conf}).DialContext(actx, "tcp", net.JoinHostPort(host, port))
		}
		cancel()
		if ctx.Err() != nil {
			// The caller gave up, which says nothing about the address.
			if err == nil {
				c.Close()
			}
			return nil, ctx.Err()
		}
		if s.Reached != nil {
			s.Reached(to.Info.Fingerprint, host, err == nil)
		}
		if err == nil {
			return c, nil
		}
		if first == nil {
			first = err
		}
	}
	return nil, notReached{first}
}

// post sends a JSON or binary body and returns the status and reply.
func post(ctx context.Context, c *http.Client, url, contentType string, body io.Reader, size int64) (int, []byte, error) {
	status, _, b, err := postWith(ctx, c, url, http.Header{"Content-Type": {contentType}}, body, size)
	return status, b, err
}

// postWith is post with the request's headers given and the reply's
// returned.
func postWith(ctx context.Context, c *http.Client, url string, header http.Header, body io.Reader, size int64) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, body)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header = header
	req.ContentLength = size
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header, b, err
}

func postJSON(ctx context.Context, c *http.Client, url string, v any) (int, []byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return 0, nil, err
	}
	return post(ctx, c, url, "application/json", bytes.NewReader(body), int64(len(body)))
}

// Register introduces this device to a peer and returns the peer's reply.
func (s *Sender) Register(ctx context.Context, to Peer) (Info, error) {
	c := s.client(to)
	defer c.CloseIdleConnections()
	status, body, err := postJSON(ctx, c, to.baseURL()+apiPrefix+"/register", s.Info)
	if err != nil {
		return Info{}, err
	}
	if status != http.StatusOK {
		return Info{}, fmt.Errorf("register: HTTP %d", status)
	}
	var info Info
	return info, json.Unmarshal(body, &info)
}

// Ask fetches a peer's description, which also shows it is there and is
// who its fingerprint says.
func (s *Sender) Ask(ctx context.Context, to Peer) (Info, error) {
	info, _, err := s.Question(ctx, to, nil)
	return info, err
}

// offerHeader is the headers an offer of files goes with.
func (s *Sender) offerHeader() http.Header {
	header := http.Header{"Content-Type": {"application/json"}}
	for k, v := range s.OfferHeader {
		header[k] = v
	}
	return header
}

// Question is Ask with more headers to send, beside AskHeader, and with the
// headers of the answer returned: both are for what Yonder devices tell
// each other.
func (s *Sender) Question(ctx context.Context, to Peer, more http.Header) (Info, http.Header, error) {
	c := s.client(to)
	defer c.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, "GET", to.baseURL()+apiPrefix+"/info", nil)
	if err != nil {
		return Info{}, nil, err
	}
	for _, header := range []http.Header{s.AskHeader, more} {
		for k, v := range header {
			req.Header[k] = v
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		return Info{}, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Info{}, nil, fmt.Errorf("info: HTTP %d", resp.StatusCode)
	}
	var info Info
	return info, resp.Header, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info)
}

// Post sends body to a path of a peer that is not LocalSend's, with the
// same check of the peer's certificate, and returns the status and the
// reply: it is for what Yonder devices ask each other.
func (s *Sender) Post(ctx context.Context, to Peer, path string, body []byte) (int, []byte, error) {
	c := s.client(to)
	defer c.CloseIdleConnections()
	return post(ctx, c, to.baseURL()+path, "application/octet-stream", bytes.NewReader(body), int64(len(body)))
}

// Send offers the files at paths to a peer and uploads the ones it accepts.
// It returns how many were sent. Waiting for the other side to decide can
// take as long as ctx allows.
func (s *Sender) Send(ctx context.Context, to Peer, paths []string) (int, error) {
	offer := prepareRequest{Info: s.Info}
	var err error
	if offer.Files, err = describe(paths); err != nil {
		return 0, err
	}

	c := s.client(to)
	defer c.CloseIdleConnections()
	api := to.baseURL() + apiPrefix
	said, err := json.Marshal(offer)
	if err != nil {
		return 0, err
	}
	status, _, body, err := postWith(ctx, c, api+"/prepare-upload", s.offerHeader(), bytes.NewReader(said), int64(len(said)))
	if err != nil {
		return 0, err
	}
	session, err := answered(status, body)
	if session == nil {
		return 0, err
	}

	report := reporter(ctx)
	var progress Progress
	for id := range session.Files {
		progress.Total += offer.Files[id].Size
	}
	report(progress)

	sent := 0
	for i, p := range paths {
		id := strconv.Itoa(i)
		token, accepted := session.Files[id]
		if !accepted {
			continue
		}
		progress.File = offer.Files[id].FileName
		count := func(n int) {
			progress.Done += int64(n)
			report(progress)
		}
		q := url.Values{"sessionId": {session.SessionID}, "fileId": {id}, "token": {token}}
		if err := upload(ctx, c, api+"/upload?"+q.Encode(), p, offer.Files[id].Size, count); err != nil {
			callOff(ctx, c, api, session.SessionID)
			return sent, &FileError{Name: filepath.Base(p), Err: err}
		}
		sent++
	}
	return sent, nil
}

// describe makes the list of files of an offer of the files at paths. A
// file's id is its place in paths.
func describe(paths []string) (map[string]FileMeta, error) {
	files := map[string]FileMeta{}
	for i, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				err = pe.Err // the path is in the FileError
			}
			return nil, &FileError{Name: p, Err: err}
		}
		if st.IsDir() {
			return nil, &FileError{Name: p, Err: errors.New("it is a folder; only files can be sent for now")}
		}
		if !st.Mode().IsRegular() {
			// A named pipe or a device would hold the send for ever when
			// it is opened. A link counts as what it points to.
			return nil, &FileError{Name: p, Err: errors.New("it is not an ordinary file; only ordinary files can be sent")}
		}
		fileType := mime.TypeByExtension(filepath.Ext(p))
		if fileType == "" {
			fileType = "application/octet-stream"
		}
		id := strconv.Itoa(i)
		files[id] = FileMeta{ID: id, FileName: filepath.Base(p), Size: st.Size(), FileType: fileType}
	}
	return files, nil
}

// FileError is a send that failed on one of its files. Its text names the
// file, for the person; Unnamed leaves the name out, for a log.
type FileError struct {
	Name string // the file's name, or its path as the person chose it
	Err  error
}

func (e *FileError) Error() string { return e.Name + ": " + e.Err.Error() }
func (e *FileError) Unwrap() error { return e.Err }

// Unnamed is err as a log may hold it: if it failed on a file, without the
// file's name, and without the path that an error of the file system holds.
func Unnamed(err error) error {
	var fe *FileError
	if errors.As(err, &fe) {
		err = fe.Err
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}

// answered reads the answer to an offer: the session, or why there is none.
// A receiver that needs none of the files gives neither.
func answered(status int, body []byte) (*prepareResponse, error) {
	switch status {
	case http.StatusOK:
	case http.StatusNoContent:
		return nil, nil
	case http.StatusForbidden:
		return nil, ErrDeclined
	case http.StatusConflict:
		return nil, ErrBusy
	default:
		// The other end's text is left out: the error goes into the log.
		return nil, fmt.Errorf("offer refused: HTTP %d", status)
	}
	var session prepareResponse
	if err := json.Unmarshal(body, &session); err != nil {
		// Without the JSON error's text, which repeats what was sent.
		return nil, errors.New("offer reply: not valid JSON")
	}
	return &session, nil
}

// reporter is where a send reports its progress: see WithProgress.
func reporter(ctx context.Context) func(Progress) {
	if report, _ := ctx.Value(progressKey{}).(func(Progress)); report != nil {
		return report
	}
	return func(Progress) {}
}

// callOff tells the receiver that the session is over, so that it stops
// waiting, even if ctx is what ended.
func callOff(ctx context.Context, c *http.Client, api, session string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	post(ctx, c, api+"/cancel?"+url.Values{"sessionId": {session}}.Encode(), "application/json", nil, 0)
}

// upload sends one file, calling count with the size of each piece read.
func upload(ctx context.Context, c *http.Client, url, path string, size int64, count func(n int)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	status, _, err := post(ctx, c, url, "application/octet-stream", &meter{r: f, count: count}, size)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("upload: HTTP %d", status)
	}
	return nil
}
