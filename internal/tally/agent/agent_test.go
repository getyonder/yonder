package agent

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for agent, want := range map[string][2]string{
		"Yonder/0.1.27 (linux)":       {"0.1.27", "linux"},
		"Yonder/10.20.30 (windows)":   {"10.20.30", "windows"},
		"Go-http-client/1.1":          {},
		"":                            {},
		"Yonder/0.1.27":               {},
		"Yonder/0.1.27 (linux) extra": {},
		"Mozilla/5.0 (X11; Linux)":    {},
	} {
		if v, sys := Parse(agent); v != want[0] || sys != want[1] {
			t.Errorf("Parse(%q) = %q, %q; want %q, %q", agent, v, sys, want[0], want[1])
		}
	}
}

// The app and the web version are built without the server's database: this
// package is all of the totals that they carry.
func TestAppsHaveNoDatabase(t *testing.T) {
	for _, c := range []struct {
		pkg string
		env []string
	}{
		{"yonder/cmd/yonder", nil},
		{"yonder/cmd/yonder-web", []string{"GOOS=js", "GOARCH=wasm"}},
	} {
		cmd := exec.Command("go", "list", "-deps", c.pkg)
		cmd.Env = append(os.Environ(), c.env...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list %s: %v", c.pkg, err)
		}
		for _, dep := range strings.Fields(string(out)) {
			if dep == "database/sql" || dep == "yonder/internal/tally" || strings.HasPrefix(dep, "modernc.org/") {
				t.Errorf("%s is built with %s", c.pkg, dep)
			}
		}
	}
}
