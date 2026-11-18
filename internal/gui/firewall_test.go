package gui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"yonder/internal/app"
	"yonder/internal/firewall"
	"yonder/internal/localsend"
	"yonder/internal/nettest"
	"yonder/internal/pairing"
)

// A firewall that is read as keeping devices out raises the notice; its
// button opens the firewall and says so; backing out of the system's
// prompt changes nothing.
func TestFirewallNotice(t *testing.T) {
	var open, cancel atomic.Bool
	cancel.Store(true)
	s, w, _ := deviceWith(t, "Nimble Harbour", Options{
		Firewall: func(int) firewall.Status { return firewall.Status{On: true, Blocking: !open.Load()} },
		FixFirewall: func(int) error {
			if cancel.Load() {
				return firewall.ErrCancelled
			}
			open.Store(true)
			return nil
		},
	})
	st := w.until(t, "the notice", func(st State) bool { return st.Blocked != nil })
	if st.Blocked.Title != "This computer's firewall is keeping other devices out" || st.Blocked.Button != "Let them in…" || st.Problem != "" {
		t.Fatalf("notice: %+v, problem %q", st.Blocked, st.Problem)
	}
	if err := s.FixFirewall(); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.Blocked == nil {
		t.Fatal("the notice went though the prompt was cancelled")
	}
	cancel.Store(false)
	if err := s.FixFirewall(); err != nil {
		t.Fatal(err)
	}
	w.untilSaid(t, "The firewall now lets devices on your network reach Yonder.")
	if st := s.State(); st.Blocked != nil || st.Problem != "" {
		t.Fatalf("after the fix: %+v, problem %q", st.Blocked, st.Problem)
	}
}

// "Not now" puts the notice away and leaves a line under the device's name.
func TestFirewallNotNow(t *testing.T) {
	s, w, _ := deviceWith(t, "Nimble Harbour", Options{
		Firewall:    func(int) firewall.Status { return firewall.Status{On: true, Blocking: true} },
		FixFirewall: func(int) error { return errors.New("not asked for") },
	})
	w.until(t, "the notice", func(st State) bool { return st.Blocked != nil })
	s.HideFirewall()
	if st := s.State(); st.Blocked != nil || st.Problem != "Firewall is keeping other devices out" {
		t.Fatalf("after Not now: %+v, problem %q", st.Blocked, st.Problem)
	}
}

// Sending to a device on the network that takes no connection says what
// the likely reason is, not what the system called the failure.
func TestSendNotReached(t *testing.T) {
	a, aw, _ := device(t, "Swift Comet")
	file := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{
		app.Model:   "Could not reach Nimble Harbour. A firewall on it may be keeping this device out; Yonder there will say so.",
		"LocalSend": "Could not reach Nimble Harbour. A firewall on it may be keeping this device out.",
	} {
		id := "fingerprint of a " + model
		a.node.Nearby.Seen(localsend.Info{Alias: "Nimble Harbour", Fingerprint: id, DeviceModel: model, Port: 1, Protocol: "https"}, "127.0.0.1")
		if err := a.Send(id, []string{file}); err != nil {
			t.Fatal(err)
		}
		st := aw.until(t, "the send to fail", func(st State) bool { return len(st.Transfers) == 1 && st.Transfers[0].Note != "" })
		if st.Transfers[0].Note != want {
			t.Errorf("%s: note %q", model, st.Transfers[0].Note)
		}
		a.DismissTransfer(st.Transfers[0].ID)
		a.node.Nearby.Forget(id)
	}
}

// A paired device that is also seen on the local network, but takes no
// connection there, gets the files through the tunnel instead. Through the
// relay, so only with YONDER_NET_TESTS.
func TestSendFallsBackToTunnel(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	a, aw, _ := device(t, "Sender")
	b, bw, saveDir := device(t, "Receiver")
	for _, s := range []*Service{a, b} {
		if err := s.node.OpenTunnel(ctx); err != nil {
			t.Fatalf("tunnel: %v", err)
		}
	}
	p, err := b.node.Pair(ctx, func(context.Context, pairing.Device) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.node.Join(ctx, p.Link); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := p.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	// The sender sees the receiver on the local network, at an address
	// where nothing answers.
	info := b.node.Info
	info.Port = 1
	a.node.Nearby.Seen(info, "127.0.0.1")

	path := filepath.Join(t.TempDir(), "far.txt")
	if err := os.WriteFile(path, []byte("over the tunnel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(info.Fingerprint, []string{path}); err != nil {
		t.Fatal(err)
	}
	st := bw.until(t, "the receiver to be asked", func(st State) bool { return st.Offer != nil })
	if !st.Offer.Paired {
		t.Fatalf("the offer did not come through the tunnel: %+v", st.Offer)
	}
	if err := b.AnswerOffer(st.Offer.ID, true, false); err != nil {
		t.Fatal(err)
	}
	st = aw.until(t, "the send to end", func(st State) bool { return len(st.Transfers) == 1 && st.Transfers[0].Note != "" })
	if st.Transfers[0].Note != "Sent 1 file to Receiver." {
		t.Fatalf("note: %q", st.Transfers[0].Note)
	}
	if got, err := os.ReadFile(filepath.Join(saveDir, "far.txt")); err != nil || string(got) != "over the tunnel" {
		t.Fatalf("received %q, %v", got, err)
	}
}
