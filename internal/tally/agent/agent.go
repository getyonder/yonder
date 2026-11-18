// Package agent is how an app names itself when it asks for an update: its
// version and its system, and nothing else. It is apart from the totals
// (package tally) so that the app, which sends the name, does not carry the
// server's database with it.
package agent

import (
	"regexp"
	"slices"
)

var (
	versionShape = regexp.MustCompile(`^\d{1,3}\.\d{1,3}\.\d{1,3}$`)
	agentShape   = regexp.MustCompile(`^Yonder/(\S+) \((\S+)\)$`)
	systems      = []string{"windows", "macos", "linux"}
)

// KnownVersion says whether a version has the one shape that is kept: three
// numbers with points between them.
func KnownVersion(version string) bool { return versionShape.MatchString(version) }

// KnownSystem says whether a system is one of those that are kept.
func KnownSystem(system string) bool { return slices.Contains(systems, system) }

// Name is the User-Agent an app asks for an update with: its version and
// its system, as Go names it (GOOS), and nothing else. A version that is
// not three numbers, a development build's say, is sent as "dev", so the
// text keeps its shape and the server keeps the system alone.
func Name(version, goos string) string {
	if !KnownVersion(version) {
		version = "dev"
	}
	if goos == "darwin" {
		goos = "macos"
	}
	return "Yonder/" + version + " (" + goos + ")"
}

// Parse takes the version and the system from the User-Agent an app asks
// for an update with, "Yonder/0.1.27 (linux)". Anything else gives two
// empty strings.
func Parse(agent string) (version, system string) {
	m := agentShape.FindStringSubmatch(agent)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}
