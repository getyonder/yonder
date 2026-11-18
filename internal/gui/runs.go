package gui

import (
	"os"
	"path/filepath"
	"strings"
)

// runExts are the file types the system runs, or installs, when they are
// opened: programs, scripts, shortcuts and installers on Windows, macOS and
// Linux.
var runExts = map[string]bool{
	".exe": true, ".msi": true, ".msix": true, ".bat": true, ".cmd": true, ".com": true,
	".scr": true, ".pif": true, ".ps1": true, ".psm1": true, ".vbs": true, ".vbe": true,
	".js": true, ".jse": true, ".wsf": true, ".wsh": true, ".hta": true, ".cpl": true,
	".reg": true, ".jar": true, ".lnk": true, ".url": true, ".desktop": true,
	".sh": true, ".bash": true, ".zsh": true, ".csh": true, ".run": true, ".bin": true,
	".app": true, ".command": true, ".workflow": true, ".dmg": true, ".pkg": true,
	".appimage": true, ".deb": true, ".rpm": true, ".apk": true, ".py": true,
	".chm": true, ".scf": true, ".msp": true, ".inf": true, ".xll": true,
	".appref-ms": true, ".settingcontent-ms": true, ".library-ms": true,
	".terminal": true, ".iso": true, ".img": true, ".vhd": true, ".vhdx": true,
	".pyw": true, ".msc": true, ".diagcab": true, ".appx": true, ".msixbundle": true,
	".appinstaller": true, ".application": true, ".jnlp": true,
}

// runsWhenOpened reports whether the system would run the file at path if it
// were opened, instead of showing it: by its type, or, with no type, by its
// being executable. Trailing dots and spaces do not hide the type, which
// Windows ignores.
func runsWhenOpened(path string) bool {
	name := strings.TrimRight(filepath.Base(path), ". ")
	ext := strings.ToLower(filepath.Ext(name))
	if ext != "" {
		return runExts[ext]
	}
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
