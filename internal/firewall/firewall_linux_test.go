package firewall

import (
	"os"
	"path/filepath"
	"testing"
)

const rules = `*filter
### RULES ###

### tuple ### allow udp 53317 0.0.0.0/0 any 0.0.0.0/0 in
-A ufw-user-input -p udp --dport 53317 -j ACCEPT

### tuple ### allow tcp 53317 0.0.0.0/0 any 0.0.0.0/0 in
-A ufw-user-input -p tcp --dport 53317 -j ACCEPT

### tuple ### allow tcp 6000:6007 0.0.0.0/0 any 192.168.0.0/16 in
### tuple ### allow any 80,443 0.0.0.0/0 any 0.0.0.0/0 in
### tuple ### deny tcp 9000 0.0.0.0/0 any 0.0.0.0/0 in
### tuple ### allow tcp 9001 0.0.0.0/0 any 0.0.0.0/0 out
### END RULES ###
`

func TestUfwAllows(t *testing.T) {
	for what, want := range map[string]bool{
		"53317/tcp": true, "53317/udp": true, "53318/tcp": false,
		"6003/tcp": true, "6003/udp": false, "6008/tcp": false,
		"443/udp": true, "9000/tcp": false, "9001/tcp": false,
	} {
		if got := ufwAllows(rules, what); got != want {
			t.Errorf("ufwAllows(%s) = %v, want %v", what, got, want)
		}
	}
}

// ufwFiles points the package at a ufw set up in a folder of the test's.
// Tests using it must not run in parallel: the paths are the package's.
func ufwFiles(t *testing.T, conf, policy string, rules *string) {
	t.Helper()
	dir := t.TempDir()
	oldConf, oldDefault, oldRules := ufwConf, ufwDefault, ufwRules
	t.Cleanup(func() { ufwConf, ufwDefault, ufwRules = oldConf, oldDefault, oldRules })
	ufwConf, ufwDefault, ufwRules = filepath.Join(dir, "ufw.conf"), filepath.Join(dir, "default"), filepath.Join(dir, "user.rules")
	write := func(path, text string) {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(ufwConf, conf)
	write(ufwDefault, policy)
	if rules != nil {
		write(ufwRules, *rules)
	}
}

func TestUfw(t *testing.T) {
	r := rules
	for _, c := range []struct {
		name, conf, policy string
		rules              *string
		port               int
		on, unreadable     bool
		missing            int
	}{
		{"off", "ENABLED=no\n", `DEFAULT_INPUT_POLICY="DROP"`, &r, 53318, false, false, 0},
		{"on, lets everything in", "ENABLED=yes\n", `DEFAULT_INPUT_POLICY="ACCEPT"`, &r, 53318, false, false, 0},
		{"on, port open", "# comment\nENABLED=yes\n", `DEFAULT_INPUT_POLICY="DROP"`, &r, 53317, true, false, 0},
		{"on, port closed", "ENABLED=yes\n", `DEFAULT_INPUT_POLICY="DROP"`, &r, 53318, true, false, 1},
		{"on, rules kept for root", "ENABLED=yes\n", `DEFAULT_INPUT_POLICY="DROP"`, nil, 53317, true, true, 0},
	} {
		ufwFiles(t, c.conf, c.policy, c.rules)
		on, missing, err := ufw(c.port)
		if on != c.on || (err != nil) != c.unreadable || len(missing) != c.missing {
			t.Errorf("%s: on %v, missing %v, err %v", c.name, on, missing, err)
		}
	}
}
