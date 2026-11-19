package waiting

import (
	"slices"
	"testing"
)

func TestTheOldestIsLetGo(t *testing.T) {
	l := New(2)
	var dropped []int
	enter := func(n int) func() { return l.Enter(func() { dropped = append(dropped, n) }) }
	leave1 := enter(1)
	leave2 := enter(2)
	if len(dropped) != 0 {
		t.Fatalf("dropped %v with the line not over its limit", dropped)
	}
	enter(3)
	enter(4)
	if !slices.Equal(dropped, []int{1, 2}) {
		t.Fatalf("dropped %v, want the two oldest in turn", dropped)
	}
	// One that was let go leaves afterwards, as its connection ends.
	leave1()
	leave2()
	leave2()
	if l.Len() != 2 {
		t.Fatalf("%d wait, want 2", l.Len())
	}
}

func TestOneThatHasLeftIsNotDropped(t *testing.T) {
	l := New(2)
	dropped := 0
	leave := l.Enter(func() { dropped++ })
	leave()
	for range 2 {
		l.Enter(func() { t.Error("dropped with the line not over its limit") })
	}
	if dropped != 0 || l.Len() != 2 {
		t.Fatalf("dropped %d and %d wait, want 0 and 2", dropped, l.Len())
	}
}

func TestNoLimit(t *testing.T) {
	l := New(0)
	for range 100 {
		l.Enter(func() { t.Error("dropped from a line with no limit") })
	}
	if l.Len() != 100 {
		t.Fatalf("%d wait, want 100", l.Len())
	}
}
