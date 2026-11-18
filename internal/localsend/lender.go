package localsend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// This file is the sending end of a send that the receiver fetches; fetch.go
// says why there is one, and is the receiving end.

var (
	// ErrFetchRefused means the receiver was asked to fetch the files and
	// found no way to this device that the files may take. Its person was
	// not asked.
	ErrFetchRefused = errors.New("the receiver found no way to fetch the files")
	// ErrFetchStopped means the receiver stopped fetching before it had
	// every file: its person cancelled, or a file could not be saved.
	ErrFetchStopped = errors.New("the receiver stopped the transfer")
	// ErrFetchQuiet means the receiver said yes and then stopped asking
	// for the files.
	ErrFetchQuiet = errors.New("the receiver stopped asking for the files")
)

const (
	// lendIdle is how long a receiver that has said yes may go without a
	// request, and without a file on the move, before the send gives up.
	lendIdle = time.Minute
	// answerWait is how long a request for a file waits for the receiver's
	// yes to arrive here: the receiver starts to fetch as it answers, and
	// its first request can come in before its answer does.
	answerWait = 10 * time.Second
	// closingShare is the part of the idle time that a receiver with every
	// file has to say its closing word, before the send counts as done
	// without it.
	closingShare = 6
)

// Lender gives out the files of the sends that receivers fetch (Sender.Lend),
// and only those: each file to the receiver it was offered to, if that
// receiver took it, once, and until the send ends. Every other request gets
// 404. It serves nothing by itself; a Receiver passes the requests to it.
type Lender struct {
	// IdleTime, if set, replaces the default time a receiver that has said
	// yes may go without a request.
	IdleTime time.Duration
	// Trace, if set, is told each step of a send that is fetched, for a
	// log.
	Trace func(format string, args ...any)

	mu    sync.Mutex
	loans map[string]*loan
}

// loan is one send that the receiver fetches.
type loan struct {
	id     string
	info   Info
	files  map[string]FileMeta
	paths  map[string]string // by file id
	allow  func(*http.Request) bool
	report func(Progress)

	trace func(format string, args ...any)

	said     chan prepareResponse // the receiver's answer, given on its own connection
	answered chan struct{}        // closed when the receiver's answer is in, or the loan is over
	done     chan bool            // the receiver's closing word: whether it has every file
	ended    atomic.Bool          // stops files still going out

	mu       sync.Mutex
	listed   bool
	accepted map[string]bool // nil until the receiver has answered
	taken    map[string]bool
	inFlight int
	lastUsed time.Time
	sent     int
	progress Progress
}

func (l *Lender) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	ln := l.loans[r.URL.Query().Get("sessionId")]
	l.mu.Unlock()
	// One answer for everything that is not served, so that nothing can be
	// learned from the difference.
	if ln == nil || !ln.allow(r) {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "POST " + apiPrefix + "/prepare-download":
		ln.list(w)
	case "GET " + apiPrefix + "/download":
		ln.give(w, r)
	case "POST " + answerPath:
		ln.say(w, r)
	case "POST " + fetchedPath:
		ln.close(w, r)
	default:
		fail(w, http.StatusNotFound, "not found")
	}
}

// list gives the list of files, once.
func (ln *loan) list(w http.ResponseWriter) {
	ln.mu.Lock()
	again := ln.listed
	ln.listed = true
	ln.mu.Unlock()
	if again {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	ln.trace("lend: list of files given")
	writeJSON(w, downloadList{Info: ln.info.reply(), SessionID: ln.id, Files: ln.files})
}

// say takes the receiver's answer to the offer, given on its own connection.
// Lend acts on it if it is there before the answer that the offer gets.
func (ln *loan) say(w http.ResponseWriter, r *http.Request) {
	var answer prepareResponse
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxOfferBody)).Decode(&answer); err != nil {
		fail(w, http.StatusBadRequest, "invalid body")
		return
	}
	select {
	case ln.said <- answer:
	default: // said already
	}
	w.WriteHeader(http.StatusOK)
}

// give sends one file that the receiver took, once.
func (ln *loan) give(w http.ResponseWriter, r *http.Request) {
	wait := time.NewTimer(answerWait)
	defer wait.Stop()
	select {
	case <-ln.answered:
	case <-wait.C:
	case <-r.Context().Done():
	}
	id := r.URL.Query().Get("fileId")
	ln.mu.Lock()
	ok := ln.accepted[id] && !ln.taken[id] && !ln.ended.Load()
	if ok {
		ln.taken[id] = true
		ln.inFlight++
	}
	ln.mu.Unlock()
	if !ok {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	defer func() {
		ln.mu.Lock()
		ln.inFlight--
		ln.lastUsed = time.Now()
		ln.mu.Unlock()
	}()

	meta := ln.files[id]
	f, err := os.Open(ln.paths[id])
	if err != nil {
		fail(w, http.StatusNotFound, "not found")
		return
	}
	defer f.Close()
	body := &meter{r: io.LimitReader(f, meta.Size), stop: ln.ended.Load}
	body.count = func(n int) {
		ln.mu.Lock()
		ln.progress.File = meta.FileName
		ln.progress.Done += int64(n)
		progress := ln.progress
		ln.mu.Unlock()
		ln.report(progress)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	// A file that is now shorter than was offered ends short of the length
	// given, which the receiver takes as a failure.
	start := time.Now()
	n, err := io.Copy(w, body)
	if err == nil && n == meta.Size {
		ln.mu.Lock()
		ln.sent++
		ln.mu.Unlock()
	}
	ln.trace("lend: a file given, %d of %d bytes after %v (%s), error: %v", n, meta.Size, time.Since(start).Round(10*time.Millisecond), rate(n, time.Since(start)), Unnamed(err))
}

// close takes the receiver's closing word.
func (ln *loan) close(w http.ResponseWriter, r *http.Request) {
	select {
	case ln.done <- r.URL.Query().Get("complete") == "1":
	default: // said already
	}
	ln.trace("lend: closing word, complete: %v", r.URL.Query().Get("complete") == "1")
	w.WriteHeader(http.StatusOK)
}

// answer records which files the receiver took, when its yes arrives.
func (ln *loan) answer(ids map[string]string) {
	ln.mu.Lock()
	ln.accepted = map[string]bool{}
	for id := range ids {
		if f, ok := ln.files[id]; ok {
			ln.accepted[id] = true
			ln.progress.Total += f.Size
		}
	}
	ln.lastUsed = time.Now()
	progress := ln.progress
	ln.mu.Unlock()
	ln.report(progress)
	close(ln.answered)
}

// open starts a loan, and shut ends it.
func (l *Lender) open(ln *loan) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loans == nil {
		l.loans = map[string]*loan{}
	}
	l.loans[ln.id] = ln
}

func (l *Lender) shut(ln *loan) {
	l.mu.Lock()
	delete(l.loans, ln.id)
	l.mu.Unlock()
	ln.ended.Store(true)
	ln.mu.Lock()
	if ln.accepted == nil {
		ln.accepted = map[string]bool{}
		close(ln.answered)
	}
	ln.mu.Unlock()
}

func (l *Lender) tracef(format string, args ...any) {
	if l.Trace != nil {
		l.Trace(format, args...)
	}
}

func (l *Lender) idleTime() time.Duration {
	if l.IdleTime > 0 {
		return l.IdleTime
	}
	return lendIdle
}

// Lend offers the files at paths to a peer for it to fetch from l, where
// Send would upload them, and returns how many it fetched. The offer goes
// the same way as Send's, but lists no files: the receiver asks l for the
// list, and then for each file it takes, over a connection of its own.
// allow is asked about every such request, and only what it allows is
// served; it is how the caller holds the files to one device and to a path
// they may take. Lend ends when the receiver says that it has finished,
// when it stops asking, or when ctx ends.
func (s *Sender) Lend(ctx context.Context, to Peer, paths []string, l *Lender, allow func(*http.Request) bool) (int, error) {
	files, err := describe(paths)
	if err != nil {
		return 0, err
	}
	ln := &loan{
		id: randomID(), info: s.Info, files: files, paths: map[string]string{}, allow: allow, report: reporter(ctx),
		trace: l.tracef, said: make(chan prepareResponse, 1),
		answered: make(chan struct{}), done: make(chan bool, 1), taken: map[string]bool{},
	}
	for i, p := range paths {
		ln.paths[strconv.Itoa(i)] = p
	}
	l.open(ln)
	defer l.shut(ln)

	c := s.client(to)
	defer c.CloseIdleConnections()
	api := to.baseURL() + apiPrefix
	offer, err := json.Marshal(prepareRequest{Info: s.Info, Files: map[string]FileMeta{}})
	if err != nil {
		return 0, err
	}
	header := s.offerHeader()
	header.Set(FetchHeader, ln.id)
	// The answer comes two ways: as the answer to the offer, on this
	// device's connection, which may be on the relay and slow; and from a
	// receiver of 0.1.20 or later on its own connection too (see say). The
	// first to arrive counts.
	type offerReply struct {
		status int
		header http.Header
		body   []byte
		err    error
	}
	replied := make(chan offerReply, 1)
	octx, drop := context.WithCancel(ctx)
	defer drop()
	started := time.Now()
	l.tracef("lend: offer made")
	go func() {
		status, reply, body, err := postWith(octx, c, api+"/prepare-upload", header, bytes.NewReader(offer), int64(len(offer)))
		replied <- offerReply{status, reply, body, err}
	}()
	var session *prepareResponse
	select {
	case said := <-ln.said:
		l.tracef("lend: answer came on the receiver's connection after %v, files taken: %d", time.Since(started).Round(10*time.Millisecond), len(said.Files))
		if len(said.Files) == 0 {
			return 0, ErrDeclined
		}
		session = &said
	case r := <-replied:
		l.tracef("lend: answer came on our connection after %v (HTTP %d, error: %v)", time.Since(started).Round(10*time.Millisecond), r.status, r.err)
		if r.err != nil {
			return 0, r.err
		}
		if r.status == http.StatusForbidden && r.header.Get(FetchHeader) == fetchNoPath {
			return 0, ErrFetchRefused
		}
		if session, err = answered(r.status, r.body); session == nil {
			return 0, err
		}
		if r.header.Get(FetchHeader) != fetchYes {
			// It took the offer for an ordinary one, and waits for uploads.
			callOff(ctx, c, api, session.SessionID)
			return 0, errors.New("the receiver cannot fetch files")
		}
	}
	ln.answer(session.Files)

	sent := func() int {
		ln.mu.Lock()
		defer ln.mu.Unlock()
		return ln.sent
	}
	idle := l.idleTime()
	tick := time.NewTicker(idle / 4)
	defer tick.Stop()
	for {
		select {
		case complete := <-ln.done:
			if !complete {
				return sent(), ErrFetchStopped
			}
			// Its word is that it has them all, though the last request
			// here may not have returned yet.
			return len(ln.accepted), nil
		case <-ctx.Done():
			callOff(ctx, c, api, session.SessionID)
			return sent(), ctx.Err()
		case <-tick.C:
			ln.mu.Lock()
			still := time.Since(ln.lastUsed)
			if ln.inFlight > 0 {
				still = 0
			}
			all := ln.sent == len(ln.accepted)
			ln.mu.Unlock()
			// Every file went out in full and the closing word, which
			// follows the last file at once, has not come: it was lost.
			if all && still > idle/closingShare {
				return sent(), nil
			}
			if quiet := still > idle; quiet {
				callOff(ctx, c, api, session.SessionID)
				return sent(), ErrFetchQuiet
			}
		}
	}
}
