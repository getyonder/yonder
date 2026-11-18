package firewall

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

// Manual is whether Fix only opens the system's settings, leaving the
// change to the person.
const Manual = false

// readScript prints "off", "open", "closed" or "blocked" for the program at
// $exe listening on $port. Anyone may read the firewall's settings; this
// goes through its COM interface because netsh's output is in the system's
// language. A block rule beats an allow rule, and with neither Windows
// drops what comes in.
const readScript = `
$ErrorActionPreference = 'Stop'
$fw = New-Object -ComObject HNetCfg.FwPolicy2
$cur = $fw.CurrentProfileTypes
$on = $false
foreach ($p in 1, 2, 4) { if (($cur -band $p) -and $fw.FirewallEnabled($p)) { $on = $true } }
if (-not $on) { 'off'; exit }
$allow = $false; $block = $false
foreach ($r in $fw.Rules) {
  if (-not $r.Enabled -or $r.Direction -ne 1 -or -not ($r.Profiles -band $cur)) { continue }
  if ($r.ApplicationName) {
    if ([Environment]::ExpandEnvironmentVariables($r.ApplicationName) -ine $exe) { continue }
  } elseif ($r.Protocol -ne 6 -or (($r.LocalPorts -split ',') -notcontains "$port")) { continue }
  if ($r.Action -eq 0) { $block = $true } else { $allow = $true }
}
if ($block) { 'blocked' } elseif ($allow) { 'open' } else { 'closed' }
`

// fixScript runs as administrator: it removes every incoming rule for the
// program (the block rules Windows makes when its own question is answered
// "Cancel", and stale allow rules) and allows it on every kind of network.
const fixScript = `
$ErrorActionPreference = 'Stop'
Get-NetFirewallApplicationFilter -Program $exe -ErrorAction SilentlyContinue | Get-NetFirewallRule | Where-Object { $_.Direction -eq 'Inbound' } | Remove-NetFirewallRule
New-NetFirewallRule -DisplayName 'Yonder' -Direction Inbound -Action Allow -Program $exe -Profile Any | Out-Null
`

// Check reads Windows Firewall's state and its rules for this program.
// Another firewall (a security suite's) cannot be read.
func Check(port int) Status {
	out, err := powershell(30*time.Second, vars(port)+readScript)
	if err != nil {
		return Status{}
	}
	switch strings.TrimSpace(out) {
	case "open":
		return Status{On: true}
	case "closed", "blocked":
		return Status{On: true, Blocking: true}
	}
	return Status{}
}

// Fix adds the rule from an administrator's PowerShell, which Windows
// starts after its own prompt (UAC); a standard user is asked for an
// administrator's password there.
func Fix(port int) error {
	// Backing out of the prompt makes Start-Process fail.
	outer := fmt.Sprintf(`
try {
  $p = Start-Process powershell -Verb RunAs -Wait -PassThru -WindowStyle Hidden -ArgumentList '-NoProfile', '-NonInteractive', '-EncodedCommand', '%s'
  exit $p.ExitCode
} catch { exit 77 }
`, encode(vars(port)+fixScript))
	_, err := powershell(5*time.Minute, outer)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit) && exit.ExitCode() == 77:
		return ErrCancelled
	}
	return errors.New("Windows Firewall didn't take the change. In Windows Security, open Firewall & network protection, then Allow an app through firewall, and add Yonder.")
}

// OpenLocalNetwork is for the Mac only.
func OpenLocalNetwork() error { return errors.New("This system has no Local Network setting.") }

// vars sets what both scripts work on.
func vars(port int) string {
	exe, _ := os.Executable()
	return fmt.Sprintf("$exe = '%s'\n$port = %d\n", strings.ReplaceAll(exe, "'", "''"), port)
}

// encode is PowerShell's -EncodedCommand form, which needs no quoting.
func encode(script string) string {
	units := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[2*i:], u)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// powershell runs a script with no window and returns what it printed.
func powershell(limit time.Duration, script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-EncodedCommand", encode(script))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.Output()
	return string(out), err
}
