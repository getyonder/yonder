package settings

import (
	"strings"
	"testing"
)

func TestFirstRunPicksANameAndKeepsIt(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(first.Name)) != 2 || first.SaveDir == "" {
		t.Fatalf("first run gave %+v", first)
	}
	again, err := Load(dir)
	if err != nil || again.Name != first.Name {
		t.Fatalf("second load gave %q, err %v; want %q", again.Name, err, first.Name)
	}
}

func TestLabels(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	s.SetLabel("AB", "Mum's laptop")
	s.SetLabel("CD", "Office")
	s.SetLabel("CD", "")
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	s, _ = Load(dir)
	if len(s.Labels) != 1 || s.Labels["AB"] != "Mum's laptop" {
		t.Fatalf("labels = %v", s.Labels)
	}
}

func TestAlwaysAccept(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	s.SetAlways("AB", true)
	s.SetAlways("AB", true)
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	s, _ = Load(dir)
	if !s.Always("AB") || len(s.AlwaysAccept) != 1 {
		t.Fatalf("always accept = %v", s.AlwaysAccept)
	}
	s.SetAlways("AB", false)
	if s.Always("AB") {
		t.Fatal("still always accepting after turning it off")
	}
}
