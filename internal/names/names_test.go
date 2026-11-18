package names

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClean(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"a plain name stays", "Anna's laptop", "Anna's laptop"},
		{"spaces at the ends go", "  desk \t", "desk"},
		{"control characters go", "a\x00b\nc\x1b[31md", "abc[31md"},
		{"direction marks go", "gpj‮.exe⁦‏؜", "gpj.exe"},
		{"nothing but marks is no name", "‮ ‎", ""},
		{"a long name is cut", strings.Repeat("é", 1<<20), strings.Repeat("é", Max)},
		{"a cut does not leave a space at the end", strings.Repeat("a", Max-1) + "  b", strings.Repeat("a", Max-1)},
	} {
		got := Clean(tc.in)
		if got != tc.want {
			t.Errorf("%s: got %q (%d characters), want %q", tc.name, Clip(got), utf8.RuneCountInString(got), Clip(tc.want))
		}
		if again := Clean(got); again != got {
			t.Errorf("%s: cleaned twice it changes again", tc.name)
		}
	}
}

// Clip keeps a failure's line short.
func Clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
