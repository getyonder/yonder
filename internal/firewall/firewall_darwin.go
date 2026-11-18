package firewall

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Manual is whether Fix only opens the system's settings, leaving the
// change to the person. On the Mac a program cannot ask for the
// administrator's say on this.
const Manual = true

const socketfilterfw = "/usr/libexec/ApplicationFirewall/socketfilterfw"

// Pages of System Settings. Neither address is documented by Apple; if one
// stops working, System Settings still opens, and the notice names the
// page in words.
const (
	firewallPage     = "x-apple.systempreferences:com.apple.Network-Settings.extension?Firewall"
	localNetworkPage = "x-apple.systempreferences:com.apple.settings.PrivacySecurity.extension?Privacy_LocalNetwork"
)

// Check asks the Mac's firewall whether it is on and whether it blocks
// this program. The Local Network permission is a separate thing that
// cannot be read at all.
func Check(port int) Status {
	global := ask("--getglobalstate")
	switch {
	case strings.Contains(global, "State = 2"):
		// Set to block all incoming connections.
		return Status{On: true, Blocking: true}
	case !strings.Contains(global, "State = 1") && !strings.Contains(global, "is enabled"):
		return Status{}
	}
	// The firewall may know the program by its file or by the .app.
	paths := []string{}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, exe)
		if app, _, inApp := strings.Cut(exe, ".app/"); inApp {
			paths = append(paths, app+".app")
		}
	}
	for _, p := range paths {
		if out := strings.ToLower(ask("--getappblocked", p)); strings.Contains(out, "is blocked") {
			return Status{On: true, Blocking: true}
		}
	}
	return Status{On: true}
}

// Fix opens the firewall's page of System Settings.
func Fix(port int) error { return exec.Command("open", firewallPage).Run() }

// OpenLocalNetwork opens the page where Yonder is allowed onto the local
// network.
func OpenLocalNetwork() error { return exec.Command("open", localNetworkPage).Run() }

// ask runs socketfilterfw and returns what it printed, or nothing.
func ask(args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, socketfilterfw, args...).Output()
	return string(out)
}
