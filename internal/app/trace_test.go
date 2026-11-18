package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSendLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, TraceName)
	log := openTrace(dir)
	log.logf("send to %s: started, files: %d", "Quiet Harbor", 2)
	log.close()
	log.logf("after it is closed") // written nowhere
	got, err := os.ReadFile(path)
	if err != nil || !strings.HasSuffix(string(got), " send to Quiet Harbor: started, files: 2\n") || bytes.Count(got, []byte("\n")) != 1 {
		t.Fatalf("log is %q, err %v, want the one line with its time", got, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("log has mode %v, want 600", info.Mode().Perm())
	}

	// A log that has grown large starts again; a small one is kept.
	openTrace(dir).close()
	if kept, _ := os.ReadFile(path); !bytes.Equal(kept, got) {
		t.Fatal("a small log was not kept")
	}
	if err := os.WriteFile(path, make([]byte, traceMax+1), 0o600); err != nil {
		t.Fatal(err)
	}
	openTrace(dir).close()
	if info, _ := os.Stat(path); info.Size() != 0 {
		t.Fatalf("a large log was kept, %d bytes", info.Size())
	}

	// No log is no fault.
	var none *tracer
	none.logf("nothing")
	none.close()
}

func TestSendLogStaysSmallWhileWritten(t *testing.T) {
	dir := t.TempDir()
	log := openTrace(dir)
	defer log.close()
	// One long line is cut.
	log.logf("%s", strings.Repeat("x", 100000))
	if info, _ := os.Stat(filepath.Join(dir, TraceName)); info.Size() > traceLineMax {
		t.Fatalf("a long line wrote %d bytes", info.Size())
	}
	// Many lines never take the log past its limit.
	for i := 0; i < 3*traceMax/traceLineMax; i++ {
		log.logf("%s", strings.Repeat("y", traceLineMax))
		if info, _ := os.Stat(filepath.Join(dir, TraceName)); info.Size() > traceMax {
			t.Fatalf("the log grew to %d bytes", info.Size())
		}
	}
}

// Text with a line break, in an error from another device for example,
// stays one line.
func TestSendLogLineStaysOneLine(t *testing.T) {
	dir := t.TempDir()
	log := openTrace(dir)
	log.logf("error: %v", "refused\n2026-11-16 00:00:00.000 a forged line\x1b[2J")
	log.close()
	got, err := os.ReadFile(filepath.Join(dir, TraceName))
	if err != nil || bytes.Count(got, []byte("\n")) != 1 || !bytes.HasSuffix(got, []byte("\n")) || bytes.ContainsRune(got, 0x1b) {
		t.Fatalf("log is %q, err %v, want one line without control characters", got, err)
	}
}

// A paired device's name is written quoted and cut in the send log, so it
// cannot start a line of its own or make one long.
func TestDeviceNameInTheSendLog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB, _ := wiredPair(t)
	dir := t.TempDir()
	a.log = openTrace(dir)
	a.ways.set(straight, relayed)
	b.ways.set(relayed, straight)
	toB.Name = "B\n2026-01-01 00:00:00.000 a line of its own" + strings.Repeat("N", 1000)
	if sent, err := a.SendPaired(ctx, toB, []string{file(t, []byte("x"))}); err != nil || sent != 1 {
		t.Fatalf("sent %d, err %v", sent, err)
	}
	a.log.close()
	got, err := os.ReadFile(filepath.Join(dir, TraceName))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(got), "\n"), "\n") {
		if !strings.Contains(line, ` "B\n2026`) || len(line) > 300 {
			t.Fatalf("a line of the log: %q", line)
		}
	}
}

// When an upload fails, the window's error names the file and the send log
// does not.
func TestFileNameStaysOutOfTheSendLog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	a, b, toB, _ := wiredPair(t)
	dir := t.TempDir()
	a.log = openTrace(dir)
	a.ways.set(straight, relayed)
	b.ways.set(relayed, straight)
	b.Receiver.SaveDir = file(t, []byte("not a folder")) // so the file cannot be saved
	sent, err := a.SendPaired(ctx, toB, []string{file(t, []byte("x"))})
	if err == nil || sent != 0 || !strings.Contains(err.Error(), secretName) {
		t.Fatalf("sent %d, err %v, want an error that names the file", sent, err)
	}
	a.log.close()
	got, _ := os.ReadFile(filepath.Join(dir, TraceName))
	if !strings.Contains(string(got), "error: upload: HTTP") || strings.Contains(string(got), secretName) {
		t.Fatalf("the log:\n%s", got)
	}
}
