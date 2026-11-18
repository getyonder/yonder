package main

import (
	"os"
	"testing"

	"yonder/internal/control"
	"yonder/internal/gui"
)

// running is a Yonder that answers terminal commands and says this version.
type running string

func (running) List() (string, []gui.Device)                    { return "Quiet Harbor", nil }
func (running) SendWait(string, []string) (string, bool, error) { return "", false, nil }
func (r running) AppVersion() string                            { return string(r) }

// Only a running Yonder that does not say its version is taken for an older
// one: none running, or one that says its version, starts Yonder as before.
func TestOlderRuns(t *testing.T) {
	// The socket's folder must be its owner's alone.
	runtime := t.TempDir()
	if err := os.Chmod(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	dir := t.TempDir()
	if olderRuns(dir) {
		t.Fatal("no Yonder runs, and an older one was found")
	}
	for _, c := range []struct {
		says string
		want bool
	}{{"", true}, {"0.1.29", false}} {
		stop, err := control.Serve(control.Path(dir), running(c.says))
		if err != nil {
			t.Fatal(err)
		}
		if got := olderRuns(dir); got != c.want {
			t.Errorf("a running Yonder that says %q: older %v, want %v", c.says, got, c.want)
		}
		stop()
	}
}
