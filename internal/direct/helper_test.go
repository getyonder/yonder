package direct

import (
	"context"
	"crypto/sha256"
	"io"
	"math/rand/v2"
	"net"
	"testing"
	"time"
)

// patience is the one limit of every wait in these tests. It is long, so
// that a loaded machine does not fail a test; a wait that works returns
// far sooner.
const patience = 60 * time.Second

// within runs f and gives its result, or fails the test when f has not
// returned in time. Every wait of these tests goes through it.
func within[T any](t *testing.T, what string, f func() T) T {
	t.Helper()
	ch := make(chan T, 1)
	go func() { ch <- f() }()
	select {
	case v := <-ch:
		return v
	case <-time.After(patience):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// until waits for cond to hold.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	within(t, what, func() bool {
		for !cond() {
			time.Sleep(2 * time.Millisecond)
		}
		return true
	})
}

// result is what a call that returns (n, error) gave.
type result struct {
	n   int
	err error
}

var local = Config{Loopback: true}

// connect gives two Peers, connected to each other over loopback.
func connect(t *testing.T) (a, b *Peer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	t.Cleanup(cancel)
	a, offer, err := Offer(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	b, answer, err := Answer(ctx, local, offer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	if err := a.SetAnswer(answer); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Peer{a, b} {
		if err := p.Connected(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

// pipe gives the two ends of one connection: a's from Dial, b's from
// Accept.
func pipe(t *testing.T, a, b *Peer) (ca, cb *conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	type accepted struct {
		c   net.Conn
		err error
	}
	got := make(chan accepted, 1)
	go func() {
		c, err := b.Accept()
		got <- accepted{c, err}
	}()
	c, err := a.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	acc := within(t, "Accept", func() accepted { return <-got })
	if acc.err != nil {
		t.Fatal(acc.err)
	}
	t.Cleanup(func() { c.Close(); acc.c.Close() })
	return c.(*conn), acc.c.(*conn)
}

// noise gives n bytes that differ for each seed.
func noise(seed uint64, n int) []byte {
	b := make([]byte, n)
	r := rand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8)})
	r.Read(b)
	return b
}

// readSum reads n bytes and gives their SHA-256.
func readSum(r io.Reader, n int) ([32]byte, error) {
	h := sha256.New()
	_, err := io.CopyN(h, r, int64(n))
	return [32]byte(h.Sum(nil)), err
}
