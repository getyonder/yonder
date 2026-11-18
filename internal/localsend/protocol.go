// Package localsend speaks the LocalSend protocol, version 2
// (https://github.com/localsend/protocol). It has no interface and no
// knowledge of how a connection arrived, so the same code serves the local
// network and the tunnel.
package localsend

import (
	"strings"

	"yonder/internal/names"
)

const (
	// Version is the protocol version Yonder announces.
	Version = "2.1"
	// DefaultPort is LocalSend's default for both HTTP and multicast.
	DefaultPort = 53317

	apiPrefix = "/api/localsend/v2"
)

// Info describes a device. Port, Protocol and Announce are only sent when
// announcing or registering; replies leave them out.
type Info struct {
	Alias       string `json:"alias"`
	Version     string `json:"version"`
	DeviceModel string `json:"deviceModel,omitempty"`
	DeviceType  string `json:"deviceType,omitempty"`
	Fingerprint string `json:"fingerprint"`
	Port        int    `json:"port,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	Download    bool   `json:"download"`
	Announce    *bool  `json:"announce,omitempty"`
}

// encrypted reports whether the device takes files over TLS. One that says
// "http" (the stock LocalSend app with its encryption switched off) is sent
// to without: nothing then proves who is at its address, and the files can
// be read on the way. No protocol means https, as in the answer to a
// registration.
func (i Info) encrypted() bool {
	return i.Protocol == "" || i.Protocol == "https"
}

// maxFingerprint is the longest fingerprint taken from another device. One
// made from a certificate has 64 characters.
const maxFingerprint = 128

// cleaned is what another device said of itself in the form that is kept
// and shown: its names go through names.Clean, so they are short and hold
// no character that forges a log line or turns text round. It is not ok
// when the fingerprint, which is kept whole or not at all, is too long or
// holds such a character. Every Info from another device goes through it.
func (i Info) cleaned() (Info, bool) {
	if len(i.Fingerprint) > maxFingerprint || strings.ContainsFunc(i.Fingerprint, names.Bad) {
		return Info{}, false
	}
	i.Alias, i.DeviceModel = names.Clean(i.Alias), names.Clean(i.DeviceModel)
	i.Version, i.DeviceType, i.Protocol = names.Clean(i.Version), names.Clean(i.DeviceType), names.Clean(i.Protocol)
	return i, true
}

// reply is the form of Info sent as a response body.
func (i Info) reply() Info {
	i.Port, i.Protocol, i.Announce = 0, "", nil
	return i
}

// FileMeta describes one offered file.
type FileMeta struct {
	ID       string `json:"id"`
	FileName string `json:"fileName"`
	Size     int64  `json:"size"`
	FileType string `json:"fileType"`
	SHA256   string `json:"sha256,omitempty"`
	Preview  string `json:"preview,omitempty"`
}

type prepareRequest struct {
	Info  Info                `json:"info"`
	Files map[string]FileMeta `json:"files"`
}

type prepareResponse struct {
	SessionID string            `json:"sessionId"`
	Files     map[string]string `json:"files"` // file id to upload token
}

// maxLogText is how many characters of a value from another device a log
// line holds.
const maxLogText = 100

// Clip cuts a value from another device, such as its name, to maxLogText
// characters, so that what another device sends cannot make a log line long.
// It does not quote: a line that holds the value writes it with %q.
func Clip(s string) string {
	n := 0
	for i := range s {
		if n == maxLogText {
			return s[:i] + "…"
		}
		n++
	}
	return s
}
