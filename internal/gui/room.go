package gui

import (
	"context"
	"errors"
	"strings"
	"time"

	"yonder/internal/nearby"
)

// Pairing by a code, for two devices in different places whose owners can
// talk but cannot pass a link or show a QR code: one shows a short code,
// the other types it, and from there it is the pairing of two nearby
// devices, with its code to compare (see package nearby, room.go). The
// typed code only brings the two together at the meeting point.

const (
	// roomWait is how long the meeting point gets to give a code, or to
	// bring this device to the one that shows a code and have it answer.
	roomWait = 20 * time.Second
	// roomLinger is how long the device that showed the code stays in the
	// room after its part is over. The other device leaves when its own
	// part is over, which ends the room; this is for one that never does.
	roomLinger = 30 * time.Second
)

// Room is the "Use a code" tab while this device shows a code.
type Room struct {
	// Code is the six digits to type on the other device; empty until the
	// meeting point has given them, which takes a moment.
	Code string `json:"code"`
	// Problem says why there is no code, if there isn't.
	Problem string `json:"problem"`
}

type roomDialog struct {
	Room
	stop context.CancelFunc
}

// ShowCode opens a room at the meeting point and shows its code. When the
// other device comes with the code, the dialog for comparing codes takes
// over; AnswerCode takes it from there.
func (s *Service) ShowCode() {
	ctx, stop := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.room != nil {
		s.mu.Unlock()
		stop()
		return
	}
	dialog := &roomDialog{stop: stop}
	s.room = dialog
	c := s.roomClient()
	s.mu.Unlock()
	s.changed()

	// update changes the dialog, unless it has been closed meanwhile.
	update := func(f func(*Room)) {
		s.mu.Lock()
		if s.room == dialog {
			f(&dialog.Room)
		}
		s.mu.Unlock()
		s.changed()
	}
	if c == nil {
		update(func(v *Room) { v.Problem = "Pairing by a code is not available here." })
		return
	}
	asked := make(chan *nearby.Session, 1)
	c.Asked = func(session *nearby.Session) {
		select {
		case asked <- session:
		default: // only the first; a room has one other device
			session.Close()
		}
	}
	go func() {
		defer stop()
		openCtx, cancel := context.WithTimeout(ctx, roomWait)
		room, err := c.OpenRoom(openCtx)
		cancel()
		if err != nil {
			update(func(v *Room) { v.Problem = roomProblem(err) })
			return
		}
		update(func(v *Room) { v.Code = room.Code })
		select {
		case <-ctx.Done(): // closed by the person
			room.Close()
		case <-room.Done():
			update(func(v *Room) {
				switch err := room.Err(); {
				case errors.Is(err, nearby.ErrNoRoom):
					*v = Room{Problem: "This code has ended. Show a new one."}
				case errors.Is(err, nearby.ErrGone):
					*v = Room{Problem: "The other device left. Show a new code."}
				default:
					*v = Room{Problem: "The connection was lost. Show a code again."}
				}
			})
		case session := <-asked:
			// The tab has done its work; the dialog with the code to
			// compare is next.
			s.mu.Lock()
			showing := s.room == dialog
			if showing {
				s.room = nil
			}
			s.mu.Unlock()
			if !showing { // closed by the person just now
				session.Close()
				room.Close()
				return
			}
			code := s.showCode(session, true)
			s.changed()
			s.shell.Attention()
			if code != nil {
				<-code.ctx.Done()
				select {
				case <-room.Done():
				case <-time.After(roomLinger):
				}
			}
			room.Close()
		}
	}()
}

// StopCode stops showing a code; the code stops working.
func (s *Service) StopCode() {
	s.mu.Lock()
	dialog := s.room
	s.room = nil
	s.mu.Unlock()
	if dialog != nil {
		dialog.stop()
		s.changed()
	}
}

// UseCode goes to the device that shows the given code and asks it to
// pair. When it has answered, both devices show a code to compare;
// AnswerCode takes it from there.
func (s *Service) UseCode(code string) error {
	code = strings.Join(strings.Fields(code), "")
	if len(code) != nearby.RoomDigits || strings.Trim(code, "0123456789") != "" {
		return errors.New("A code has six digits.")
	}
	s.mu.Lock()
	c := s.roomClient()
	busy := s.code != nil
	s.mu.Unlock()
	if c == nil {
		return errors.New("Pairing by a code is not available here.")
	}
	if busy {
		return errors.New("Finish the pairing that is already open first.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), roomWait)
	defer cancel()
	room, session, err := c.JoinRoom(ctx, code)
	if err != nil {
		return errors.New(roomProblem(err))
	}
	dialog := s.showCode(session, true)
	if dialog == nil {
		room.Close()
		return errors.New("Finish the pairing that is already open first.")
	}
	go func() {
		<-dialog.ctx.Done()
		room.Close()
	}()
	return nil
}

// roomClient makes a client for one room, or nil without a meeting point.
// It is apart from the one that shows this device to those nearby, so
// pairing by a code works with that turned off. s.mu must be held.
func (s *Service) roomClient() *nearby.Client {
	if s.meeting == "" {
		return nil
	}
	return &nearby.Client{URL: s.meeting, Name: s.set.Name, Kind: s.kind, Fingerprint: s.node.Info.Fingerprint}
}

// roomProblem says why a room could not be opened or joined.
func roomProblem(err error) string {
	switch {
	case errors.Is(err, nearby.ErrNoRoom):
		return "No device shows that code. Check the code; it works once, for 10 minutes."
	case errors.Is(err, nearby.ErrTooMany):
		return "Too many wrong codes from this network. Wait 10 minutes, or pair with a link instead."
	case errors.Is(err, nearby.ErrFull):
		return "Too many codes are in use just now. Try again in a few minutes."
	case errors.Is(err, nearby.ErrOldServer):
		return "This needs a newer server."
	case errors.Is(err, nearby.ErrGone):
		return "The other device left. Ask for a new code and try again."
	case errors.Is(err, nearby.ErrRefused):
		return "That device is busy with another pairing. Ask for a new code and try again."
	case errors.Is(err, nearby.ErrOffline):
		return "Could not reach Yonder's server. Pairing by a code needs the internet; check the connection and try again."
	default:
		return "The other device did not answer. Ask for a new code and try again."
	}
}
