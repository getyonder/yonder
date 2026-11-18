// The tests in the files named _backend_test.go start a meeting point, which
// is back-end code; packaging/publish.sh leaves these files out of the public
// repository.
package gui

import (
	"net/http/httptest"
	"strings"
	"testing"

	"yonder/internal/meeting"
	"yonder/internal/settings"
)

// meetingPoint starts a meeting point; every test device arrives from
// 127.0.0.1, so all of them are nearby.
func meetingPoint(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(meeting.New(meeting.DefaultLimits))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// Devices at the same meeting point list each other as nearby and to be
// paired; one that is also seen on the local network is listed once, as
// the local network has it.
func TestNearbyThroughMeetingPoint(t *testing.T) {
	url := meetingPoint(t)
	a, wa, _ := deviceAt(t, "Ada", url)
	b, wb, _ := deviceAt(t, "Bo", url)

	st := wa.until(t, "Ada to list Bo", func(s State) bool { return row(s, "Bo") != nil })
	if d := row(st, "Bo"); !d.Pairable || !d.Nearby || d.Paired || d.Presence != "online" || !strings.HasPrefix(d.ID, meetPrefix) {
		t.Fatalf("Bo in Ada's list: %+v", d)
	}
	if !st.Visible {
		t.Error("Ada should be shown to nearby devices unless turned off")
	}
	wb.until(t, "Bo to list Ada", func(s State) bool { return row(s, "Ada") != nil })

	// Sending needs a pairing first.
	if err := a.Send(row(st, "Bo").ID, []string{"/nonexistent"}); err == nil {
		t.Error("files were sent to a device that is not paired")
	}

	fp := introduce(a, b)
	a.changed()
	st = wa.until(t, "Ada to list Bo once, from the local network", func(s State) bool {
		d := row(s, "Bo")
		return d != nil && !d.Pairable
	})
	if len(st.Devices) != 1 || st.Devices[0].ID != fp {
		t.Fatalf("Ada's list: %+v, want Bo once", st.Devices)
	}
}

// A paired device that is at the meeting point is not listed a second time,
// and counts as on the same network once the tunnel has answered.
func TestPairedAtMeetingPoint(t *testing.T) {
	url := meetingPoint(t)
	a, wa, _ := deviceAt(t, "Ada", url)
	b, _, _ := deviceAt(t, "Bo", url)
	wa.until(t, "Ada to list Bo", func(s State) bool { return row(s, "Bo") != nil })

	fp := b.node.Info.Fingerprint
	for _, presence := range []string{"checking", "offline", "online"} {
		bo := &Device{ID: fp, Name: "Bo", Paired: true, Presence: presence}
		a.mu.Lock()
		more := a.meetDevices(map[string]*Device{fp: bo})
		a.mu.Unlock()
		if len(more) != 0 {
			t.Errorf("%s: Bo is listed again: %+v", presence, more)
		}
		if want := presence == "online"; bo.Nearby != want {
			t.Errorf("%s: nearby is %v, want %v", presence, bo.Nearby, want)
		}
	}
}

// Turning "show this device" off takes it out of the others' lists and is
// remembered; turning it on brings it back.
func TestSetVisible(t *testing.T) {
	url := meetingPoint(t)
	a, wa, _ := deviceAt(t, "Ada", url)
	_, wb, _ := deviceAt(t, "Bo", url)
	wb.until(t, "Bo to list Ada", func(s State) bool { return row(s, "Ada") != nil })

	if err := a.SetVisible(false); err != nil {
		t.Fatal(err)
	}
	wb.until(t, "Ada to leave Bo's list", func(s State) bool { return row(s, "Ada") == nil })
	st := wa.until(t, "Ada to stop listing nearby devices", func(s State) bool { return !s.Visible && len(s.Devices) == 0 })
	if st.Visible {
		t.Error("still visible")
	}
	if set, err := settings.Load(a.dir); err != nil || !set.HideNearby {
		t.Errorf("the choice was not saved: %+v, %v", set, err)
	}

	if err := a.SetVisible(true); err != nil {
		t.Fatal(err)
	}
	wb.until(t, "Ada to be back in Bo's list", func(s State) bool { return row(s, "Ada") != nil })
	wa.until(t, "Ada to list Bo again", func(s State) bool { return s.Visible && row(s, "Bo") != nil })
}

// Asking a nearby device to pair puts the same code on both; a no on
// either takes the dialog off both.
func TestCodeDialog(t *testing.T) {
	url := meetingPoint(t)
	a, wa, _ := deviceAt(t, "Ada", url)
	b, wb, _ := deviceAt(t, "Bo", url)
	st := wa.until(t, "Ada to list Bo", func(s State) bool { return row(s, "Bo") != nil })
	wb.until(t, "Bo to list Ada", func(s State) bool { return row(s, "Ada") != nil })

	ask := func() (State, State) {
		t.Helper()
		if err := a.PairNearby(row(st, "Bo").ID); err != nil {
			t.Fatal(err)
		}
		sa := wa.until(t, "Ada's code", func(s State) bool { return s.Code != nil })
		sb := wb.until(t, "Bo's code", func(s State) bool { return s.Code != nil })
		if len(sa.Code.Code) != 6 || sa.Code.Code != sb.Code.Code {
			t.Fatalf("codes %q and %q, want the same six digits", sa.Code.Code, sb.Code.Code)
		}
		if sa.Code.Name != "Bo" || sa.Code.Asked || sb.Code.Name != "Ada" || !sb.Code.Asked {
			t.Fatalf("dialogs: Ada's %+v, Bo's %+v", sa.Code, sb.Code)
		}
		return sa, sb
	}

	ask()
	if err := a.PairNearby(row(st, "Bo").ID); err == nil {
		t.Error("a second pairing was started while one is open")
	}
	b.AnswerCode(false)
	wb.until(t, "Bo's dialog to close", func(s State) bool { return s.Code == nil })
	wa.until(t, "Ada's dialog to close", func(s State) bool { return s.Code == nil })
	wa.untilSaid(t, "Bo did not pair.")

	ask()
	a.AnswerCode(false)
	wa.until(t, "Ada's dialog to close", func(s State) bool { return s.Code == nil })
	wb.until(t, "Bo's dialog to close", func(s State) bool { return s.Code == nil })
	wb.untilSaid(t, "Ada did not pair.")

	// A yes waits for the other owner, who can still say no.
	ask()
	b.AnswerCode(true)
	wb.until(t, "Bo to wait for Ada", func(s State) bool { return s.Code != nil && s.Code.Waiting })
	a.AnswerCode(false)
	wb.until(t, "Bo's dialog to close", func(s State) bool { return s.Code == nil })
	if paired, _ := b.node.Paired(); len(paired) != 0 {
		t.Errorf("Bo paired with %+v after Ada said no", paired)
	}
}
