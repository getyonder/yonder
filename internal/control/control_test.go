package control

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"yonder/internal/gui"
)

// fake is a running Yonder with a fixed list, that records what it is
// asked to send.
type fake struct {
	devices []gui.Device
	mu      sync.Mutex
	sentTo  string
	paths   []string
}

func (f *fake) List() (string, []gui.Device) { return "Quiet Harbor", f.devices }

func (f *fake) AppVersion() string { return "0.1.29" }

func (f *fake) SendWait(id string, paths []string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "gone" {
		return "", false, errors.New("That device is no longer in the list.")
	}
	f.sentTo, f.paths = id, paths
	return "Sent.", id != "declines", nil
}

var list = []gui.Device{
	{ID: "a1", Name: "Swift Comet", Label: "Mum's laptop", Paired: true, Presence: "online"},
	{ID: "b2", Name: "Kind Pineapple", Nearby: true, Presence: "online", LocalSend: true},
	{ID: "c3", Name: "Twin", Nearby: true, Presence: "online"},
	{ID: "d4", Name: "twin", Paired: true, Presence: "offline"},
	{ID: "e5", Name: "Stranger", Pairable: true, Presence: "online"},
	{ID: "declines", Name: "No Thanks", Nearby: true, Presence: "online"},
}

// serve runs a fake Yonder at a socket of its own.
func serve(t *testing.T) (*fake, string) {
	t.Helper()
	f := &fake{devices: list}
	path := sockPath(t)
	stop, err := Serve(path, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return f, path
}

func TestDevices(t *testing.T) {
	_, path := serve(t)
	got, err := Devices(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != Version || !got.Running || got.App != "0.1.29" || got.Name != "Quiet Harbor" || !slices.Equal(got.Devices, list) {
		t.Fatalf("list: %+v", got)
	}
}

func TestSend(t *testing.T) {
	f, path := serve(t)
	files := []string{"/tmp/one", "/tmp/two"}
	for _, c := range []struct {
		why  string
		ask  Send
		to   string // the device the app is asked to send to; "" for none
		ok   bool
		note string // what the note must contain
	}{
		{"by ID", Send{ID: "b2", Paths: files}, "b2", true, "Sent."},
		{"by name, in any case", Send{Device: "swift comet", Paths: files}, "a1", true, "Sent."},
		{"by the owner's own name", Send{Device: "Mum's Laptop", Paths: files}, "a1", true, "Sent."},
		{"a device that takes nothing", Send{ID: "declines", Paths: files}, "declines", false, "Sent."},
		{"two of one name", Send{Device: "Twin", Paths: files}, "", false, "2 devices are called"},
		{"no such name", Send{Device: "Nobody", Paths: files}, "", false, `no paired or nearby device is called "Nobody"`},
		{"a device not yet paired", Send{Device: "Stranger", Paths: files}, "", false, `is called "Stranger"`},
		{"an ID the app no longer lists", Send{ID: "gone", Paths: files}, "", false, "no longer in the list"},
		{"no files", Send{ID: "b2"}, "", false, "No files"},
	} {
		f.mu.Lock()
		f.sentTo, f.paths = "", nil
		f.mu.Unlock()
		got, err := SendFiles(context.Background(), path, c.ask)
		if err != nil {
			t.Fatalf("%s: %v", c.why, err)
		}
		f.mu.Lock()
		to, paths := f.sentTo, f.paths
		f.mu.Unlock()
		if got.OK != c.ok || !strings.Contains(got.Note, c.note) || to != c.to {
			t.Errorf("%s: answer %+v, sent to %q; want ok=%v, a note with %q, sent to %q", c.why, got, to, c.ok, c.note, c.to)
		}
		if c.to != "" && !slices.Equal(paths, files) {
			t.Errorf("%s: paths %q", c.why, paths)
		}
	}
}

func TestNotRunning(t *testing.T) {
	path := sockPath(t)
	if _, err := Devices(context.Background(), path); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("with no socket: %v", err)
	}
	if _, err := SendFiles(context.Background(), path, Send{ID: "a1", Paths: []string{"/tmp/one"}}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("send with no socket: %v", err)
	}
	// After a Yonder has stopped, too.
	stop, err := Serve(path, &fake{})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if _, err := Devices(context.Background(), path); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("after stop: %v", err)
	}
}

func TestSocketLeftBehindIsReplacedAndOneInUseIsNot(t *testing.T) {
	path := sockPath(t)
	// A Yonder that was killed leaves its socket in the folder.
	dead, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	dead.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the test did not leave a socket behind:", err)
	}
	stop, err := Serve(path, &fake{devices: list})
	if err != nil {
		t.Fatal("over a socket left behind:", err)
	}
	defer stop()
	if _, err := Devices(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if _, err := Serve(path, &fake{}); err == nil {
		t.Fatal("a second Yonder took a socket that was in use")
	}
	if got, err := Devices(context.Background(), path); err != nil || len(got.Devices) != len(list) {
		t.Fatalf("the first Yonder after the second tried: %+v, %v", got, err)
	}
}

func TestSocketIsTheOwnersAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no such file modes")
	}
	_, path := serve(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("socket mode %o", mode)
	}
}

func TestPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	a, b := Path("/home/me/.config/yonder"), Path("/home/me/other")
	if a == b || filepath.Dir(a) != "/run/user/1000" {
		t.Fatalf("paths %q and %q", a, b)
	}
	wd, _ := os.Getwd()
	if Path("x") != Path(filepath.Join(wd, "x")) {
		t.Fatal("a folder named two ways has two sockets")
	}
	if len(a) > 100 {
		t.Fatalf("%q is too long for a socket on some systems", a)
	}
}

func TestPathWithoutRuntimeDirIsInAFolderOfItsOwn(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TEMP", tmp)
	p := Path("/home/me/.config/yonder")
	if filepath.Dir(p) == os.TempDir() || filepath.Dir(filepath.Dir(p)) != os.TempDir() {
		t.Fatalf("%q is not in a folder of its own in %q", p, os.TempDir())
	}
}

func TestServeMakesItsFolderPrivateAndRefusesAWiderOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no such file modes")
	}
	made := filepath.Join(t.TempDir(), "yonder-1")
	stop, err := Serve(filepath.Join(made, "y.sock"), &fake{})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if info, err := os.Stat(made); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("folder made: %v %v", info, err)
	}
	wide := filepath.Join(t.TempDir(), "wide")
	if err := os.Mkdir(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Serve(filepath.Join(wide, "y.sock"), &fake{}); err == nil {
		t.Fatal("a folder open to others was used")
	}
	// The commands do not speak to a socket there either.
	if _, err := Devices(context.Background(), filepath.Join(wide, "y.sock")); err == nil || errors.Is(err, ErrNotRunning) {
		t.Fatalf("a command used a folder open to others: err %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(made, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Serve(filepath.Join(link, "y.sock"), &fake{}); err == nil {
		t.Fatal("a link in place of the folder was used")
	}
}

func TestSendRequestHasASizeLimit(t *testing.T) {
	_, path := serve(t)
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	defer client.CloseIdleConnections()
	big := `{"device":"` + strings.Repeat("a", maxBody) + `"}`
	resp, err := client.Post("http://yonder/send", "application/json", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestServerClosesSlowConnections(t *testing.T) {
	srv := newServer(http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.IdleTimeout <= 0 || srv.MaxHeaderBytes <= 0 {
		t.Fatalf("limits not set: %+v", srv)
	}
}

// sockPath is a socket path in a folder only the test user can use, as
// Serve asks.
func sockPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "y.sock")
}

func TestSocketFolderOnWindowsIsUnderTheUsersOwnData(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache) // UserCacheDir on Linux; %LocalAppData% on Windows
	t.Setenv("LocalAppData", cache)
	if got, want := socketFolder("windows"), filepath.Join(cache, "Yonder"); got != want {
		t.Fatalf("windows folder %q, want %q", got, want)
	}
	if got := socketFolder("linux"); !strings.HasPrefix(got, filepath.Join(os.TempDir(), "yonder-")) {
		t.Fatalf("linux folder %q is not under the temporary folder", got)
	}
}
