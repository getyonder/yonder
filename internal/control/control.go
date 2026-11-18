// Package control lets a terminal command ask the Yonder that is already
// running, in place of starting a second one beside it.
//
// Two Yonders with one identity get in each other's way: each opens its own
// listener and its own tunnel connections under the same keys, and two that
// ask a paired device at the same moment can be told it is offline when it
// is not. So the running app listens on a socket only its owner can open,
// and `yonder devices` and `yonder send` go there first.
//
// What travels is HTTP with JSON bodies. List is also what `yonder devices
// -json` prints, and so what other programs read: its fields are added to,
// never renamed, and Version goes up only if that promise is broken.
package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"yonder/internal/gui"
)

// Version is the version of List's shape.
const Version = 1

// dialWait is how long a command waits for the socket to take it. The
// socket is on this machine, so a running app answers at once.
const dialWait = 2 * time.Second

// ErrNotRunning is returned when no Yonder is running for the folder.
var ErrNotRunning = errors.New("Yonder is not running")

// List is this device and the devices it can see.
type List struct {
	Version int `json:"version"`
	// Running is whether the answer is the running app's. If not, it is
	// what a Yonder started for the one question could find in a few
	// seconds.
	Running bool         `json:"running"`
	Name    string       `json:"name"`
	Devices []gui.Device `json:"devices"`
	// App is the version of the Yonder that answers. One from before
	// 0.1.30 does not say it.
	App string `json:"app,omitempty"`
}

// Send asks for files to be sent to one device, named by its ID or, with
// no ID, by its name or the owner's own name for it.
type Send struct {
	ID     string   `json:"id,omitempty"`
	Device string   `json:"device,omitempty"`
	Paths  []string `json:"paths"`
}

// Sent is how a Send ended.
type Sent struct {
	// OK is whether the device took files; Note says what happened, in
	// the words the window uses.
	OK   bool   `json:"ok"`
	Note string `json:"note"`
}

// App is what the running Yonder does for a command; *gui.Service is one.
type App interface {
	List() (name string, devices []gui.Device)
	AppVersion() string
	SendWait(id string, paths []string) (note string, ok bool, err error)
}

// Path is where the socket for a settings folder is. A socket's path may
// be only some hundred bytes long, so it is not in the folder itself.
func Path(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	sum := sha256.Sum256([]byte(dir))
	name := "yonder-" + hex.EncodeToString(sum[:8]) + ".sock"
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" {
		return filepath.Join(base, name)
	}
	return filepath.Join(socketFolder(runtime.GOOS), name)
}

// socketFolder is the folder of this user's own that the socket goes in
// when there is no runtime folder, which Serve makes and checks. The
// temporary folder is shared, so it is not used on Windows, where a
// folder's owner is not read: the user's local application data is.
func socketFolder(goos string) string {
	if goos == "windows" {
		if base, err := os.UserCacheDir(); err == nil {
			return filepath.Join(base, "Yonder")
		}
	}
	return filepath.Join(os.TempDir(), "yonder-"+strconv.Itoa(os.Getuid()))
}

// privateDir makes sure dir is a folder only this user can use: it is made
// with mode 700 if it is not there, and one that someone else owns, that is
// open to others, or that is a link is refused, since another user could
// have made it first and answer the commands.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return private(dir)
}

// private is the check of privateDir, for a folder that is there.
func private(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || !ownedByMe(fi) {
		return fmt.Errorf("%s is not a folder of yours", dir)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is open to other users (mode %o); it must be 700", dir, fi.Mode().Perm())
	}
	return nil
}

// Limits on what a command may send, and how long a connection may hold
// the socket without saying anything.
const (
	maxBody           = 1 << 20
	maxHeader         = 1 << 14
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 60 * time.Second
)

// newServer is the server for the socket: a connection that is slow or
// silent is closed, and requests have a size limit.
func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		MaxHeaderBytes:    maxHeader,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// Serve answers commands at path for app until the returned function is
// called. A socket left behind by a Yonder that is gone is replaced; one
// that still answers is an error.
func Serve(path string, app App) (stop func(), err error) {
	if err := privateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		if _, askErr := Devices(context.Background(), path); !errors.Is(askErr, ErrNotRunning) {
			return nil, fmt.Errorf("another Yonder answers at %s", path)
		}
		os.Remove(path)
		if ln, err = net.Listen("unix", path); err != nil {
			return nil, err
		}
	}
	// The folder is the owner's alone already; this says so of the
	// socket too.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /devices", func(w http.ResponseWriter, _ *http.Request) {
		name, devices := app.List()
		json.NewEncoder(w).Encode(List{Version: Version, Running: true, Name: name, Devices: devices, App: app.AppVersion()})
	})
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		var q Send
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			status := http.StatusBadRequest
			if _, tooBig := err.(*http.MaxBytesError); tooBig {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, err.Error(), status)
			return
		}
		json.NewEncoder(w).Encode(send(app, q))
	})
	srv := newServer(mux)
	go srv.Serve(ln)
	return func() {
		srv.Close()
		os.Remove(path)
	}, nil
}

// send does a Send and waits for its end. The command that asked may be
// gone by then; the transfer goes on all the same, as one begun in the
// window does when the window is closed.
func send(app App, q Send) Sent {
	if len(q.Paths) == 0 {
		return Sent{Note: "No files to send."}
	}
	if q.ID == "" {
		_, devices := app.List()
		id, err := Find(devices, q.Device)
		if err != nil {
			return Sent{Note: err.Error()}
		}
		q.ID = id
	}
	note, ok, err := app.SendWait(q.ID, q.Paths)
	if err != nil {
		return Sent{Note: err.Error()}
	}
	return Sent{OK: ok, Note: note}
}

// Find gives the ID of the device called name: by the name it gave itself
// or by the owner's own name for it, in either case. Devices that files
// cannot be sent to yet are passed over.
func Find(devices []gui.Device, name string) (string, error) {
	var ids, nearby []string
	for _, d := range devices {
		if d.Pairable {
			continue
		}
		if strings.EqualFold(d.Name, name) || (d.Label != "" && strings.EqualFold(d.Label, name)) {
			ids = append(ids, d.ID)
		} else {
			nearby = append(nearby, d.Name)
		}
	}
	switch {
	case len(ids) == 1:
		return ids[0], nil
	case len(ids) > 1:
		return "", fmt.Errorf("%d devices are called %q; say which with -id (`yonder devices -json` lists them)", len(ids), name)
	case len(nearby) == 0:
		return "", fmt.Errorf("no paired or nearby device is called %q, and no other devices are listed", name)
	}
	return "", fmt.Errorf("no paired or nearby device is called %q; I can see: %s", name, strings.Join(nearby, ", "))
}

// Devices asks the Yonder running at path for its List.
func Devices(ctx context.Context, path string) (List, error) {
	var l List
	return l, ask(ctx, path, http.MethodGet, "/devices", nil, &l)
}

// SendFiles asks the Yonder running at path to send files, and waits for
// the end of the transfer.
func SendFiles(ctx context.Context, path string, q Send) (Sent, error) {
	var s Sent
	return s, ask(ctx, path, http.MethodPost, "/send", q, &s)
}

// ask makes one request at the socket. It is ErrNotRunning if nothing
// listens there.
func ask(ctx context.Context, path, method, what string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			// Never speak to a socket in a folder someone else could
			// have made: it would be told which files to send.
			if err := private(filepath.Dir(path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			return (&net.Dialer{Timeout: dialWait}).DialContext(ctx, "unix", path)
		},
	}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, "http://yonder"+what, body)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		var dial *net.OpError
		if errors.As(err, &dial) && dial.Op == "dial" {
			return ErrNotRunning
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("the running Yonder said: %s", strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
