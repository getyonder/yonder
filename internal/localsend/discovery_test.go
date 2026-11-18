package localsend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"yonder/internal/names"
)

// announcement is what the harness receiver would multicast.
func announcement(t *testing.T, h *harness, announce bool) []byte {
	t.Helper()
	info := h.peer().Info
	info.Announce = &announce
	msg, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// These tests feed messages straight to the handler; real multicast is
// checked by hand against the stock app, since it depends on the machine's
// interfaces.
func TestAnnouncementIsNotedAndAnsweredOnce(t *testing.T) {
	h := newHarness(t)
	registered := 0
	h.rc.Registered = func(Info, string) { registered++ }

	s := newSender(t)
	reg := &Registry{Self: s.Info.Fingerprint}
	added := 0
	reg.Added = func(Peer) { added++ }
	d := &Discovery{Info: s.Info, Sender: s, Seen: reg.Seen}

	// The same announcement heard three times, as on a machine with several interfaces.
	for range 3 {
		d.handle(context.Background(), announcement(t, h, true), "127.0.0.1")
	}
	if registered != 1 {
		t.Fatalf("answered %d times, want 1", registered)
	}
	if added != 1 || len(reg.List()) != 1 {
		t.Fatalf("device listed %d times (added %d), want once", len(reg.List()), added)
	}
	if p, ok := reg.Find("TEST RECEIVER"); !ok || p.Info.Port != h.port {
		t.Fatalf("find by name: %+v, %v", p, ok)
	}
}

func TestReplyIsNotedButNotAnswered(t *testing.T) {
	h := newHarness(t)
	registered := 0
	h.rc.Registered = func(Info, string) { registered++ }
	s := newSender(t)
	reg := &Registry{Self: s.Info.Fingerprint}
	d := &Discovery{Info: s.Info, Sender: s, Seen: reg.Seen}

	d.handle(context.Background(), announcement(t, h, false), "127.0.0.1")
	if registered != 0 || len(reg.List()) != 1 {
		t.Fatalf("answered %d times, listed %d; want 0 and 1", registered, len(reg.List()))
	}
}

func TestOwnAnnouncementAndJunkAreIgnored(t *testing.T) {
	s := newSender(t)
	reg := &Registry{Self: s.Info.Fingerprint}
	d := &Discovery{Info: s.Info, Sender: s, Seen: reg.Seen}

	own, _ := json.Marshal(s.Info)
	d.handle(context.Background(), own, "127.0.0.1")
	d.handle(context.Background(), []byte("not json"), "127.0.0.1")
	d.handle(context.Background(), []byte(`{"alias":"no fingerprint"}`), "127.0.0.1")
	if n := len(reg.List()); n != 0 {
		t.Fatalf("listed %d devices, want none", n)
	}
}

func TestOnlyLocalAddressesAreCalledBack(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "192.168.1.5": true, "10.1.2.3": true, "172.16.0.9": true,
		"169.254.1.1": true, "100.64.0.1": true, "100.127.255.1": true,
		"fe80::1%eth0": true, "fd00::1": true, "::1": true,
		"8.8.8.8": false, "100.128.0.1": false, "172.32.0.1": false,
		"2001:db8::1": false, "example.com": false, "": false,
	} {
		if got := isLocalHost(host); got != want {
			t.Errorf("isLocalHost(%q) = %v, want %v", host, got, want)
		}
	}

	h := newHarness(t)
	registered := 0
	h.rc.Registered = func(Info, string) { registered++ }
	s := newSender(t)
	reg := &Registry{Self: s.Info.Fingerprint}
	d := &Discovery{Info: s.Info, Sender: s, Seen: reg.Seen}
	d.handle(context.Background(), announcement(t, h, true), "8.8.8.8")
	if registered != 0 {
		t.Fatalf("called back a public address %d times", registered)
	}

	// A privileged port is not called either, though the device is noted.
	info := h.peer().Info
	info.Port = 22
	announce := true
	info.Announce = &announce
	msg, _ := json.Marshal(info)
	d.handle(context.Background(), msg, "127.0.0.1")
	if registered != 0 || len(reg.List()) != 1 {
		t.Fatalf("called %d times, listed %d; want 0 and 1", registered, len(reg.List()))
	}
}

func TestAnsweredListIsBounded(t *testing.T) {
	d := &Discovery{}
	for i := range maxAnswered {
		if !d.shouldAnswer(fmt.Sprint("fp", i)) {
			t.Fatalf("answer %d refused early", i)
		}
	}
	if d.shouldAnswer("one more") {
		t.Fatal("answered beyond the limit")
	}
	if len(d.answered) > maxAnswered {
		t.Fatalf("%d answer times kept", len(d.answered))
	}
	// Once the old times are past answerEvery they make room.
	for k := range d.answered {
		d.answered[k] = time.Now().Add(-2 * answerEvery)
	}
	if !d.shouldAnswer("one more") {
		t.Fatal("old answer times did not make room")
	}
}

// What a device says of itself is kept short, and one address cannot fill
// the list: a megabyte of name is cut, a fingerprint that long is not kept
// at all, and the devices one address adds stop at maxPeersFrom.
func TestSeenKeepsLittleFromOneAddress(t *testing.T) {
	r := &Registry{Self: "me"}
	huge := strings.Repeat("n", 1<<20)
	r.Seen(Info{Fingerprint: "fp", Alias: huge, DeviceModel: huge, DeviceType: huge, Version: huge, Protocol: "https"}, "10.0.0.1")
	got := r.List()
	if len(got) != 1 {
		t.Fatalf("%d devices listed, want 1", len(got))
	}
	if i := got[0].Info; len(i.Alias) != names.Max || len(i.DeviceModel) != names.Max || len(i.DeviceType) != names.Max || len(i.Version) != names.Max {
		t.Errorf("kept %d, %d, %d and %d bytes of name, model, type and version; want %d of each", len(i.Alias), len(i.DeviceModel), len(i.DeviceType), len(i.Version), names.Max)
	}
	r.Seen(Info{Fingerprint: "evil\u202eback", Alias: "x"}, "10.0.0.1")
	r.Seen(Info{Fingerprint: huge}, "10.0.0.1")
	if n := len(r.List()); n != 1 {
		t.Errorf("a device with a fingerprint not fit to keep was listed: %d devices", n)
	}
	r.Seen(Info{Fingerprint: "named", Alias: "a\nb\u202ec "}, "10.0.0.1")
	if got := r.peers["named"].info.Alias; got != "abc" {
		t.Errorf("a name was kept as %q, want %q", got, "abc")
	}

	for i := range 2 * maxPeersFrom {
		r.Seen(Info{Fingerprint: fmt.Sprint("flood", i)}, "10.0.0.1")
	}
	if n := len(r.List()); n != maxPeersFrom {
		t.Errorf("one address added %d devices, want %d", n, maxPeersFrom)
	}
	// A device already listed is still updated, and another address still
	// adds its own.
	r.Seen(Info{Fingerprint: "fp", Alias: "again"}, "10.0.0.1")
	r.Seen(Info{Fingerprint: "other"}, "10.0.0.2")
	if r.peers["fp"].info.Alias != "again" || r.peers["other"] == nil {
		t.Error("the limit for one address stopped an update, or a device at another address")
	}
}

func TestSeenListIsBounded(t *testing.T) {
	r := &Registry{Self: "me"}
	for i := range maxPeers {
		r.Seen(Info{Fingerprint: fmt.Sprint("fp", i)}, fmt.Sprint("10.0.", i/250, ".", i%250))
	}
	r.Seen(Info{Fingerprint: "flood"}, "10.1.0.1")
	if n := len(r.List()); n != maxPeers {
		t.Fatalf("%d devices listed, want %d", n, maxPeers)
	}
	if _, ok := r.peers["flood"]; ok {
		t.Fatal("a new device was listed beyond the limit")
	}
	// A device already listed is still updated, and quiet ones make room.
	r.Seen(Info{Fingerprint: "fp1", Alias: "again"}, "10.0.0.1")
	r.peers["fp2"].lastSeen = time.Now().Add(-2 * peerStale)
	r.Seen(Info{Fingerprint: "newcomer"}, "10.1.0.1")
	if _, ok := r.peers["newcomer"]; !ok {
		t.Fatal("a stale device did not make room")
	}
	if len(r.peers) > maxPeers {
		t.Fatalf("%d devices kept", len(r.peers))
	}
}
