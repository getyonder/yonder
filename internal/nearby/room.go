package nearby

import (
	"context"
	"errors"
)

// Pairing by a code, for two devices that are not at the same address and
// so are not nearby: one opens a room at the meeting point and is given a
// short code, its owner tells the other owner the code, and the other
// device joins the room with it. In the room each is the other's only
// device nearby, and they pair as nearby devices do, the device that came
// with the code asking. The room's code only brings the two together: the
// meeting point knows it and somebody may guess it, so the code the owners
// compare afterwards is what keeps the pairing safe, as it is nearby.

// Reasons a room cannot be had, or ends.
var (
	ErrNoRoom    = errors.New("no room has that code")
	ErrTooMany   = errors.New("too many codes that named no room")
	ErrFull      = errors.New("the meeting point has no room free")
	ErrOldServer = errors.New("the meeting point is from before rooms")
)

// Room is a device's place in a room, on a connection of its own.
type Room struct {
	// Code is the room's code.
	Code string

	stop context.CancelFunc
	done chan struct{}
	err  error
}

// OpenRoom opens a room and returns once the meeting point has given its
// code. The other device asks to pair when it comes, so set Asked. A client
// is for one room or for Run, never both.
func (c *Client) OpenRoom(ctx context.Context) (*Room, error) {
	return c.enter(ctx, RoomNew)
}

// JoinRoom goes to the room with the given code and asks the device that
// opened it to pair; it returns once both hold the code to compare, as
// Pair does. When the pairing is over, close the room.
func (c *Client) JoinRoom(ctx context.Context, code string) (*Room, *Session, error) {
	r, err := c.enter(ctx, code)
	if err != nil {
		return nil, nil, err
	}
	peers := c.Peers()
	if len(peers) != 1 {
		r.Close()
		return nil, nil, ErrNoRoom
	}
	s, err := c.Pair(ctx, peers[0].ID)
	if err != nil {
		r.Close()
		return nil, nil, err
	}
	return r, s, nil
}

// enter connects for a room and waits for the welcome to it. The
// connection then lasts until the room ends or is closed.
func (c *Client) enter(ctx context.Context, room string) (*Room, error) {
	c.room, c.welcomed = room, make(chan struct{})
	run, stop := context.WithCancel(context.Background())
	r := &Room{stop: stop, done: make(chan struct{})}
	go func() {
		err := c.connect(run)
		if run.Err() == nil {
			r.err = err
		}
		stop()
		close(r.done)
	}()
	select {
	case <-c.welcomed:
		c.mu.Lock()
		r.Code = c.code
		c.mu.Unlock()
		return r, nil
	case <-r.done:
		return nil, r.err
	case <-ctx.Done():
		r.Close()
		return nil, ctx.Err()
	}
}

// Close leaves the room, which ends it for the other device too.
func (r *Room) Close() {
	r.stop()
	<-r.done
}

// Done is closed when this device is no longer in the room.
func (r *Room) Done() <-chan struct{} { return r.done }

// Err is why the room ended: ErrGone when the other device left, ErrNoRoom
// when nobody came in time, ErrOffline when the connection was lost, or nil
// if this device closed it.
func (r *Room) Err() error {
	select {
	case <-r.done:
		return r.err
	default:
		return nil
	}
}
