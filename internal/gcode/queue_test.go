package gcode

import (
	"strconv"
	"testing"
)

func okN(n int64) Response { return ParseLine("ok N" + strconv.FormatInt(n, 10) + " P30 B15") }

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
	w.Prepare("G1 X1")
	n2, _ := w.Prepare("G1 X2")
	n3, _ := w.Prepare("G1 X3")
	w.Ack(okN(n2))
	if w.Outstanding() != 1 {
		t.Fatalf("outstanding after ack(n2) = %d, want 1 (only n3 left)", w.Outstanding())
	}
	w.Ack(okN(n3))
	if w.Outstanding() != 0 {
		t.Fatalf("outstanding after ack(n3) = %d, want 0", w.Outstanding())
	}
}

func TestWindowAckN0DropsNothing(t *testing.T) {
	w := NewWindow(16)
	w.Prepare("G1 X1") // N1
	w.Ack(okN(0))      // the handshake's "N0 M110 N0" ack
	if w.Outstanding() != 1 {
		t.Fatalf("ok N0 must not ack N1, outstanding = %d", w.Outstanding())
	}
}

func TestWindowPlainOKDrainsOldestWithoutAdvancedOK(t *testing.T) {
	w := NewWindow(16)
	w.Prepare("G1 X1")
	w.Prepare("G1 X2")
	w.Ack(ParseLine("ok"))
	if w.Outstanding() != 1 {
		t.Fatalf("outstanding = %d, want 1", w.Outstanding())
	}
}

func TestWindowBareOKIgnoredOnceNumbered(t *testing.T) {
	w := NewWindow(16)
	w.Prepare("G1 X1") // N1
	w.Ack(okN(1))
	w.Prepare("G1 X2") // N2
	w.Ack(ParseLine("ok P30 B15"))
	if w.Outstanding() != 1 {
		t.Fatalf("bare ok popped a numbered line, outstanding = %d", w.Outstanding())
	}
	// M105 prints its own "ok T:..." with no N: it acks that M105 and
	// everything before it, never a line without an M105 behind it.
	w.Ack(ParseLine("ok T:20.00 /0.00 B:20.00 /0.00 @:0 B@:0"))
	if w.Outstanding() != 1 {
		t.Fatalf("ok T: without an outstanding M105 acked a line, outstanding = %d", w.Outstanding())
	}
	w.Prepare("M105") // N3
	w.Prepare("G1 X3")
	w.Ack(ParseLine("ok T:20.00 /0.00 B:20.00 /0.00 @:0 B@:0"))
	if w.Outstanding() != 1 {
		t.Fatalf("ok T: should ack through the M105, outstanding = %d", w.Outstanding())
	}
}

// R-02: the M29's "ok N" is lost; the drain probe's M105 reply must drain
// the window instead of leaving the M105 itself outstanding forever.
func TestWindowM105ProbeDrainsLostOK(t *testing.T) {
	w := NewWindow(16)
	n, _ := w.Prepare("G1 X1")
	w.Ack(okN(n))    // seenN
	w.Prepare("M29") // its ok is lost
	w.Prepare("M105")
	w.Ack(ParseLine("ok T:25.0 /0.0 B:25.0 /0.0 @:0 B@:0"))
	if w.Outstanding() != 0 {
		t.Fatalf("window never drains: outstanding=%d", w.Outstanding())
	}
}

// R-11: an M110 blocks further lines until its ok; a stale resend holds
// the outstanding lines behind the M110 and releases them on its ack.
func TestWindowM110Barrier(t *testing.T) {
	w := NewWindow(16)
	w.Reset(0)
	if n, f := w.PrepareBarrier("M110 N0"); n != 0 || f != FrameLine(0, "M110 N0") {
		t.Fatalf("barrier n=%d framed=%q", n, f)
	}
	if w.CanSend() {
		t.Fatal("CanSend during M110 barrier")
	}
	if l, _, _ := w.Resend(1); len(l) != 1 || l[0].Cmd != "M110 N0" {
		t.Fatalf("resend during barrier = %+v", l)
	}
	w.Ack(okN(0))
	if !w.CanSend() || w.Outstanding() != 0 {
		t.Fatalf("barrier not cleared, outstanding=%d", w.Outstanding())
	}
}

func TestWindowResendBudgetExpires(t *testing.T) {
	w := NewWindow(16)
	for i := 0; i < 4; i++ {
		w.Prepare("G1 X1")
	}
	if l, _, _ := w.Resend(1); len(l) != 4 {
		t.Fatalf("first resend = %+v", l)
	}
	w.mu.Lock()
	w.resendAt = w.resendAt.Add(-2 * resendIgnoreFor)
	w.mu.Unlock()
	if l, _, _ := w.Resend(1); len(l) != 4 {
		t.Fatalf("resend after the budget expired was ignored: %+v", l)
	}
	if l := w.Replay(); len(l) != 4 {
		t.Fatalf("replay = %+v", l)
	}
	if l, _, _ := w.Resend(1); len(l) != 4 {
		t.Fatalf("Replay did not clear the budget: %+v", l)
	}
}

func TestWindowBareOKAfterResendKeepsLine(t *testing.T) {
	// Non-ADVANCED_OK firmware: the bare ok after "Resend: 2" must not
	// drop line 2, which is being replayed.
	w := NewWindow(16)
	w.Prepare("G1 X1")
	w.Prepare("G1 X2")
	w.Ack(ParseLine("ok")) // acks N1
	lines, resync, _ := w.Resend(2)
	if resync || len(lines) != 1 || lines[0].N != 2 {
		t.Fatalf("resend = %+v resync=%v", lines, resync)
	}
	w.Ack(ParseLine("ok")) // the flush_and_request_resend ok
	if w.Outstanding() != 1 {
		t.Fatalf("outstanding = %d, want 1 (N2 still held)", w.Outstanding())
	}
	lines, resync, _ = w.Resend(2)
	if resync || len(lines) != 1 {
		t.Fatalf("second resend of N2 = %+v resync=%v", lines, resync)
	}
}

func TestWindowResendReplaysFromN(t *testing.T) {
	w := NewWindow(16)
	w.Prepare("G1 X1") // N1
	w.Prepare("G1 X2") // N2
	w.Prepare("G1 X3") // N3

	lines, resync, _ := w.Resend(2)
	if resync || len(lines) != 2 || lines[0].N != 2 || lines[1].N != 3 {
		t.Fatalf("resend = %+v resync=%v", lines, resync)
	}
	if w.Outstanding() != 3 {
		t.Fatalf("outstanding after resend = %d, want 3", w.Outstanding())
	}
	// N3 was in flight behind N2 and draws one duplicate "Resend: 2".
	if lines, resync, _ := w.Resend(2); resync || len(lines) != 0 {
		t.Fatalf("duplicate resend should be ignored, got %+v", lines)
	}
	// A further request is honoured again (replay itself got corrupted).
	if lines, _, _ := w.Resend(2); len(lines) != 2 {
		t.Fatalf("third resend should replay, got %+v", lines)
	}
}

func TestWindowStaleResendResyncs(t *testing.T) {
	w := NewWindow(16)
	for i := 0; i < 5; i++ {
		w.Prepare("G1 X1")
	}
	w.Ack(okN(3)) // N4, N5 outstanding
	lines, resync, k := w.Resend(1)
	if !resync || k != 3 || len(lines) != 1 || lines[0].Cmd != "M110 N3" || lines[0].N != 3 {
		t.Fatalf("resync=%v k=%d lines=%+v", resync, k, lines)
	}
	// N4, N5 are held until the M110 is acked, then released in order.
	if w.CanSend() || len(w.TakeReplay()) != 0 {
		t.Fatal("held lines released before the M110 ack")
	}
	w.Ack(ParseLine("ok")) // the flush_and_request_resend ok
	w.Ack(okN(3))
	if w.CanSend() {
		t.Fatal("CanSend before the released lines were taken")
	}
	held := w.TakeReplay()
	if len(held) != 2 || held[0].N != 4 || held[1].N != 5 || !w.CanSend() {
		t.Fatalf("released = %+v", held)
	}
	// Empty window: resync points at the next new line.
	w.Ack(okN(5))
	_, resync, k = w.Resend(1)
	if !resync || k != 5 {
		t.Fatalf("resync=%v k=%d", resync, k)
	}
}

func TestWindowResetClearsState(t *testing.T) {
	w := NewWindow(16)
	w.Prepare("G1 X1")
	w.Reset(1)
	if w.Outstanding() != 0 {
		t.Fatalf("outstanding after reset = %d, want 0", w.Outstanding())
	}
	n, framed := w.Prepare("M115")
	if n != 1 || framed != FrameLine(1, "M115") {
		t.Fatalf("n=%d framed=%q", n, framed)
	}
}
