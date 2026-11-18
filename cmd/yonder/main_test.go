package main

import "testing"

func TestPlainRemovesWhatActsOnTheTerminal(t *testing.T) {
	for in, want := range map[string]string{
		"notes.txt":          "notes.txt",
		"Mum's laptop":       "Mum's laptop",
		"Zo\u00eb \u2013 PC": "Zo\u00eb \u2013 PC",
		"a\x1b[2Jb":          "a[2Jb",
		"ok\rfake":           "okfake",
		"x\a\x00y\x7f":       "xy",
		"gpj.\u202Eexe":      "gpj.exe",
	} {
		if got := plain(in); got != want {
			t.Errorf("plain(%q) = %q, want %q", in, got, want)
		}
	}
}
