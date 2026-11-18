package localsend

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"yonder/internal/identity"
	"yonder/internal/names"
)

// sessionIdle is how long a session with no upload in flight blocks new ones.
const sessionIdle = 2 * time.Minute

// acceptTimeout is how long a person has to answer "accept?" before the
// offer is declined and the slot freed; Receiver.AcceptTimeout overrides it.
const acceptTimeout = 5 * time.Minute

// quietWait is how long a file that is arriving may go without one more
// byte before its transfer is stopped; Receiver.QuietWait overrides it. A
// sender can stop with no word: a web page frozen by a phone's locked
// screen, a device that left its network.
const quietWait = time.Minute

// An address may make offerBurst offers in offerWindow: each raises a
// question for the person, so a stream of them is a nuisance and not a use.
// At most maxOfferSources addresses are tracked at once. A marked offer
// refused for want of a path raised no question and is not counted so; it
// counts instead against noPathBurst in offerWindow, which leaves room for an
// honest try or two again but stops a stream of them, each of which holds
// the receive session while this device looks for the way to the sender.
const (
	offerBurst      = 5
	noPathBurst     = 3
	offerWindow     = time.Minute
	maxOfferSources = 1024
)

// offerReadTime is how long a sender has to deliver its offer.
const offerReadTime = 30 * time.Second

// Limits on one offer. An offer is read while the app still has nobody's
// consent, so what it may hold in memory is small. A preview is only a
// thumbnail; one over maxPreview is dropped, the file is still offered. A
// file name is shown in the question whole, so it is refused when it is
// longer than a disk takes: maxNamePart bytes for each of its parts,
// maxFileName for all of it. maxInfoBody is the most a device may send to
// say who it is.
const (
	maxInfoBody   = 16 << 10
	maxFileName   = 1024
	maxNamePart   = 255
	maxOfferBody  = 1 << 20
	maxOfferReads = 16 // offers read at once
	maxOfferFiles = 1000
	maxPreview    = 64 << 10
)

// Offer is a sender asking to send files.
type Offer struct {
	Sender Info
	Remote string // the sender's address, without port
	Files  []FileMeta
	// CertFingerprint is the fingerprint of the certificate the sender
	// proved it holds, or empty if it presented none. Unlike Sender, it
	// cannot be made up.
	CertFingerprint string
	// Fetch is the mark of an offer whose files this device is to fetch
	// from the sender (see Receiver.Source), and empty for an ordinary one.
	Fetch string
}

// Receiver is the receiving half of the Upload API: an http.Handler that
// takes offers, asks Accept, and saves the files it accepted into SaveDir.
type Receiver struct {
	// Info is how this device describes itself.
	Info Info
	// SaveDir is where received files go.
	SaveDir string
	// Accept decides on an offer and returns the ids of the files to take;
	// none means decline. It may block while a person decides; ctx ends if
	// the sender gives up. If nil, every offer is declined.
	Accept func(ctx context.Context, o Offer) []string
	// Received, if set, is called after each file is saved.
	Received func(path string, f FileMeta)
	// Registered, if set, is called when another device introduces itself.
	Registered func(peer Info, remote string)
	// Progress, if set, is called once when an offer is accepted and then
	// as its files arrive. It is called often and must not block.
	Progress func(Progress)
	// AcceptTimeout, if set, replaces the default time a person has to
	// answer before an offer is declined.
	AcceptTimeout time.Duration
	// QuietWait, if set, replaces the default time a file that is arriving
	// may go without one more byte.
	QuietWait time.Duration
	// Quiet, if set, is called when a transfer is about to be stopped
	// because nothing arrived for that time; Finished follows. It must not
	// call back into the Receiver.
	Quiet func()
	// Finished, if set, is called when an accepted transfer ends: complete
	// if every file arrived. It must not call back into the Receiver.
	Finished func(complete bool)
	// Source, if set, lets a sender offer files for this device to fetch
	// over its own connection to the sender, where the sender would
	// otherwise upload them: it is asked for the way to the sender of such
	// an offer. An error means there is no way the files may take, and the
	// sender is told so. If nil, such an offer is refused as an empty one.
	Source func(ctx context.Context, o Offer) (*Source, error)
	// BorrowTime, if set, replaces the default time that finding the way
	// to such a sender and fetching its list of files may take.
	BorrowTime time.Duration
	// Trace, if set, is told each step of a fetch, for a log.
	Trace func(format string, args ...any)
	// Lender, if set, answers the requests of devices that fetch files this
	// device has offered them.
	Lender *Lender

	mu      sync.Mutex
	session *session
	offers  map[string][]time.Time // recent offers by sender address
	noPaths map[string][]time.Time // recent offers refused for want of a path
	reading map[string]bool        // addresses whose offer is being read
}

type session struct {
	id       string
	remote   string
	files    map[string]*incoming
	inFlight int
	lastUsed time.Time
	accepted bool
	progress Progress
	ended    atomic.Bool // stops uploads still arriving
	fetched  bool        // this device fetches the files; none may be uploaded
	stop     func()      // stops a fetch, if the files are fetched
}

type incoming struct {
	meta    FileMeta
	token   string
	started bool
}

// busy reports whether the session should still block a new offer.
func (s *session) busy() bool {
	return s.inFlight > 0 || time.Since(s.lastUsed) < sessionIdle
}

func (rc *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method + " " + r.URL.Path {
	case "GET " + apiPrefix + "/info":
		writeJSON(w, rc.Info.reply())
	case "POST " + apiPrefix + "/register":
		rc.register(w, r)
	case "POST " + apiPrefix + "/prepare-upload":
		rc.prepare(w, r)
	case "POST " + apiPrefix + "/upload":
		rc.upload(w, r)
	case "POST " + apiPrefix + "/cancel":
		rc.cancel(w, r)
	case "POST " + apiPrefix + "/prepare-download", "GET " + apiPrefix + "/download", "POST " + answerPath, "POST " + fetchedPath:
		if rc.Lender == nil {
			fail(w, http.StatusNotFound, "not found")
			return
		}
		rc.Lender.ServeHTTP(w, r)
	default:
		fail(w, http.StatusNotFound, "not found")
	}
}

func (rc *Receiver) register(w http.ResponseWriter, r *http.Request) {
	var peer Info
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxInfoBody)).Decode(&peer); err != nil {
		fail(w, http.StatusBadRequest, "invalid body")
		return
	}
	peer, ok := peer.cleaned()
	if !ok {
		fail(w, http.StatusBadRequest, "invalid body")
		return
	}
	if rc.Registered != nil {
		rc.Registered(peer, remoteHost(r))
	}
	writeJSON(w, rc.Info.reply())
}

func (rc *Receiver) prepare(w http.ResponseWriter, r *http.Request) {
	// A busy receiver refuses before reading anything, and only a few
	// offers are read at once, one for each address, so many at once cannot
	// fill memory. The slot is taken after the read: a sender that stalls
	// while sending its offer must not keep the others out.
	remote := remoteHost(r)
	// An offer with the mark lists no files: this device fetches the list,
	// and then the files, from the sender.
	mark := ""
	if rc.Source != nil {
		mark = r.Header.Get(FetchHeader)
	}
	rc.mu.Lock()
	busy := rc.session != nil && rc.session.busy()
	crowded := !busy && (rc.reading[remote] || len(rc.reading) >= maxOfferReads)
	if !busy && !crowded {
		if rc.reading == nil {
			rc.reading = map[string]bool{}
		}
		rc.reading[remote] = true
	}
	rc.mu.Unlock()
	if busy {
		fail(w, http.StatusConflict, "blocked by another session")
		return
	}
	if crowded {
		fail(w, http.StatusTooManyRequests, "too many offers")
		return
	}
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(offerReadTime))
	req, ok := rc.readOffer(w, r, mark != "")

	// It is held while the person decides, so a second offer gets 409.
	// Only offers that would raise a question count against the address.
	s := &session{id: randomID(), remote: remote, files: map[string]*incoming{}, inFlight: 1, fetched: mark != ""}
	rc.mu.Lock()
	delete(rc.reading, remote)
	busy = rc.session != nil && rc.session.busy()
	allowed := ok && !busy && (mark == "" || within(rc.noPaths, remote, noPathBurst)) && rc.allowOffer(remote)
	if allowed {
		if rc.session != nil {
			rc.end(rc.session, "a new offer took the place of one that never finished")
		}
		rc.session = s
	}
	rc.mu.Unlock()
	switch {
	case !ok:
		return
	case busy:
		fail(w, http.StatusConflict, "blocked by another session")
		return
	case !allowed:
		fail(w, http.StatusTooManyRequests, "too many offers")
		return
	}
	offer := Offer{Sender: req.Info, Remote: remote, Fetch: mark}
	rc.tracef("receive: offer from %q, to be fetched: %v", Clip(req.Info.Alias), mark != "")
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		offer.CertFingerprint = identity.Fingerprint(r.TLS.PeerCertificates[0].Raw)
	}
	ctx, cancel := context.WithTimeout(r.Context(), rc.acceptTimeout())
	defer cancel()
	var src *Source
	if mark != "" {
		if src, req.Files, ok = rc.borrow(ctx, w, offer); !ok {
			rc.mu.Lock()
			s.inFlight--
			rc.end(s, "the list of files was not fetched")
			rc.mu.Unlock()
			return
		}
	}
	for _, f := range req.Files {
		offer.Files = append(offer.Files, f)
	}

	var accepted []string
	asked := time.Now()
	if rc.Accept != nil {
		accepted = rc.Accept(ctx, offer)
	}
	rc.tracef("receive: answered after %v, files taken: %d of %d", time.Since(asked).Round(10*time.Millisecond), len(accepted), len(req.Files))

	rc.mu.Lock()
	s.inFlight--
	s.lastUsed = time.Now()
	resp := prepareResponse{SessionID: s.id, Files: map[string]string{}}
	for _, id := range accepted {
		if f, ok := req.Files[id]; ok {
			token := randomID()
			s.files[id] = &incoming{meta: f, token: token}
			resp.Files[id] = token
			s.progress.Total += f.Size
		}
	}
	if len(s.files) == 0 || ctx.Err() != nil {
		rc.end(s, "the offer was declined, or not answered in time")
		rc.mu.Unlock()
		if src != nil {
			// The no also goes on this device's own connection, which may
			// be much the quicker of the two.
			go func() {
				rc.tell(src, mark, prepareResponse{SessionID: s.id, Files: map[string]string{}})
				src.close()
			}()
		}
		fail(w, http.StatusForbidden, "rejected")
		return
	}
	s.accepted = true
	var fctx context.Context
	if src != nil {
		fctx, s.stop = context.WithCancel(context.Background())
	}
	progress := s.progress
	rc.mu.Unlock()
	if rc.Progress != nil {
		rc.Progress(progress)
	}
	if src != nil {
		w.Header().Set(FetchHeader, fetchYes)
	}
	writeJSON(w, resp)
	if src != nil {
		go rc.fetch(fctx, s, src, mark, resp)
	}
}

func (rc *Receiver) tracef(format string, args ...any) {
	if rc.Trace != nil {
		rc.Trace(format, args...)
	}
}

func (rc *Receiver) acceptTimeout() time.Duration {
	if rc.AcceptTimeout > 0 {
		return rc.AcceptTimeout
	}
	return acceptTimeout
}

// allowOffer counts an offer from an address and reports whether it is
// within the limit. rc.mu must be held.
func (rc *Receiver) allowOffer(remote string) bool {
	if rc.offers == nil {
		rc.offers = map[string][]time.Time{}
	}
	if !within(rc.offers, remote, offerBurst) {
		return false
	}
	rc.offers[remote] = append(rc.offers[remote], time.Now())
	return true
}

// within drops the times in counts that are older than offerWindow and
// reports whether remote has fewer than burst left, and whether there is room
// for it among the addresses tracked. It counts nothing itself.
func within(counts map[string][]time.Time, remote string, burst int) bool {
	now := time.Now()
	if len(counts) >= maxOfferSources {
		for k, times := range counts { // forget addresses that went quiet
			if now.Sub(times[len(times)-1]) >= offerWindow {
				delete(counts, k)
			}
		}
	}
	recent := counts[remote][:0]
	for _, t := range counts[remote] {
		if now.Sub(t) < offerWindow {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(counts, remote)
	} else {
		counts[remote] = recent
	}
	return len(recent) < burst && (len(recent) > 0 || len(counts) < maxOfferSources)
}

// forgetOffer takes back the count of the last offer from an address, one
// that was refused for want of a path before it raised a question, and
// counts it against noPathBurst instead.
func (rc *Receiver) forgetOffer(remote string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if times := rc.offers[remote]; len(times) > 1 {
		rc.offers[remote] = times[:len(times)-1]
	} else {
		delete(rc.offers, remote)
	}
	if rc.noPaths == nil {
		rc.noPaths = map[string][]time.Time{}
	}
	rc.noPaths[remote] = append(rc.noPaths[remote], time.Now())
}

// readOffer reads and checks an offer, answering the sender itself when it
// is not acceptable. A marked offer's files are not read from it: its list
// comes later (see borrow).
func (rc *Receiver) readOffer(w http.ResponseWriter, r *http.Request, marked bool) (prepareRequest, bool) {
	var req prepareRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxOfferBody)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "invalid body")
		return req, false
	}
	var ok bool
	if req.Info, ok = req.Info.cleaned(); !ok {
		fail(w, http.StatusBadRequest, "invalid body")
		return req, false
	}
	if marked {
		req.Files = nil
		return req, true
	}
	if problem := checkFiles(req.Files); problem != "" {
		fail(w, http.StatusBadRequest, problem)
		return req, false
	}
	return req, true
}

// checkFiles checks the list of files of an offer, however the list came,
// and returns what is wrong with it, or "". It drops previews that are too
// large.
func checkFiles(files map[string]FileMeta) string {
	if len(files) == 0 {
		return "invalid body"
	}
	if len(files) > maxOfferFiles {
		return "too many files"
	}
	for id, f := range files {
		if f.ID != id || f.Size < 0 {
			return "invalid body"
		}
		if _, ok := safeRel(f.FileName); !ok {
			return "invalid file name"
		}
		if len(f.Preview) > maxPreview {
			f.Preview = ""
			files[id] = f
		}
	}
	return ""
}

func (rc *Receiver) upload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("sessionId") == "" || q.Get("fileId") == "" || q.Get("token") == "" {
		fail(w, http.StatusBadRequest, "missing parameters")
		return
	}
	rc.mu.Lock()
	s := rc.session
	var f *incoming
	// The files of a fetched session arrive only on this device's own
	// connection, which is the one held to a direct path.
	if s != nil && !s.fetched && s.id == q.Get("sessionId") && s.remote == remoteHost(r) {
		f = s.files[q.Get("fileId")]
	}
	if f == nil || f.token != q.Get("token") || f.started {
		rc.mu.Unlock()
		fail(w, http.StatusForbidden, "invalid token or IP address")
		return
	}
	f.started = true
	s.inFlight++
	rc.mu.Unlock()

	// A read that waits on a sender gone quiet ends when its time is up.
	wake := func() { http.NewResponseController(w).SetReadDeadline(time.Now()) }
	if status, err := rc.take(s, f, r.Body, wake); err != nil {
		fail(w, status, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// take saves one accepted file of s as it arrives through r. It is the one
// way a file is taken in, whether the sender uploads it or this device
// fetches it. The caller has marked f as started and counted it in flight.
// On failure it returns the HTTP status that answers an upload. If nothing
// arrives for the quiet time the whole transfer ends, so that a sender that
// stopped with no word does not hold this device for good; wake, if set, is
// then called to end a read of r that still waits (ending the session
// already stops a fetch).
func (rc *Receiver) take(s *session, f *incoming, r io.Reader, wake func()) (int, error) {
	wait := quietWait
	if rc.QuietWait > 0 {
		wait = rc.QuietWait
	}
	quiet := time.AfterFunc(wait, func() {
		rc.mu.Lock()
		current := rc.session == s
		if current {
			if rc.Quiet != nil {
				rc.Quiet()
			}
			rc.end(s, fmt.Sprintf("nothing arrived for %v", wait))
		}
		rc.mu.Unlock()
		if current && wake != nil {
			wake()
		}
	})
	defer quiet.Stop()
	body := &meter{r: r, stop: s.ended.Load}
	body.count = func(n int) {
		if n > 0 {
			quiet.Reset(wait)
		}
		rc.mu.Lock()
		s.progress.File = f.meta.FileName
		s.progress.Done += int64(n)
		progress := s.progress
		rc.mu.Unlock()
		if rc.Progress != nil {
			rc.Progress(progress)
		}
	}
	start := time.Now()
	saved, status, err := rc.save(f.meta, body)
	rc.tracef("receive: a file ended, %d of %d bytes after %v (%s), error: %v", body.n, f.meta.Size, time.Since(start).Round(10*time.Millisecond), rate(body.n, time.Since(start)), err)

	rc.mu.Lock()
	s.inFlight--
	s.lastUsed = time.Now()
	current := rc.session == s
	if err != nil {
		f.started = false // let the sender try this file again
		s.progress.Done -= body.n
	} else {
		delete(s.files, f.meta.ID)
	}
	last := len(s.files) == 0
	rc.mu.Unlock()

	if err != nil {
		return status, err
	}
	if !current {
		// Cancelled while this file was arriving.
		os.Remove(saved)
		return http.StatusForbidden, errors.New("session cancelled")
	}
	if rc.Received != nil {
		rc.Received(saved, f.meta)
	}
	if last {
		// After Received, so the last file is known before Finished.
		rc.mu.Lock()
		rc.end(s, "every file arrived")
		rc.mu.Unlock()
	}
	return 0, nil
}

func (rc *Receiver) cancel(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if s := rc.session; s != nil && s.id == r.URL.Query().Get("sessionId") && s.remote == remoteHost(r) {
		rc.end(s, "the sender cancelled")
	}
	w.WriteHeader(http.StatusOK)
}

// Cancel abandons the transfer in progress, if there is one.
func (rc *Receiver) Cancel() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.session != nil {
		rc.end(rc.session, "this device cancelled")
	}
}

// end clears s if it is still the current session. rc.mu must be held. why
// is for the log.
func (rc *Receiver) end(s *session, why string) {
	if rc.session != s {
		return
	}
	rc.session = nil
	rc.tracef("receive: ended: %s; files not arrived: %d", why, len(s.files))
	s.ended.Store(true)
	if s.stop != nil {
		s.stop()
	}
	if s.accepted && rc.Finished != nil {
		rc.Finished(len(s.files) == 0)
	}
}

// save streams body into SaveDir under the file's name, checking its size
// and, when the sender gave one, its SHA-256. On failure it returns the HTTP
// status to answer with.
func (rc *Receiver) save(meta FileMeta, body io.Reader) (string, int, error) {
	rel, _ := safeRel(meta.FileName) // checked when the offer came in
	dest := filepath.Join(rc.SaveDir, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", http.StatusInternalServerError, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".yonder-*.part")
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed into place

	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(body, meta.Size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	if n != meta.Size {
		return "", http.StatusBadRequest, fmt.Errorf("size does not match the offer")
	}
	if meta.SHA256 != "" && !strings.EqualFold(meta.SHA256, hex.EncodeToString(hash.Sum(nil))) {
		return "", http.StatusUnprocessableEntity, fmt.Errorf("checksum mismatch")
	}
	final, err := moveWithoutOverwrite(tmp.Name(), dest)
	if err != nil {
		return "", http.StatusInternalServerError, err
	}
	return final, 0, nil
}

// moveWithoutOverwrite renames tmp to dest, or to "name (1).ext" and so on
// when dest is taken, and returns the name used.
func moveWithoutOverwrite(tmp, dest string) (string, error) {
	ext := filepath.Ext(dest)
	stem := strings.TrimSuffix(dest, ext)
	for i := 0; i < 10000; i++ {
		candidate := dest
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		// Creating the name exclusively reserves it; the rename then
		// replaces our own empty placeholder.
		placeholder, err := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		placeholder.Close()
		if err := os.Rename(tmp, candidate); err != nil {
			os.Remove(candidate)
			return "", err
		}
		return candidate, os.Chmod(candidate, 0o644)
	}
	return "", fmt.Errorf("no free name for %s", dest)
}

// safeRel turns a sender's file name, which may include folders, into a
// relative path that cannot leave the save folder. A hidden part loses its
// dot for a "_" (a file in ".config/autostart" runs at login). Names are
// refused whose parts would land on disk as something else: Windows'
// reserved device names, and ones with control or direction-changing
// characters (names.Bad), which make a name look like another; and names
// too long for a disk.
func safeRel(name string) (string, bool) {
	clean := path.Clean("/" + strings.ReplaceAll(name, `\`, "/"))[1:]
	if clean == "" || len(clean) > maxFileName || strings.ContainsFunc(clean, names.Bad) {
		return "", false
	}
	parts := strings.Split(clean, "/")
	for i, part := range parts {
		// Windows drops dots and spaces at the end, and reads a colon as
		// the start of a second stream of the file.
		if len(part) > maxNamePart || reservedName(part) || strings.Contains(part, ":") || part != strings.TrimRight(part, ". ") {
			return "", false
		}
		// A hidden name is kept, but not hidden: people do send ".env".
		if strings.HasPrefix(part, ".") {
			parts[i] = "_" + part[1:]
		}
	}
	return filepath.Join(parts...), true
}

// reservedName reports a Windows device name, which names the device and
// not a file there, whatever follows its first dot.
func reservedName(part string) bool {
	stem, _, _ := strings.Cut(part, ".")
	switch stem = strings.ToUpper(strings.TrimRight(stem, " ")); stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	return len(stem) == 4 && (stem[:3] == "COM" || stem[:3] == "LPT") && stem[3] >= '1' && stem[3] <= '9'
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// rate is a speed as the log shows it.
func rate(n int64, d time.Duration) string {
	if d <= 0 {
		return "0 KB/s"
	}
	return fmt.Sprintf("%.0f KB/s", float64(n)/1000/d.Seconds())
}
