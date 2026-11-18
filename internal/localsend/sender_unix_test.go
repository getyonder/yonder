//go:build unix

package localsend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Only regular files are sent; a link to one counts as the file.
func TestSendTakesRegularFilesOnly(t *testing.T) {
	h := newHarness(t)
	h.rc.Accept = acceptAll
	dir := t.TempDir()
	pipe := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("no named pipe here: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	refused := make(chan error, 1)
	go func() { _, err := newSender(t).Send(ctx, h.peer(), []string{pipe}); refused <- err }()
	select {
	case err := <-refused:
		if err == nil || !strings.Contains(err.Error(), "not an ordinary file") {
			t.Fatalf("a named pipe: err %v, want it refused", err)
		}
	case <-ctx.Done():
		// Opening the pipe's other end lets the stuck send go on.
		if w, err := os.OpenFile(pipe, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			w.Close()
		}
		t.Fatal("a send of a named pipe held on")
	}
	data := []byte("through a link")
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(writeFile(t, "real.txt", data), link); err != nil {
		t.Fatal(err)
	}
	if sent, err := newSender(t).Send(ctx, h.peer(), []string{link}); err != nil || sent != 1 {
		t.Fatalf("a link to a file: sent %d, err %v", sent, err)
	}
}
