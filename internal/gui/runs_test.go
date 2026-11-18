package gui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunsWhenOpened(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for name, want := range map[string]bool{
		"setup.exe": true, "SETUP.EXE": true, "a.msi": true, "a.bat": true, "a.lnk": true,
		"a.url": true, "a.desktop": true, "a.sh": true, "a.command": true, "a.dmg": true,
		"Yonder.AppImage": true, "a.deb": true, "a.rpm": true, "a.ps1": true, "a.js": true,
		"report.pdf.exe": true, "trick.exe . ": true,
		"a.pyw": true, "a.msc": true, "a.diagcab": true, "a.appx": true, "a.msixbundle": true,
		"a.appinstaller": true, "a.application": true, "a.jnlp": true,
		"photo.jpg": false, "notes.txt": false, "archive.zip": false, "README": false,
	} {
		if got := runsWhenOpened(write(name, 0o644)); got != want {
			t.Errorf("%q: got %v, want %v", name, got, want)
		}
	}
	if !runsWhenOpened(write("tool", 0o755)) {
		t.Error("an executable with no extension should count")
	}
	if runsWhenOpened(filepath.Join(dir, "missing")) {
		t.Error("a file that is not there should not count")
	}
}

func TestOpenReceivedShowsTheFolderOfAProgram(t *testing.T) {
	dir := t.TempDir()
	prog, doc := filepath.Join(dir, "setup.exe"), filepath.Join(dir, "notes.txt")
	w := &window{}
	s := &Service{shell: w, received: []Received{{Path: doc}, {Path: prog}}}
	if err := s.OpenReceived(0); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenReceived(1); err != nil {
		t.Fatal(err)
	}
	if len(w.opened) != 2 || w.opened[0] != doc || w.opened[1] != dir {
		t.Fatalf("opened %q", w.opened)
	}
}
