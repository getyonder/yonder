package localsend

import (
	"context"
	"errors"
	"io"
)

// Progress is how far a transfer has got, across all its files.
type Progress struct {
	// File is the name of the file on the move, empty before the first.
	File string
	// Done and Total count bytes in the files the receiver took.
	Done, Total int64
}

type progressKey struct{}

// WithProgress returns a context that makes Sender.Send and Sender.Lend
// report to f: once when the receiver accepts, then as bytes go out. f is
// called often, by whatever is moving the bytes, and must not block.
func WithProgress(ctx context.Context, f func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, f)
}

var errStopped = errors.New("the transfer was cancelled")

// meter counts the bytes read through it and can be told to stop.
type meter struct {
	r     io.Reader
	n     int64
	count func(n int)
	stop  func() bool
}

func (m *meter) Read(b []byte) (int, error) {
	if m.stop != nil && m.stop() {
		return 0, errStopped
	}
	n, err := m.r.Read(b)
	m.n += int64(n)
	m.count(n)
	return n, err
}
