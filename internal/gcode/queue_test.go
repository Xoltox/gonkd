package gcode

import "testing"

func TestWindowCapacity(t *testing.T) {
	w := NewWindow(2)
	if !w.CanSend() {
		t.Fatal("expected room to send")
	}
	w.Prepare("G1 X1")
	if !w.CanSend() {
		t.Fatal("expected room for second line")
	}
	w.Prepare("G1 X2")
	if w.CanSend() {
		t.Fatal("expected window full at capacity 2")
	}
}

func TestWindowAckDrainsInOrder(t *testing.T) {
	w := NewWindow(16)
	n1, _ := w.Prepare("G1 X1")
	n2, _ := w.Prepare("G1 X2")
	n3, _ := w.Prepare("G1 X3")
	_ = n1
	if w.Outstanding() != 3 {
		t.Fatalf("outstanding = %d, want 3", w.Outstanding())
	}
	w.Ack(Response{Kind: KindOK, OKLine: n2, HasAdv: true, Planner: 60, Buffer: 10})
	if w.Outstanding() != 1 {
		t.Fatalf("outstanding after ack(n2) = %d, want 1 (only n3 left)", w.Outstanding())
	}
	if w.LastBuffer() != 10 {
		t.Fatalf("LastBuffer = %d, want 10", w.LastBuffer())
	}
	w.Ack(Response{Kind: KindOK, OKLine: n3})
	if w.Outstanding() != 0 {
		t.Fatalf("outstanding after ack(n3) = %d, want 0", w.Outstanding())
	}
}

func TestWindowPlainOKDrainsOldest(t *testing.T) {
	w := NewWindow(16)
	w.Prepare("G1 X1")
	w.Prepare("G1 X2")
	w.Ack(Response{Kind: KindOK})
	if w.Outstanding() != 1 {
		t.Fatalf("outstanding = %d, want 1", w.Outstanding())
	}
}

func TestWindowResendReplaysFromN(t *testing.T) {
	w := NewWindow(16)
	w.Prepare("G1 X1") // N1
	w.Prepare("G1 X2") // N2
	w.Prepare("G1 X3") // N3

	resend := w.Resend(2)
	if len(resend) != 2 {
		t.Fatalf("resend len = %d, want 2", len(resend))
	}
	if resend[0].N != 2 || resend[1].N != 3 {
		t.Fatalf("resend = %+v", resend)
	}
	// Resend must not remove them from outstanding -- they're still
	// unacknowledged until Marlin sends "ok" for them.
	if w.Outstanding() != 3 {
		t.Fatalf("outstanding after resend = %d, want 3", w.Outstanding())
	}
}

func TestWindowResetClearsState(t *testing.T) {
	w := NewWindow(16)
	w.Prepare("G1 X1")
	w.Reset(1)
	if w.Outstanding() != 0 {
		t.Fatalf("outstanding after reset = %d, want 0", w.Outstanding())
	}
	n, framed := w.Prepare("M110 N0")
	if n != 1 {
		t.Fatalf("n after reset = %d, want 1", n)
	}
	if framed != "N1 M110 N0*"+itoa(int(Checksum("N1 M110 N0")))+"\n" {
		t.Fatalf("framed = %q", framed)
	}
}
