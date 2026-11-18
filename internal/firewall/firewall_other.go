//go:build !linux && !windows && !darwin

package firewall

import "errors"

// Manual is whether Fix only opens the system's settings, leaving the
// change to the person.
const Manual = false

// Check reads nothing here: a browser has no firewall of its own to read.
func Check(port int) Status { return Status{} }

// Fix cannot change anything here.
func Fix(port int) error { return errors.New("Yonder can't change this system's firewall.") }

// OpenLocalNetwork is for the Mac only.
func OpenLocalNetwork() error { return errors.New("This system has no Local Network setting.") }
