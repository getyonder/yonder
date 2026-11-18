package gui

import (
	"context"
	"testing"
	"time"

	"yonder/internal/nettest"
)

// spaced is a code as the window shows it and a person may type it.
func spaced(code string) string { return " " + code[:3] + " " + code[3:] + " " }

// showCode has a device show a code and returns it.
func showCode(t *testing.T, s *Service, w *window) string {
	t.Helper()
	s.ShowCode()
	st := w.until(t, "a code to show", func(s State) bool { return s.Room != nil && (s.Room.Code != "" || s.Room.Problem != "") })
	if len(st.Room.Code) != 6 || st.Room.Problem != "" {
		t.Fatalf("the tab shows %+v, want a code of six digits", st.Room)
	}
	return st.Room.Code
}

// One device shows a code and the other types it: both then show the same
// code to compare, as nearby devices do, and a no on either takes the
// dialog off both. Neither is shown to nearby devices, which does not
// matter.
func TestCodeTab(t *testing.T) {
	url := meetingPoint(t)
	a, wa, _ := deviceAt(t, "Ada", url)
	b, wb, _ := deviceAt(t, "Bo", url)
	for _, s := range []*Service{a, b} {
		if err := s.SetVisible(false); err != nil {
			t.Fatal(err)
		}
	}

	meet := func() {
		t.Helper()
		code := showCode(t, a, wa)
		if err := b.UseCode(spaced(code)); err != nil {
			t.Fatal(err)
		}
		sa := wa.until(t, "Ada's code to compare", func(s State) bool { return s.Code != nil })
		sb := wb.until(t, "Bo's code to compare", func(s State) bool { return s.Code != nil })
		if len(sa.Code.Code) != 6 || sa.Code.Code != sb.Code.Code {
			t.Fatalf("codes %q and %q, want the same six digits", sa.Code.Code, sb.Code.Code)
		}
		if sa.Code.Name != "Bo" || !sa.Code.Asked || sb.Code.Name != "Ada" || sb.Code.Asked {
			t.Fatalf("dialogs: Ada's %+v, Bo's %+v", sa.Code, sb.Code)
		}
		if sa.Room != nil {
			t.Errorf("Ada still shows the code to type, %+v, beside the code to compare", sa.Room)
		}
	}

	meet()
	if err := b.UseCode("123456"); err == nil {
		t.Error("a second pairing was started while one is open")
	}
	b.AnswerCode(false)
	wb.until(t, "Bo's dialog to close", func(s State) bool { return s.Code == nil })
	wa.until(t, "Ada's dialog to close", func(s State) bool { return s.Code == nil })
	wa.untilSaid(t, "Bo did not pair.")

	meet()
	a.AnswerCode(false)
	wa.until(t, "Ada's dialog to close", func(s State) bool { return s.Code == nil })
	wb.until(t, "Bo's dialog to close", func(s State) bool { return s.Code == nil })
	wb.untilSaid(t, "Ada did not pair.")
}

// What a person is told when the code is not one, names no room, or there
// is no meeting point to ask.
func TestCodeTabProblems(t *testing.T) {
	a, wa, _ := deviceAt(t, "Ada", meetingPoint(t))
	for _, code := range []string{"", "12345", "1234567", "12345a", "١٢٣٤٥٦"} {
		if err := a.UseCode(code); err == nil || err.Error() != "A code has six digits." {
			t.Errorf("code %q: %v", code, err)
		}
	}
	mine := showCode(t, a, wa)
	other := "000000"
	if mine == other {
		other = "000001"
	}
	const none = "No device shows that code. Check the code; it works once, for 10 minutes."
	if err := a.UseCode(other); err == nil || err.Error() != none {
		t.Errorf("a code nobody shows: %v", err)
	}
	a.StopCode()
	wa.until(t, "the code to go", func(s State) bool { return s.Room == nil })

	b, wb, _ := device(t, "Bo")
	const unavailable = "Pairing by a code is not available here."
	if err := b.UseCode("123456"); err == nil || err.Error() != unavailable {
		t.Errorf("without a meeting point: %v", err)
	}
	b.ShowCode()
	if st := wb.until(t, "Bo to be told", func(s State) bool { return s.Room != nil && s.Room.Problem != "" }); st.Room.Problem != unavailable {
		t.Errorf("without a meeting point the tab says %q", st.Room.Problem)
	}
}

// The whole of it: a code shown, typed, the codes compared on both, and
// the two are paired through their tunnels. Through the relay, so only with
// YONDER_NET_TESTS.
func TestPairByCode(t *testing.T) {
	nettest.Need(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	url := meetingPoint(t)
	a, wa, _ := deviceAt(t, "Ada", url)
	b, wb, _ := deviceAt(t, "Bo", url)
	for _, s := range []*Service{a, b} {
		if err := s.node.OpenTunnel(ctx); err != nil {
			t.Fatalf("tunnel: %v", err)
		}
	}
	if err := b.UseCode(showCode(t, a, wa)); err != nil {
		t.Fatal(err)
	}
	wa.until(t, "Ada's code to compare", func(s State) bool { return s.Code != nil })
	wb.until(t, "Bo's code to compare", func(s State) bool { return s.Code != nil })
	b.AnswerCode(true)
	wb.until(t, "Bo to wait for Ada", func(s State) bool { return s.Code != nil && s.Code.Waiting })
	a.AnswerCode(true)
	wa.untilSaid(t, "Paired with Bo.")
	wb.untilSaid(t, "Paired with Ada.")
	for _, s := range []*Service{a, b} {
		if paired, _ := s.node.Paired(); len(paired) != 1 {
			t.Errorf("%s has %d paired devices, want 1", s.set.Name, len(paired))
		}
	}
	wa.until(t, "Ada's dialog to close", func(s State) bool { return s.Code == nil && s.Room == nil })
	wb.until(t, "Bo's dialog to close", func(s State) bool { return s.Code == nil })
}
