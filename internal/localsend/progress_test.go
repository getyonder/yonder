package localsend

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

func TestProgressOnBothSides(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var got []Progress
	var finished []bool
	var verified string
	h.rc.Accept = func(ctx context.Context, o Offer) []string {
		verified = o.CertFingerprint
		return acceptAll(ctx, o)
	}
	h.rc.Progress = func(p Progress) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	}
	h.rc.Finished = func(complete bool) { finished = append(finished, complete) }

	one, two := []byte("first"), bytes.Repeat([]byte("y"), 1<<20)
	total := int64(len(one) + len(two))
	paths := []string{writeFile(t, "one.txt", one), writeFile(t, "two.bin", two)}
	var sent []Progress
	ctx := WithProgress(context.Background(), func(p Progress) { sent = append(sent, p) })
	s := newSender(t)
	if n, err := s.Send(ctx, h.peer(), paths); err != nil || n != 2 {
		t.Fatalf("sent %d, err %v", n, err)
	}

	if first, last := sent[0], sent[len(sent)-1]; first != (Progress{Total: total}) || last != (Progress{File: "two.bin", Done: total, Total: total}) {
		t.Fatalf("sender reported %+v first and %+v last", first, last)
	}
	mu.Lock()
	defer mu.Unlock()
	if first, last := got[0], got[len(got)-1]; first != (Progress{Total: total}) || last.Done != total || last.Total != total {
		t.Fatalf("receiver reported %+v first and %+v last", first, last)
	}
	if len(finished) != 1 || !finished[0] {
		t.Fatalf("finished = %v, want one complete transfer", finished)
	}
	if want := s.Info.Fingerprint; verified != want {
		t.Fatalf("offer carried certificate fingerprint %q, want the sender's %q", verified, want)
	}
}

func TestReceiverCancel(t *testing.T) {
	h := newHarness(t)
	var finished []bool
	h.rc.Finished = func(complete bool) { finished = append(finished, complete) }
	data := []byte("never arrives")
	status, session := h.prepare(meta("a", "a.txt", data))
	if status != 200 {
		t.Fatalf("prepare: HTTP %d", status)
	}
	h.rc.Cancel()
	if len(finished) != 1 || finished[0] {
		t.Fatalf("finished = %v, want one incomplete transfer", finished)
	}
	if status := h.upload(session.SessionID, "a", session.Files["a"], data); status != 403 {
		t.Fatalf("upload after cancel: HTTP %d, want 403", status)
	}
}
