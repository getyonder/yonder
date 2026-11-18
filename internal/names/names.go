// Package names holds the one rule for a name that another device sent
// (the name of a device, or its model) before it is kept, logged or shown.
package names

import (
	"strings"
	"unicode"
)

// Max is the most characters Clean leaves in a name.
const Max = 64

// Clean is a name as another device sent it, without control characters
// (line breaks and escape codes would forge lines in the log) or characters
// that change the direction of text, without spaces at the ends, and cut to
// Max characters. Every name taken from another device goes through it.
func Clean(name string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if Bad(r) {
			return -1
		}
		return r
	}, name))
	n := 0
	for i := range name {
		if n == Max {
			return strings.TrimSpace(name[:i])
		}
		n++
	}
	return name
}

// Bad reports a control character (NUL included) or one that changes the
// direction of text, such as U+202E, which shows "gpj.exe" as "exe.jpg".
func Bad(r rune) bool {
	return unicode.IsControl(r) || r == '؜' || r == '‎' || r == '‏' ||
		(r >= '‪' && r <= '‮') || (r >= '⁦' && r <= '⁩')
}
