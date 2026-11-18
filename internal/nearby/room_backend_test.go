package nearby_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"yonder/internal/meeting"
	"yonder/internal/nearby"
)

// guest is a client that is not run: it is for a room.
func guest(name, url string) *device {
	d := &device{asked: make(chan *nearby.Session, 4)}
	d.Client = &nearby.Client{URL: url, Name: name, Kind: nearby.KindApp, Fingerprint: "fingerprint of " + name}
	d.Asked = func(s *nearby.Session) { d.asked <- s }
	d.SetRandom(&counter{seed: name})
	return d
}

// openRoom opens a room that is closed when the test ends.
func openRoom(t *testing.T, d *device) *nearby.Room {
	t.Helper()
	r, err := d.OpenRoom(within(t))
	if err != nil {
		t.Fatalf("%s could not open a room: %v", d.Name, err)
	}
	t.Cleanup(r.Close)
	return r
}

// over waits for a room to end and returns why.
func over(t *testing.T, r *nearby.Room) error {
	t.Helper()
	select {
	case <-r.Done():
		return r.Err()
	case <-time.After(wait):
		t.Fatal("the room never ended")
		return nil
	}
}

// One device opens a room, the other joins it with the code, and they pair
// as nearby devices do: the same code on both, the link across. A device
// that is nearby in the ordinary way sees neither. When one leaves, the
// room is over for the other.
func TestRoomPair(t *testing.T) {
	url := serve(t, meeting.New(meeting.DefaultLimits))
	near := start(t, url, "Near")
	until(t, "Near being connected", near.Connected)
	a, b := guest("A", url), guest("B", url)

	ra := openRoom(t, a)
	if len(ra.Code) != nearby.RoomDigits {
		t.Fatalf("the room's code is %q, want %d digits", ra.Code, nearby.RoomDigits)
	}
	rb, sb, err := b.JoinRoom(within(t), ra.Code)
	if err != nil {
		t.Fatal(err)
	}
	sa := a.request(t)
	if len(sb.Code) != 6 || sa.Code != sb.Code {
		t.Fatalf("codes %q and %q, want the same six digits", sa.Code, sb.Code)
	}
	if !sa.Asked || sb.Asked || sa.Peer.Name != "B" || sb.Peer.Name != "A" {
		t.Errorf("sessions: A's %+v, B's %+v", sa, sb)
	}
	if rb.Code != ra.Code {
		t.Errorf("B's room has the code %q, A's %q", rb.Code, ra.Code)
	}

	const link = "https://yonder.example/app/#pair=tcSECRET"
	if err := sb.SendLink(link); err != nil {
		t.Fatal(err)
	}
	got, err := sa.Link(within(t))
	if err != nil || got != link {
		t.Fatalf("A got %q, %v", got, err)
	}
	sa.Finish()
	sb.Finish()

	// All of that went by the meeting point without a word to the device
	// that is nearby in the ordinary way.
	if got := namesOf(near.Peers()); got != "" {
		t.Errorf("the nearby device saw %q, want nobody", got)
	}

	rb.Close()
	if err := over(t, ra); !errors.Is(err, nearby.ErrGone) {
		t.Fatalf("A's room ended with %v, want ErrGone", err)
	}
	if err := rb.Err(); err != nil {
		t.Errorf("the room B closed itself ended with %v, want nil", err)
	}
}

// The device that opened the room leaving ends a pairing under way.
func TestRoomOpenerLeaves(t *testing.T) {
	url := serve(t, meeting.New(meeting.DefaultLimits))
	a, b := guest("A", url), guest("B", url)
	ra := openRoom(t, a)
	rb, sb, err := b.JoinRoom(within(t), ra.Code)
	if err != nil {
		t.Fatal(err)
	}
	a.request(t)
	ra.Close()
	if err := ended(t, sb); !errors.Is(err, nearby.ErrGone) {
		t.Fatalf("B's session ended with %v, want ErrGone", err)
	}
	if err := over(t, rb); !errors.Is(err, nearby.ErrGone) {
		t.Fatalf("B's room ended with %v, want ErrGone", err)
	}
}

// A code that names no room, a room that has its two devices, too many
// wrong codes and a meeting point with no room free each have their own
// error.
func TestRoomRefusals(t *testing.T) {
	limits := meeting.DefaultLimits
	limits.Misses, limits.AddressRooms = 2, 1
	url := serve(t, meeting.New(limits))
	a, b := guest("A", url), guest("B", url)
	ra := openRoom(t, a)

	wrong := "000000"
	if ra.Code == wrong {
		wrong = "000001"
	}
	if _, _, err := guest("C", url).JoinRoom(within(t), wrong); !errors.Is(err, nearby.ErrNoRoom) {
		t.Fatalf("a wrong code: %v, want ErrNoRoom", err)
	}
	if _, err := guest("C", url).OpenRoom(within(t)); !errors.Is(err, nearby.ErrFull) {
		t.Fatalf("a second room from one address: %v, want ErrFull", err)
	}
	rb, _, err := b.JoinRoom(within(t), ra.Code)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rb.Close)
	if _, _, err := guest("C", url).JoinRoom(within(t), ra.Code); !errors.Is(err, nearby.ErrNoRoom) {
		t.Fatalf("a third device: %v, want ErrNoRoom", err)
	}
	// That was the second miss from this address.
	if _, _, err := guest("C", url).JoinRoom(within(t), wrong); !errors.Is(err, nearby.ErrTooMany) {
		t.Fatalf("after two misses: %v, want ErrTooMany", err)
	}
}

// A meeting point from before rooms takes no notice of what a hello says
// about a room and welcomes the device among those nearby. The device
// leaves at once, whether it wanted to open a room or to join one.
func TestRoomOldMeetingPoint(t *testing.T) {
	left := make(chan struct{}, 2)
	url := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		var hello nearby.Message
		if readJSON(r.Context(), c, &hello) != nil {
			return
		}
		writeJSON(r.Context(), c, nearby.Message{Type: nearby.TypeWelcome, ID: "you", Peers: []nearby.Peer{}})
		var msg nearby.Message
		for readJSON(r.Context(), c, &msg) == nil {
		}
		left <- struct{}{}
	}))
	if _, err := guest("A", url).OpenRoom(within(t)); !errors.Is(err, nearby.ErrOldServer) {
		t.Fatalf("opening a room: %v, want ErrOldServer", err)
	}
	if _, _, err := guest("B", url).JoinRoom(within(t), "123456"); !errors.Is(err, nearby.ErrOldServer) {
		t.Fatalf("joining a room: %v, want ErrOldServer", err)
	}
	for range 2 {
		select {
		case <-left:
		case <-time.After(wait):
			t.Fatal("a device stayed at the old meeting point")
		}
	}
}

// With no meeting point to reach there is no room.
func TestRoomOffline(t *testing.T) {
	url := serve(t, http.NotFoundHandler())
	if _, err := guest("A", url).OpenRoom(within(t)); !errors.Is(err, nearby.ErrOffline) {
		t.Fatalf("%v, want ErrOffline", err)
	}
}
