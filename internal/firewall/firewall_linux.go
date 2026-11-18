package firewall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Manual is whether Fix only opens the system's settings, leaving the
// change to the person.
const Manual = false

// Where ufw keeps what it is set to; tests point these elsewhere.
var (
	ufwConf    = "/etc/ufw/ufw.conf"
	ufwDefault = "/etc/default/ufw"
	ufwRules   = "/etc/ufw/user.rules"
)

// Check reads ufw's files or asks firewalld; neither needs a password.
// Other firewalls cannot be read and come back as off.
func Check(port int) Status {
	if on, missing, err := ufw(port); on {
		return Status{On: true, Blocking: err == nil && len(missing) > 0 && missing[0] == needed(port)[0]}
	}
	if on, missing, err := firewalld(port); on {
		return Status{On: true, Blocking: err == nil && len(missing) > 0 && missing[0] == needed(port)[0]}
	}
	return Status{}
}

// Fix opens what Yonder needs, after the desktop's own password prompt.
func Fix(port int) error {
	var cmds []string
	if on, missing, err := ufw(port); on {
		if err != nil {
			// The rules can't be read (Debian, Ubuntu); allowing a port
			// twice does no harm.
			missing = needed(port)
		}
		for _, p := range missing {
			cmds = append(cmds, "ufw allow "+p)
		}
	} else if on, missing, err := firewalld(port); on {
		if err != nil {
			missing = needed(port)
		}
		for _, p := range missing {
			cmds = append(cmds, "firewall-cmd -q --permanent --add-port="+p)
		}
		if len(cmds) > 0 {
			cmds = append(cmds, "firewall-cmd -q --reload")
		}
	} else {
		return errors.New("Yonder doesn't know this computer's firewall, so it can't change it.")
	}
	if len(cmds) == 0 {
		return nil
	}
	script := strings.Join(cmds, " && ")
	byHand := "In a terminal, run: sudo sh -c '" + script + "'"
	if _, err := exec.LookPath("pkexec"); err != nil {
		return errors.New("This desktop has no password prompt. " + byHand)
	}
	// One prompt for every change.
	out, err := exec.Command("pkexec", "sh", "-c", script).CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit) && exit.ExitCode() == 126:
		// pkexec: the prompt was dismissed.
		return ErrCancelled
	case errors.As(err, &exit) && exit.ExitCode() == 127:
		// pkexec: nothing in this session can ask for the password, or
		// the password was wrong.
		return errors.New("The password prompt didn't work. " + byHand)
	}
	return fmt.Errorf("The firewall didn't take the change: %s. %s", strings.TrimSpace(string(out)), byHand)
}

// OpenLocalNetwork is for the Mac only.
func OpenLocalNetwork() error { return errors.New("This system has no Local Network setting.") }

// ufw reports whether ufw is on with incoming connections refused by
// default, and which of the needed ports it does not let in. err means the
// rules could not be read, as on systems that keep them for root.
func ufw(port int) (on bool, missing []string, err error) {
	if setting(ufwConf, "ENABLED") != "yes" {
		return false, nil, nil
	}
	if policy := setting(ufwDefault, "DEFAULT_INPUT_POLICY"); strings.EqualFold(policy, "ACCEPT") {
		return false, nil, nil
	}
	rules, err := os.ReadFile(ufwRules)
	if err != nil {
		return true, nil, err
	}
	for _, p := range needed(port) {
		if !ufwAllows(string(rules), p) {
			missing = append(missing, p)
		}
	}
	return true, missing, nil
}

// setting reads NAME=value from a shell-style settings file, without its
// quotes; empty if the file or the name is not there.
func setting(path, name string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+"="); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// ufwAllows reports whether ufw's saved rules let in what ("53317/tcp").
// Each rule is recorded in a comment:
//
//	### tuple ### allow tcp 53317 0.0.0.0/0 any 0.0.0.0/0 in
//
// A rule for some addresses only counts: it is most likely the local
// network.
func ufwAllows(rules, what string) bool {
	num, proto, _ := strings.Cut(what, "/")
	port, _ := strconv.Atoi(num)
	for _, line := range strings.Split(rules, "\n") {
		rest, ok := strings.CutPrefix(line, "### tuple ### ")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 7 || !(strings.HasPrefix(f[0], "allow") || strings.HasPrefix(f[0], "limit")) || !strings.HasPrefix(f[6], "in") {
			continue
		}
		if (f[1] == proto || f[1] == "any") && portIn(f[2], port) {
			return true
		}
	}
	return false
}

// portIn reports whether port is in ufw's "any", "80", "80,443" or
// "6000:6007", or a mix of those.
func portIn(spec string, port int) bool {
	if spec == "any" {
		return true
	}
	for _, part := range strings.Split(spec, ",") {
		from, to, ranged := strings.Cut(part, ":")
		a, err := strconv.Atoi(from)
		if err != nil {
			continue
		}
		b := a
		if ranged {
			if b, err = strconv.Atoi(to); err != nil {
				continue
			}
		}
		if a <= port && port <= b {
			return true
		}
	}
	return false
}

// firewalld is ufw's counterpart for firewalld, which answers questions
// from anyone in the desktop session.
func firewalld(port int) (on bool, missing []string, err error) {
	if _, lookErr := exec.LookPath("firewall-cmd"); lookErr != nil {
		return false, nil, nil
	}
	if code, runErr := firewallCmd("--state"); runErr != nil || code != 0 {
		return false, nil, nil
	}
	for _, p := range needed(port) {
		code, runErr := firewallCmd("--query-port=" + p)
		switch {
		case runErr != nil || code > 1:
			return true, nil, errors.New("firewalld did not say which ports are open")
		case code == 1:
			missing = append(missing, p)
		}
	}
	return true, missing, nil
}

// firewallCmd runs firewall-cmd and returns its exit code.
func firewallCmd(arg string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := exec.CommandContext(ctx, "firewall-cmd", arg).Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 0, err
}
