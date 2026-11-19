package direct

import (
	"context"
	"testing"
)

// In a browser the answering end finds the other end, which is in the same
// page, before a STUN server that never answers is given up: the browser
// then stops asking, and the description has no address from STUN. The
// Peer has a path, so the fallback is not asked and the connection forms
// (0.1.37 to 0.1.41 made a second Peer here, and none formed).
func TestTheFallbackIsNotAskedWhenAPathIsThere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	// A port that nothing answers on: the discard port of this machine.
	silent := "stun:127.0.0.1:9"
	cfg := Config{STUN: []string{silent}, Fallback: []string{silent}}

	a, offer, err := Offer(ctx, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, answer, err := Answer(ctx, cfg, offer)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if fromSTUN(answer) {
		t.Fatal("the answer has an address from STUN, so this test shows nothing")
	}
	if b.AskedFallback() {
		t.Error("the answering Peer asked the fallback although it had a path")
	}
	if err := a.SetAnswer(answer); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Peer{a, b} {
		if err := p.Connected(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
