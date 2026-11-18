// Package nearby lets Yonders that cannot search the local network (a web
// page cannot) find the devices around them. Each holds a connection to a
// meeting point on Yonder's server (package meeting), which introduces
// devices that arrive from the same public address. Two of them can then
// pair: they agree a key between themselves and pass the pairing link under
// it, so the meeting point never sees one, and both screens show a short
// code that only matches when nobody sits in the middle.
//
// Two devices that are not at the same address can meet too, in a room: one
// opens it and is given a short code, and the other comes with the code its
// owner was told (room.go). They then pair in the same way.
//
// This file is what goes over the connection; the meeting point shares it.
package nearby

import (
	"encoding/json"
	"time"

	"yonder/internal/address"
)

// Path is where a site serves its meeting point.
const Path = "/nearby"

// DefaultURL is the meeting point on Yonder's own server, which the desktop
// app uses. The web version uses the one of the site it was opened from.
const DefaultURL = "wss://" + address.Service + Path

// MaxMessage is the largest message either side accepts, in bytes.
const MaxMessage = 4096

// Quiet is how long a connection may say nothing before the other side
// gives it up; the meeting point sends a ping every PingEvery, and a device
// answers each with a pong.
const (
	PingEvery = 25 * time.Second
	Quiet     = 70 * time.Second
)

// Kinds of device.
const (
	KindBrowser = "browser"
	KindApp     = "app"
)

// Peer is how a device appears to the others at the meeting point.
type Peer struct {
	// ID is given by the meeting point and lasts for one connection.
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Version is the device's version of Yonder (from 0.1.31), "dev" for
	// a development build; an older Yonder says none. The meeting point
	// passes it on and does not keep or count it.
	Version string `json:"version,omitempty"`
	// Salt is random for each connection, and Tag is worked out from it
	// and the device's certificate fingerprint. A device that already
	// knows the fingerprint (it is paired with this one, or sees it on the
	// local network) can recognise it by the tag; the meeting point, which
	// does not, cannot tell that two connections are the same device.
	Salt string `json:"salt"`
	Tag  string `json:"tag"`
}

// Message types.
const (
	TypeHello   = "hello"   // device: Peer says who it is, Room which room; always first
	TypeWelcome = "welcome" // meeting point: ID is yours, Peers are here, in Room
	TypeJoin    = "join"    // meeting point: Peer has arrived
	TypeLeave   = "leave"   // meeting point: ID has gone
	TypeTo      = "to"      // device: pass Body to ID
	TypeFrom    = "from"    // meeting point: Body, from ID
	TypePing    = "ping"    // meeting point: still there?
	TypePong    = "pong"    // device: yes
	// TypeBye is the meeting point's last word before it ends a
	// connection: Reason says why. The close of the connection carries the
	// reason too, but a web page's connection cannot read it there.
	TypeBye = "bye"
)

// Message is everything that goes over the connection, as JSON.
type Message struct {
	Type  string          `json:"t"`
	ID    string          `json:"id,omitempty"`
	Peer  *Peer           `json:"peer,omitempty"`
	Peers []Peer          `json:"peers,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
	// Room, in a hello, asks for a room in place of the devices nearby:
	// RoomNew for a new one, or the code of one that another device has
	// opened. In a welcome it is the code of the room the device is in. A
	// meeting point from before rooms leaves it out of its welcome.
	Room string `json:"room,omitempty"`
	// Reason is why the meeting point ends the connection, in a bye.
	Reason string `json:"reason,omitempty"`
}

// Reasons the meeting point gives when it ends a connection, which a
// device acts on.
const (
	ReasonFull = "full" // the address or the meeting point has no room
	ReasonBusy = "busy" // too many messages this minute, or too many wrong codes
	// ReasonNoRoom is for a room that is not there, has two devices
	// already or has ended. The three are not told apart, so that asking
	// tells a guesser nothing.
	ReasonNoRoom = "no room"
)

// RoomNew is what a hello's Room says to open a room.
const RoomNew = "new"

// RoomDigits is the length of a room's code.
const RoomDigits = 6
