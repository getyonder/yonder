// Package waiting holds the connections that have not yet said what they
// want, and keeps their number down without refusing anyone: when there
// are too many, the one that has waited longest is let go for the new one.
// An honest connection speaks at once, so it is long gone from here by the
// time a flood of silent ones reaches it; a limit that refused the newest
// would let that flood shut everyone out.
package waiting

import (
	"container/list"
	"sync"
)

// Line is the connections that wait. The zero value is not usable; call New.
type Line struct {
	most int

	mu    sync.Mutex
	queue list.List // of func(), the oldest first
}

// New returns a line of at most most connections; with most at 0 or less
// there is no limit.
func New(most int) *Line { return &Line{most: most} }

// Enter puts a connection in the line. drop is called, once and with the
// line not locked, if the connection is let go for a newer one; it must end
// the connection and must not block. The connection calls leave when it has
// spoken or has ended; leave may be called more than once.
func (l *Line) Enter(drop func()) (leave func()) {
	l.mu.Lock()
	var oldest func()
	if l.most > 0 && l.queue.Len() >= l.most {
		oldest = l.queue.Remove(l.queue.Front()).(func())
	}
	e := l.queue.PushBack(drop)
	l.mu.Unlock()
	if oldest != nil {
		oldest()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			// Remove does nothing for an element that was let go.
			l.queue.Remove(e)
		})
	}
}

// Len is how many connections wait.
func (l *Line) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.queue.Len()
}
