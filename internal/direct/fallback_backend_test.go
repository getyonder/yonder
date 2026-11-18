package direct

import (
	"context"
	"net"
	"testing"

	"yonder/internal/stun"
)

// stunServer starts Yonder's STUN server on a loopback port, and returns it
// with its address as a Config wants it.
func stunServer(t *testing.T) (*stun.Server, string) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s := stun.NewServer()
	done := make(chan struct{})
	go func() { s.Serve(conn); close(done) }()
	t.Cleanup(func() { conn.Close(); <-done })
	return s, "stun:" + conn.LocalAddr().String()
}

// silent is the address of a STUN server that never answers: a port that
// is open and that nothing reads.
func silent(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return "stun:" + conn.LocalAddr().String()
}

// The first STUN server answers: the fallback is never asked.
func TestTheFallbackIsNotAskedWhenSTUNAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	first, firstAddr := stunServer(t)
	second, secondAddr := stunServer(t)
	cfg := Config{STUN: []string{firstAddr}, Fallback: []string{secondAddr}, Loopback: true}

	a, offer, err := Offer(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, answer, err := Answer(ctx, cfg, offer)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !fromSTUN(offer) || !fromSTUN(answer) {
		t.Fatal("a description has no address from STUN")
	}
	if a.AskedFallback() || b.AskedFallback() {
		t.Error("a Peer says that it asked the fallback")
	}
	if answered, _ := first.Counts(); answered == 0 {
		t.Error("the first server answered nothing")
	}
	if answered, refused := second.Counts(); answered+refused != 0 {
		t.Errorf("the fallback got %d packets", answered+refused)
	}
}

// The first STUN server gives no answer: both ends ask the fallback, and
// the two still connect.
func TestTheFallbackIsAskedWhenSTUNIsSilent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	second, secondAddr := stunServer(t)
	cfg := Config{STUN: []string{silent(t)}, Fallback: []string{secondAddr}, Loopback: true}

	a, offer, err := Offer(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, answer, err := Answer(ctx, cfg, offer)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !fromSTUN(offer) || !fromSTUN(answer) {
		t.Fatal("a description has no address from STUN")
	}
	if !a.AskedFallback() || !b.AskedFallback() {
		t.Error("a Peer does not say that it asked the fallback")
	}
	if answered, _ := second.Counts(); answered < 2 {
		t.Errorf("the fallback answered %d requests, want one from each end", answered)
	}
	if err := a.SetAnswer(answer); err != nil {
		t.Fatal(err)
	}
	if err := a.Connected(ctx); err != nil {
		t.Fatal(err)
	}
}

// Neither server answers: the description goes with the device's own
// addresses, as with no fallback.
func TestASilentFallbackLeavesTheFirstDescription(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	cfg := Config{STUN: []string{silent(t)}, Fallback: []string{silent(t)}, Loopback: true}
	a, offer, err := Offer(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if fromSTUN(offer) {
		t.Error("an address from STUN with no STUN server")
	}
	b, answer, err := Answer(ctx, local, offer)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := a.SetAnswer(answer); err != nil {
		t.Fatal(err)
	}
	if err := a.Connected(ctx); err != nil {
		t.Fatal(err)
	}
}
