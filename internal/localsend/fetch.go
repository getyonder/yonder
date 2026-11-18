package localsend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"time"
)

// This file is the receiving end of a send that goes the other way round:
// the sender offers, and the receiver fetches the files over its own
// connection to the sender. lender.go is the sending end. Two Yonders use it
// when only the receiver's connection may carry the files; LocalSend has no
// such send, and never sees one.
//
// The requests are those of LocalSend's download mode, prepare-download for
// the list of files and download for each file, with one more of Yonder's
// own at the end. What differs from that mode is who starts and who says
// yes: here the sender starts, with an ordinary offer that lists no files
// and carries a mark, and the receiver's answer to it is still the yes or
// no.

// FetchHeader carries the marks of such a send. On an offer it is the mark
// itself, the id the receiver fetches with; on the answers it is one of the
// words below. The app uses it too, to say that a device can fetch.
const FetchHeader = "Yonder-Fetch"

const (
	// fetchYes on the answer to a marked offer: the receiver fetches the
	// files it names.
	fetchYes = "1"
	// fetchNoPath on a refusal: the receiver has no way to the sender that
	// the files may take. Nobody was asked.
	fetchNoPath = "no-path"
	// answerPath is where the receiver gives its answer to a marked offer
	// on its own connection, beside the answer that the offer itself gets
	// on the sender's: the sender acts on the first to arrive. It is
	// Yonder's own, from 0.1.20; an older sender answers it with 404.
	answerPath = "/api/yonder/v1/answer"
	// tellWait is how long that answer may take to give.
	tellWait = 5 * time.Second
	// fetchedPath is where the receiver says that it has finished. It is
	// Yonder's own; LocalSend's download mode has no closing word.
	fetchedPath = "/api/yonder/v1/fetched"
	// listWait is how long the sender has to give its list of files.
	listWait = 30 * time.Second
	// borrowWait is how long finding the way to the sender and fetching its
	// list may take together. Nobody has been asked yet, and no other offer
	// is heard meanwhile.
	borrowWait = 20 * time.Second
)

// errNoList means the sender would not give its list of files: by its own
// look, this device has no way to it that the files may take.
var errNoList = errors.New("the sender gave no list of files")

// IsQuestion reports whether a request is the question Sender.Ask puts.
func IsQuestion(r *http.Request) bool {
	return r.Method == "GET" && r.URL.Path == apiPrefix+"/info"
}

// Source is the way to the sender of a marked offer.
type Source struct {
	// Sender and Peer reach the device that made the offer.
	Sender *Sender
	Peer   Peer
	// Close, if set, is called once, when the way is no longer used.
	Close func()
}

func (src *Source) close() {
	if src.Close != nil {
		src.Close()
	}
}

// downloadList is the answer to prepare-download.
type downloadList struct {
	Info      Info                `json:"info"`
	SessionID string              `json:"sessionId"`
	Files     map[string]FileMeta `json:"files"`
}

// borrow finds the way to the sender of a marked offer and fetches its list
// of files, which is checked as the list in an ordinary offer is. On failure
// it answers the sender itself. An offer refused for want of a way is not
// counted against the sender's address as an offer, since it raised no
// question, but against the limit of such refusals (see noPathBurst).
func (rc *Receiver) borrow(ctx context.Context, w http.ResponseWriter, o Offer) (*Source, map[string]FileMeta, bool) {
	ctx, cancel := context.WithTimeout(ctx, rc.borrowTime())
	defer cancel()
	noPath := func() (*Source, map[string]FileMeta, bool) {
		rc.forgetOffer(o.Remote)
		w.Header().Set(FetchHeader, fetchNoPath)
		fail(w, http.StatusForbidden, "no direct path")
		return nil, nil, false
	}
	src, err := rc.Source(ctx, o)
	if err != nil {
		return noPath()
	}
	start := time.Now()
	files, err := src.Sender.list(ctx, src.Peer, o.Fetch)
	rc.tracef("fetch: the list of files ended after %v, files: %d, error: %v", time.Since(start).Round(10*time.Millisecond), len(files), err)
	if err == nil {
		if problem := checkFiles(files); problem != "" {
			err = errors.New(problem)
		}
	}
	if err != nil {
		src.close()
		if errors.Is(err, errNoList) {
			return noPath()
		}
		fail(w, http.StatusBadGateway, "the list of files: "+err.Error())
		return nil, nil, false
	}
	return src, files, true
}

func (rc *Receiver) borrowTime() time.Duration {
	if rc.BorrowTime > 0 {
		return rc.BorrowTime
	}
	return borrowWait
}

// fetch gets the accepted files of s from the sender, one after the other,
// until all are saved, one fails, or ctx ends, which it does when the
// session is ended. Then it tells the sender how it went.
func (rc *Receiver) fetch(ctx context.Context, s *session, src *Source, mark string, answer prepareResponse) {
	defer src.close()
	// The sender gives a file only once it has the answer, so it is told
	// before the first file is asked for.
	rc.tell(src, mark, answer)
	c := src.Sender.client(src.Peer)
	defer c.CloseIdleConnections()
	api := src.Peer.baseURL()

	rc.mu.Lock()
	var ids []string
	for id := range s.files {
		ids = append(ids, id)
	}
	rc.mu.Unlock()
	sort.Strings(ids)

	complete := true
	for _, id := range ids {
		if !rc.fetchOne(ctx, c, api, s, mark, id) {
			complete = false
			break
		}
	}
	if !complete {
		rc.mu.Lock()
		rc.end(s, "a file was not fetched")
		rc.mu.Unlock()
	}
	// The closing word goes out even if ctx is what ended.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	q := url.Values{"sessionId": {mark}, "complete": {"0"}}
	if complete {
		q.Set("complete", "1")
	}
	status, _, err := post(cctx, c, api+fetchedPath+"?"+q.Encode(), "application/json", nil, 0)
	cancel()
	rc.tracef("fetch: closing word given, complete: %v (HTTP %d, error: %v)", complete, status, err)
}

// tell gives the sender of a marked offer the answer to it, yes with the
// files taken or no with none, on this device's own connection to the
// sender. The same answer goes back on the sender's connection, as the
// answer to its offer; that connection may be on the relay and slow.
func (rc *Receiver) tell(src *Source, mark string, answer prepareResponse) {
	ctx, cancel := context.WithTimeout(context.Background(), tellWait)
	defer cancel()
	c := src.Sender.client(src.Peer)
	defer c.CloseIdleConnections()
	start := time.Now()
	q := url.Values{"sessionId": {mark}}
	status, _, err := postJSON(ctx, c, src.Peer.baseURL()+answerPath+"?"+q.Encode(), answer)
	rc.tracef("fetch: answer given on our connection after %v, files taken: %d (HTTP %d, error: %v)", time.Since(start).Round(10*time.Millisecond), len(answer.Files), status, err)
}

// fetchOne gets one accepted file and saves it, and reports whether it did.
func (rc *Receiver) fetchOne(ctx context.Context, c *http.Client, api string, s *session, mark, id string) bool {
	rc.mu.Lock()
	f := s.files[id]
	if rc.session != s || f == nil || f.started {
		rc.mu.Unlock()
		return false
	}
	f.started = true
	s.inFlight++
	rc.mu.Unlock()

	q := url.Values{"sessionId": {mark}, "fileId": {id}}
	body := &asked{ask: func() (io.ReadCloser, error) {
		return download(ctx, c, api+apiPrefix+"/download?"+q.Encode())
	}}
	defer body.close()
	_, err := rc.take(s, f, body, nil)
	return err == nil
}

// asked is a file that is asked for when it is first read, so that the wait
// for the sender's answer is inside take's quiet time. A file that could
// not be asked for fails at that read.
type asked struct {
	ask func() (io.ReadCloser, error)
	r   io.ReadCloser
	err error
}

func (a *asked) Read(b []byte) (int, error) {
	if a.r == nil && a.err == nil {
		a.r, a.err = a.ask()
	}
	if a.err != nil {
		return 0, a.err
	}
	return a.r.Read(b)
}

func (a *asked) close() {
	if a.r != nil {
		a.r.Close()
	}
}

// list asks the sender of a marked offer for its list of files.
func (s *Sender) list(ctx context.Context, to Peer, mark string) (map[string]FileMeta, error) {
	ctx, cancel := context.WithTimeout(ctx, listWait)
	defer cancel()
	c := s.client(to)
	defer c.CloseIdleConnections()
	q := url.Values{"sessionId": {mark}}
	status, body, err := post(ctx, c, to.baseURL()+apiPrefix+"/prepare-download?"+q.Encode(), "application/json", nil, 0)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, errNoList
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", status)
	}
	var list downloadList
	if err := json.Unmarshal(body, &list); err != nil {
		// Without the JSON error's text, which repeats what was sent.
		return nil, errors.New("not valid JSON")
	}
	return list.Files, nil
}

// download asks for one file and returns its bytes as they arrive.
func download(ctx context.Context, c *http.Client, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}
