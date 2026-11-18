package gui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yonder/internal/app"
	"yonder/internal/localsend"
	"yonder/internal/nearby"
	"yonder/internal/pairing"
	"yonder/internal/settings"
)

// window is a Shell with nobody at it: it remembers the latest State.
type window struct {
	mu     sync.Mutex
	state  State
	said   []string
	told   []string
	opened []string
	pick   []string
	auto   bool
}

func (w *window) Changed(s State) {
	w.mu.Lock()
	w.state = s
	w.mu.Unlock()
}
func (w *window) Say(m string) {
	w.mu.Lock()
	w.said = append(w.said, m)
	w.mu.Unlock()
}
func (w *window) Attention() {}
func (w *window) Notify(title, body string) {
	w.mu.Lock()
	w.told = append(w.told, title+": "+body)
	w.mu.Unlock()
}
func (w *window) Autostart() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.auto
}
func (w *window) SetAutostart(on bool) error {
	w.mu.Lock()
	w.auto = on
	w.mu.Unlock()
	return nil
}
func (w *window) PickFiles() ([]string, error)      { return w.pick, nil }
func (w *window) PickFolder(string) (string, error) { return "", nil }
func (w *window) Open(p string) error {
	w.mu.Lock()
	w.opened = append(w.opened, p)
	w.mu.Unlock()
	return nil
}
func (w *window) Browse(string) error { return nil }

// until polls the latest State until ok says yes, and returns it.
func (w *window) until(t *testing.T, what string, ok func(State) bool) State {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		w.mu.Lock()
		s := w.state
		w.mu.Unlock()
		if ok(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; state: %+v", what, s)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// untilSaid waits for the window to have been told something.
func (w *window) untilSaid(t *testing.T, message string) {
	t.Helper()
	w.until(t, "the message "+message, func(State) bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return slices.Contains(w.said, message)
	})
}

// device starts a Service on a free local port with its own folders.
func device(t *testing.T, name string) (*Service, *window, string) {
	t.Helper()
	return deviceAt(t, name, "")
}

// deviceAt is device with a meeting point for finding nearby devices.
func deviceAt(t *testing.T, name, meeting string) (*Service, *window, string) {
	t.Helper()
	return deviceWith(t, name, Options{Meeting: meeting})
}

// deviceWith is device with more of its Options chosen.
func deviceWith(t *testing.T, name string, o Options) (*Service, *window, string) {
	t.Helper()
	dir, saveDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"name":"`+name+`","saveDir":"`+saveDir+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := &window{}
	o.Dir, o.Listen, o.Offline, o.Kind = dir, "127.0.0.1:0", true, nearby.KindApp
	s, err := Start(o, w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, w, saveDir
}

// introduce makes a see b on the local network, as discovery would.
func introduce(a, b *Service) string {
	a.node.Nearby.Seen(b.node.Info, "127.0.0.1")
	return b.node.Info.Fingerprint
}

func TestSendAskAcceptReceive(t *testing.T) {
	a, aw, _ := device(t, "Sender")
	b, bw, saveDir := device(t, "Receiver")
	id := introduce(a, b)
	st := aw.until(t, "the receiver in the sender's list", func(s State) bool { return len(s.Devices) == 1 })
	if d := st.Devices[0]; d.Name != "Receiver" || !d.Nearby || d.Paired || d.LocalSend || d.Presence != "online" {
		t.Fatalf("device row: %+v", d)
	}
	// A device on the local network that is not Yonder is LocalSend.
	a.node.Nearby.Seen(localsend.Info{Alias: "Phone", DeviceModel: "iPhone", Fingerprint: "phone"}, "127.0.0.1")
	a.changed()
	st = aw.until(t, "the phone in the sender's list", func(s State) bool { return row(s, "Phone") != nil })
	if d := row(st, "Phone"); !d.LocalSend || !d.Nearby || d.Paired || d.Pairable {
		t.Fatalf("LocalSend row: %+v", d)
	}

	data := bytes.Repeat([]byte("yonder"), 200_000)
	path := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(path, data, 0o600)
	aw.pick = []string{path}
	if err := a.PickAndSend(id); err != nil {
		t.Fatal(err)
	}
	st = aw.until(t, "the sender's transfer to wait", func(s State) bool { return len(s.Transfers) == 1 })
	if tr := st.Transfers[0]; !tr.Sending || tr.Device != "Receiver" || tr.Phase != "waiting" {
		t.Fatalf("sender's transfer: %+v", tr)
	}

	st = bw.until(t, "the offer", func(s State) bool { return s.Offer != nil })
	if o := st.Offer; o.From != "Sender" || o.Paired || len(o.Files) != 1 || o.Files[0] != (OfferFile{Name: "notes.txt", Size: int64(len(data))}) {
		t.Fatalf("offer: %+v", o)
	}
	if err := b.AnswerOffer(st.Offer.ID, true, false); err != nil {
		t.Fatal(err)
	}

	st = bw.until(t, "the file to arrive", func(s State) bool {
		return len(s.Received) == 1 && len(s.Transfers) == 1 && s.Transfers[0].Phase == "done"
	})
	bw.mu.Lock()
	told := slices.Clone(bw.told)
	bw.mu.Unlock()
	if !slices.Equal(told, []string{"Sender (unverified device) wants to send 1 file: notes.txt"}) {
		t.Fatalf("notifications for the offer: %q", told)
	}
	if r := st.Received[0]; r.Name != "notes.txt" || r.From != "Sender" || st.Unread != 1 || st.Offer != nil {
		t.Fatalf("after receiving: %+v", st)
	}
	if tr := st.Transfers[0]; tr.Done != tr.Total || tr.Total != int64(len(data)) || tr.Note != "Received 1 file from Sender." {
		t.Fatalf("receiver's transfer: %+v", tr)
	}
	if got, _ := os.ReadFile(filepath.Join(saveDir, "notes.txt")); !bytes.Equal(got, data) {
		t.Fatal("the saved file differs from the one sent")
	}
	st = aw.until(t, "the sender to finish", func(s State) bool { return s.Transfers[0].Phase == "done" })
	if tr := st.Transfers[0]; tr.Note != "Sent 1 file to Receiver." || tr.Done != int64(len(data)) {
		t.Fatalf("sender's transfer: %+v", tr)
	}

	b.SeenReceived()
	bw.until(t, "the unread count to clear", func(s State) bool { return s.Unread == 0 })
	b.DismissTransfer(st.Transfers[0].ID)
	a.DismissTransfer(st.Transfers[0].ID)
	aw.until(t, "the finished transfer to go", func(s State) bool { return len(s.Transfers) == 0 })
}

func TestDeclineAndCancel(t *testing.T) {
	a, aw, _ := device(t, "Sender")
	b, bw, _ := device(t, "Receiver")
	id := introduce(a, b)
	path := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(path, []byte("a"), 0o600)

	a.Send(id, []string{path})
	st := bw.until(t, "the offer", func(s State) bool { return s.Offer != nil })
	b.AnswerOffer(st.Offer.ID, false, false)
	st = aw.until(t, "the decline to show", func(s State) bool { return len(s.Transfers) == 1 && s.Transfers[0].Phase == "failed" })
	if note := st.Transfers[0].Note; note != "Receiver declined." {
		t.Fatalf("note: %q", note)
	}
	a.DismissTransfer(st.Transfers[0].ID)
	bw.until(t, "the offer to close", func(s State) bool { return s.Offer == nil && len(s.Transfers) == 0 })

	// The sender backs out while the receiver is still deciding.
	a.Send(id, []string{path})
	bw.until(t, "the second offer", func(s State) bool { return s.Offer != nil })
	st = aw.until(t, "the second transfer", func(s State) bool { return len(s.Transfers) == 1 })
	a.CancelTransfer(st.Transfers[0].ID)
	aw.until(t, "the cancelled transfer to go", func(s State) bool { return len(s.Transfers) == 0 })
	bw.until(t, "the withdrawn offer to close", func(s State) bool { return s.Offer == nil })
}

func TestRenameRestartsUnderTheNewName(t *testing.T) {
	a, aw, _ := device(t, "Before")
	if err := a.SetName("   "); err == nil {
		t.Fatal("an empty name was accepted")
	}
	if err := a.SetName(strings.Repeat("x", maxName+1)); err == nil {
		t.Fatal("an overlong name was accepted")
	}
	if err := a.SetName(" After  that "); err != nil {
		t.Fatal(err)
	}
	aw.until(t, "the new name", func(s State) bool { return s.Name == "After that" })
	if got := a.node.Info.Alias; got != "After that" {
		t.Fatalf("the device announces itself as %q", got)
	}
}

// The owner's own name for a paired device is tidied, kept beside the
// device's own, and goes when it is emptied or the device is unpaired.
func TestLabel(t *testing.T) {
	a, w, _ := device(t, "Ada")
	if err := a.SetLabel("FP-bo", "Mum's laptop"); err == nil {
		t.Error("a device that is not paired was given a name")
	}
	if err := pairing.Open(a.dir).Add(pairing.Device{Name: "Bo", Fingerprint: "FP-bo", Addr: "tc-bo", ClientID: "nodekey:bo"}); err != nil {
		t.Fatal(err)
	}
	if err := a.SetLabel("FP-bo", "  Mum's   laptop "); err != nil {
		t.Fatal(err)
	}
	st := w.until(t, "Bo with Ada's name for it", func(s State) bool { return row(s, "Bo") != nil && row(s, "Bo").Label != "" })
	if d := row(st, "Bo"); d.Label != "Mum's laptop" || d.Name != "Bo" {
		t.Fatalf("row: %+v", d)
	}
	if err := a.SetLabel("FP-bo", strings.Repeat("x", maxName+1)); err == nil {
		t.Error("a name that is too long was taken")
	}
	if set, _ := settings.Load(a.dir); set.Labels["FP-bo"] != "Mum's laptop" {
		t.Errorf("saved labels: %v", set.Labels)
	}

	if err := a.SetLabel("FP-bo", " "); err != nil {
		t.Fatal(err)
	}
	w.until(t, "Bo without it", func(s State) bool { return row(s, "Bo").Label == "" })

	a.SetLabel("FP-bo", "Office")
	a.Unpair("FP-bo") // there is no tunnel here to tell Bo; the device is forgotten all the same
	if set, _ := settings.Load(a.dir); len(set.Labels) != 0 {
		t.Errorf("labels after unpairing: %v", set.Labels)
	}
}

func TestRefreshDropsSilentNearbyDevices(t *testing.T) {
	a, w, _ := device(t, "Ada")
	b, _, _ := device(t, "Bo")
	introduce(a, b)
	// Nothing listens on port 1, so this one never answers.
	a.node.Nearby.Seen(localsend.Info{Alias: "Ghost", Fingerprint: "ghost", DeviceModel: "Yonder", Port: 1, Protocol: "https"}, "127.0.0.1")
	w.until(t, "Bo and Ghost listed", func(s State) bool { return row(s, "Bo") != nil && row(s, "Ghost") != nil })

	a.Refresh()
	st := w.until(t, "the refresh to end without Ghost", func(s State) bool { return !s.Refreshing && row(s, "Ghost") == nil })
	if row(st, "Bo") == nil {
		t.Errorf("Bo answered and left the list: %+v", st.Devices)
	}
}

func TestAutostartFollowsTheSystem(t *testing.T) {
	a, aw, _ := device(t, "Device")
	if a.State().Autostart {
		t.Fatal("start at login is on before it was asked for")
	}
	if err := a.SetAutostart(true); err != nil {
		t.Fatal(err)
	}
	aw.until(t, "start at login to show as on", func(s State) bool { return s.Autostart })
	if err := a.SetAutostart(false); err != nil {
		t.Fatal(err)
	}
	aw.until(t, "start at login to show as off", func(s State) bool { return !s.Autostart })
}

func TestOfferSummary(t *testing.T) {
	for want, files := range map[string][]OfferFile{
		"":                 nil,
		"a.jpg":            {{Name: "a.jpg"}},
		"a.jpg and 2 more": {{Name: "a.jpg"}, {Name: "b.jpg"}, {Name: "c.jpg"}},
	} {
		if got := offerSummary(files); got != want {
			t.Errorf("offerSummary of %d: got %q, want %q", len(files), got, want)
		}
	}
}

func TestQRRows(t *testing.T) {
	rows := qrRows("https://app.getyonder.org/#pair=" + strings.Repeat("A", 320))
	if len(rows) < 21 || len(rows[0]) != len(rows) || !strings.HasPrefix(rows[0], "1111111") {
		t.Fatalf("not a QR code: %d rows, first %q", len(rows), rows[0])
	}
}

// A device told it is closed (the web version, once its site has reached the
// month's limit) neither sends nor takes an offer, and says why both times;
// when it is open again it does both.
func TestClosedStopsSendingAndReceiving(t *testing.T) {
	const why = "Closed for the month."
	var closed atomic.Bool
	closed.Store(true)
	a, aw, _ := deviceWith(t, "Tab", Options{Closed: func(context.Context) string {
		if closed.Load() {
			return why
		}
		return ""
	}})
	b, bw, _ := device(t, "Desktop")
	toB, toA := introduce(a, b), introduce(b, a)
	path := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(path, []byte("yonder"), 0o600)

	if err := a.Send(toB, []string{path}); err == nil || err.Error() != why {
		t.Fatalf("a send while closed returned %v, want %q", err, why)
	}
	if st := a.State(); len(st.Transfers) != 0 {
		t.Fatalf("a send while closed started a transfer: %+v", st.Transfers)
	}

	if err := b.Send(toA, []string{path}); err != nil {
		t.Fatal(err)
	}
	aw.untilSaid(t, why)
	st := bw.until(t, "the desktop's send to be declined", func(s State) bool {
		return len(s.Transfers) == 1 && s.Transfers[0].Phase == "failed"
	})
	if note := st.Transfers[0].Note; note != "Tab declined." {
		t.Fatalf("the desktop's send ended with %q", note)
	}
	if st := a.State(); st.Offer != nil || len(st.Transfers) != 0 {
		t.Fatalf("an offer while closed reached the person: %+v", st)
	}

	closed.Store(false)
	if err := a.Send(toB, []string{path}); err != nil {
		t.Fatalf("a send once open again: %v", err)
	}
	bw.until(t, "the offer once open again", func(s State) bool { return s.Offer != nil })
}

func TestSendWait(t *testing.T) {
	a, _, _ := device(t, "Sender")
	b, bw, saveDir := device(t, "Receiver")
	id := introduce(a, b)
	path := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(path, []byte("yonder"), 0o600)

	type end struct {
		note string
		ok   bool
		err  error
	}
	for _, accept := range []bool{true, false} {
		ended := make(chan end, 1)
		go func() {
			note, ok, err := a.SendWait(id, []string{path})
			ended <- end{note, ok, err}
		}()
		st := bw.until(t, "the offer", func(s State) bool { return s.Offer != nil })
		select {
		case e := <-ended:
			t.Fatalf("SendWait returned before the offer was answered: %+v", e)
		default:
		}
		if err := b.AnswerOffer(st.Offer.ID, accept, false); err != nil {
			t.Fatal(err)
		}
		want := end{"Sent 1 file to Receiver.", true, nil}
		if !accept {
			want = end{"Receiver declined.", false, nil}
		}
		if e := <-ended; e != want {
			t.Fatalf("accept=%v: SendWait gave %+v, want %+v", accept, e, want)
		}
		bw.until(t, "the receiver to be free again", func(s State) bool {
			return s.Offer == nil && (len(s.Transfers) == 0 || s.Transfers[len(s.Transfers)-1].Phase != "moving")
		})
	}
	if got, _ := os.ReadFile(filepath.Join(saveDir, "notes.txt")); string(got) != "yonder" {
		t.Fatalf("the saved file: %q", got)
	}
	if _, _, err := a.SendWait("nobody", []string{path}); err == nil {
		t.Fatal("SendWait to a device that is not listed gave no error")
	}
	name, devices := a.List()
	if name != "Sender" || len(devices) != 1 || devices[0].ID != id {
		t.Fatalf("List: %q %+v", name, devices)
	}
}

func TestNoticeTextRemovesMarkupAndControlCharacters(t *testing.T) {
	if got := noticeText("<b>Eve</b>\n\x1b[2J wants", 100); got != "bEve/b[2J wants" {
		t.Fatalf("got %q", got)
	}
	long := noticeText(strings.Repeat("é", 500), noticeBodyMax)
	if n := len([]rune(long)); n != noticeBodyMax {
		t.Fatalf("a long text has %d characters, want %d", n, noticeBodyMax)
	}
}

// row is the device of that name in a State, or nil.
func row(s State, name string) *Device {
	for _, d := range s.Devices {
		if d.Name == name {
			return &d
		}
	}
	return nil
}

// The way a transfer with a browser travels is shown while it moves, and
// said in a word when it has gone well; other transfers say nothing new.
func TestWayOfATransferIsShown(t *testing.T) {
	s, w, _ := device(t, "A")
	for way, want := range map[app.Way]struct {
		word       string
		direct     bool
		connecting bool
	}{app.WayOther: {"", false, false}, app.WayConnecting: {"", false, true}, app.WayChannel: {" Direct.", true, false}, app.WayRelay: {" Relayed.", false, false}} {
		s.mu.Lock()
		tr := s.begin(Transfer{Sending: true, Device: "B", Files: 1, Phase: "moving"}, func() {})
		s.mu.Unlock()
		s.setWay(tr, way)
		w.until(t, "the transfer to be shown with its way", func(st State) bool {
			for _, shown := range st.Transfers {
				if shown.ID == tr.ID {
					return shown.Direct == want.direct && shown.Connecting == want.connecting
				}
			}
			return false
		})
		if got := s.wayWord(tr); got != want.word {
			t.Errorf("way %v: the note ends with %q, want %q", way, got, want.word)
		}
	}
}

// Anyone on the network can announce a paired device's fingerprint, under
// any name. The paired row keeps the name from the pairing; it only shows
// that the device is near. A device that is not paired is shown by the name
// it announces.
func TestAnnouncementDoesNotRenameAPairedDevice(t *testing.T) {
	paired := []pairing.Device{{Name: "Anna's laptop", Fingerprint: "FP-anna"}}
	near := []localsend.Peer{
		{Info: localsend.Info{Alias: "Your bank", DeviceModel: "LocalSend", Fingerprint: "FP-anna"}},
		{Info: localsend.Info{Alias: "Stranger", DeviceModel: "LocalSend", Fingerprint: "FP-other"}},
	}
	got := rows(paired, near, nil, settings.Settings{})
	if d := got["FP-anna"]; d == nil || d.Name != "Anna's laptop" || d.Model != app.Model || d.LocalSend || !d.Paired || !d.Nearby {
		t.Errorf("the paired device's row: %+v", d)
	}
	if d := got["FP-other"]; d == nil || d.Name != "Stranger" || !d.LocalSend || d.Paired || !d.Nearby {
		t.Errorf("the other device's row: %+v", d)
	}
}
