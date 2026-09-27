package printer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSendRejectsInvalidCommands(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	d := New(a, 16)
	for _, c := range []string{"G28\nG28", "G28\r", "M117 a\x00b", "", "   ", "M112\n"} {
		if err := d.Send(c); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("Send(%q) = %v, want ErrInvalidCommand", c, err)
		}
		if err := d.SendEmergency(c); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("SendEmergency(%q) = %v, want ErrInvalidCommand", c, err)
		}
	}
}

func TestSendTimesOutWhenQueueStuck(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	d := New(a, 16) // never started: nothing drains the queue
	for i := 0; i < queueLen; i++ {
		if err := d.SendCtx(context.Background(), "G4 P0"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := d.SendCtx(ctx, "G4 P0"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendCtx on full queue = %v", err)
	}
	d.Close()
	if err := d.Send("G4 P0"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close = %v, want ErrClosed", err)
	}
}

// SEC-1: feeding a 50k-line stream never holds more than the queue plus the
// window in memory, and every line arrives once, in order.
func TestStreamMemoryBound(t *testing.T) {
	d, fm := startDriver(t, nil)
	const lines = 50000
	// queue + window, plus the one line the pump holds between taking it
	// off the queue and numbering it, plus one because queued and
	// Outstanding are read separately: a line moving from the queue into
	// the window between the two reads is counted twice.
	limit := int64(queueLen + 16 + 1 + 1)

	var peak atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			held := d.queued.Load() + int64(d.window.Outstanding())
			if held > peak.Load() {
				peak.Store(held)
			}
			select {
			case <-stop:
				return
			default:
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()

	for i := 0; i < lines; i++ {
		if err := d.SendCtx(context.Background(), fmt.Sprintf("G1 X%d", i)); err != nil {
			t.Fatal(err)
		}
		if held := d.queued.Load() + int64(d.window.Outstanding()); held > limit {
			t.Fatalf("holding %d lines, limit %d", held, limit)
		}
	}
	waitUntil(t, 30*time.Second, "stream drained", func() bool { return d.drained() })
	close(stop)
	wg.Wait()
	if p := peak.Load(); p > limit {
		t.Fatalf("peak held lines %d > %d", p, limit)
	}
	got := fm.acceptedCmds()
	var n int
	for _, c := range got {
		if strings.HasPrefix(c, "G1 X") {
			if c != fmt.Sprintf("G1 X%d", n) {
				t.Fatalf("line %d arrived as %q", n, c)
			}
			n++
		}
	}
	if n != lines {
		t.Fatalf("printer got %d lines, want %d", n, lines)
	}
}

func writeGcode(t *testing.T, lines int) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "G1 X%d Y%d\n", i, i)
	}
	path := t.TempDir() + "/big.gcode"
	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// SEC-2: while the upload owns the port, Send and RefreshFiles are refused
// (they would be written into the file); emergency commands still pass.
func TestSendDuringUploadIsBusy(t *testing.T) {
	hold := make(chan struct{})
	d, fm := startDriver(t, func(fm *fakeMarlin) {
		fm.holdAt = 10
		fm.hold = hold
	})
	names := NewNameMap(t.TempDir() + "/names.json")
	path := writeGcode(t, 200)

	done := make(chan error, 1)
	go func() {
		_, _, err := d.UploadToSD(context.Background(), path, "big.gcode", names, nil)
		done <- err
	}()
	waitUntil(t, 2*time.Second, "upload in progress", func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return fm.saved == 10
	})
	if !d.Uploading() {
		t.Fatal("Uploading() = false during upload")
	}
	if err := d.Send("G28"); !errors.Is(err, ErrBusy) {
		t.Fatalf("Send during upload = %v, want ErrBusy", err)
	}
	if err := d.RefreshFiles(); !errors.Is(err, ErrBusy) {
		t.Fatalf("RefreshFiles during upload = %v, want ErrBusy", err)
	}
	if _, _, err := d.UploadToSD(context.Background(), path, "b.gcode", names, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("second upload = %v, want ErrBusy", err)
	}
	// Emergency commands bypass ownership. The fake is not reading right
	// now (net.Pipe has no buffer), so the write completes after release.
	emerg := make(chan error, 1)
	go func() { emerg <- d.Send("M108") }()
	close(hold)
	if err := <-emerg; err != nil {
		t.Fatalf("emergency during upload: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("upload: %v", err)
	}
	fm.mu.Lock()
	content := fm.card["BIG.GCO"]
	fm.mu.Unlock()
	if strings.Contains(content, "G28") || strings.Count(content, "\n") != 200 {
		t.Fatalf("file content corrupted: %d lines, G28=%v", strings.Count(content, "\n"), strings.Contains(content, "G28"))
	}
	if err := d.Send("G28"); err != nil {
		t.Fatalf("Send after upload: %v", err)
	}
}

func TestUploadWaitsForM29Ack(t *testing.T) {
	m29 := make(chan struct{})
	d, fm := startDriver(t, func(fm *fakeMarlin) { fm.m29Hold = m29 })
	names := NewNameMap(t.TempDir() + "/names.json")
	path := writeGcode(t, 50)

	done := make(chan error, 1)
	go func() {
		_, _, err := d.UploadToSD(context.Background(), path, "wait.gcode", names, nil)
		done <- err
	}()
	waitUntil(t, 2*time.Second, "M29 received", func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return fm.sawM29
	})
	select {
	case err := <-done:
		t.Fatalf("UploadToSD returned (%v) before M29 was acked", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(m29)
	if err := <-done; err != nil {
		t.Fatalf("upload: %v", err)
	}
}

func TestUploadCancelSendsM29(t *testing.T) {
	hold := make(chan struct{})
	d, fm := startDriver(t, func(fm *fakeMarlin) {
		fm.holdAt = 20
		fm.hold = hold
	})
	names := NewNameMap(t.TempDir() + "/names.json")
	path := writeGcode(t, 5000)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := d.UploadToSD(ctx, path, "cancel.gcode", names, nil)
		done <- err
	}()
	waitUntil(t, 2*time.Second, "upload in progress", func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return fm.saved == 20
	})
	cancel()
	time.Sleep(20 * time.Millisecond)
	close(hold)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("upload = %v, want context.Canceled", err)
	}
	fm.mu.Lock()
	saving, sawM29 := fm.saving, fm.sawM29
	_, partial := fm.card["CANCEL.GCO"]
	saved := fm.saved
	fm.mu.Unlock()
	if saving || !sawM29 {
		t.Fatalf("printer still saving (sawM29=%v)", sawM29)
	}
	waitUntil(t, 2*time.Second, "partial file deleted", func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		_, partial = fm.card["CANCEL.GCO"]
		return !partial
	})
	if saved > 20+queueLen+16+1 {
		t.Fatalf("%d lines saved after cancel, feeder did not stop", saved)
	}
	if l := names.LongFor("CANCEL.GCO"); l != "CANCEL.GCO" {
		t.Fatalf("name mapping kept after failed upload: %q", l)
	}
}

func TestUploadOpenFailed(t *testing.T) {
	d, fm := startDriver(t, func(fm *fakeMarlin) { fm.failOpen = true })
	names := NewNameMap(t.TempDir() + "/names.json")
	path := writeGcode(t, 10)
	if _, _, err := d.UploadToSD(context.Background(), path, "x.gcode", names, nil); err == nil {
		t.Fatal("upload succeeded although M28 failed")
	}
	for _, c := range fm.acceptedCmds() {
		if strings.HasPrefix(c, "G1 ") {
			t.Fatalf("file line %q sent after a failed M28 (would run live)", c)
		}
	}
}

// SEC-11: a reboot after Handshake resets numbering, re-sends init and
// reports EventPrinterReset; normal commands work afterwards.
func TestStartBannerResync(t *testing.T) {
	var events []Event
	var mu sync.Mutex
	serverConn, clientConn := net.Pipe()
	fm := newFakeMarlin(serverConn)
	fm.start("start\n")
	d := New(clientConn, 16)
	d.OnEvent(func(ev Event, line string) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	})
	if err := d.Handshake(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	d.Start()
	defer func() { d.Close(); fm.stop() }()
	waitUntil(t, 2*time.Second, "init acked", func() bool { return fm.has("M20 L") && d.drained() })
	for i := 0; i < 5; i++ {
		d.Send("G4 P0")
	}
	waitUntil(t, 2*time.Second, "drained", d.drained)

	before := len(fm.acceptedCmds())
	fm.reboot()
	waitUntil(t, 2*time.Second, "reset event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) == 1 && events[0] == EventPrinterReset
	})
	if err := d.Send("G28"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, "G28 after reset", func() bool { return fm.has("G28") })
	after := fm.acceptedCmds()[before:]
	if strings.Join(after, "|") != "M110 N0|M115|M155 S2|M27 S2|M20 L|G28" {
		t.Fatalf("after reboot printer got %q", after)
	}
}

// SEC-11: a Resend for a line the window no longer holds (printer reset
// without a banner) resyncs with M110 instead of looping.
func TestStaleResendResyncs(t *testing.T) {
	d, fm := startDriver(t, nil)
	for i := 0; i < 5; i++ {
		d.Send("G4 P0")
	}
	waitUntil(t, 2*time.Second, "drained", d.drained)
	fm.mu.Lock()
	fm.lastN = 0 // silent reset: next line draws "Resend: 1"
	fm.resends = 0
	fm.mu.Unlock()

	if err := d.Send("G1 X5"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, "G1 accepted after resync", func() bool {
		return fm.has("G1 X5") && d.drained()
	})
	time.Sleep(50 * time.Millisecond)
	fm.mu.Lock()
	resends := fm.resends
	fm.mu.Unlock()
	if resends > 2 {
		t.Fatalf("%d resend requests: resend loop", resends)
	}
}

// G7: the bare "ok" Marlin prints right after "Resend:" must not drop the
// line being replayed, even when the replay itself is rejected again.
func TestBareOKAfterResendKeepsLine(t *testing.T) {
	d, fm := startDriver(t, nil)
	fm.mu.Lock()
	fm.reject[fm.lastN+1] = 2
	fm.mu.Unlock()
	if err := d.Send("G1 X7"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, "G1 X7 accepted", func() bool {
		return fm.has("G1 X7") && d.drained()
	})
	if err := d.Send("G1 X8"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, "next line accepted", func() bool { return fm.has("G1 X8") })
}

func TestDoneOnEOF(t *testing.T) {
	d, fm := startDriver(t, nil)
	fm.stop()
	select {
	case <-d.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed after EOF")
	}
	if d.Err() == nil {
		t.Fatal("Err() = nil after EOF")
	}
	if err := d.Send("G28"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after EOF = %v, want ErrClosed", err)
	}
	if _, _, err := d.UploadToSD(context.Background(), writeGcode(t, 1), "a.gcode", NewNameMap(t.TempDir()+"/n.json"), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("upload after EOF = %v, want ErrClosed", err)
	}
}

// G8/SEC-17: unquoted long names, subdirectories skipped, and an autoreport
// in the middle of the listing is not a file.
func TestM20ListingWithInterleavedTemps(t *testing.T) {
	d, _ := startDriver(t, func(fm *fakeMarlin) {
		fm.card["CUBE.GCO"] = "G28\n"
		fm.card["BENCHY~1.GCO"] = "G28\nG28\n"
		fm.listExtra = []string{
			" T:210.00 /210.00 B:60.00 /60.00 @:127 B@:80",
			"SUB/PART.GCO 99 Sub Dir/part.gcode",
		}
	})
	seq := d.listSeq.Load()
	if err := d.RefreshFiles(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, "listing", func() bool { return d.listSeq.Load() != seq })
	files := d.SDFiles()
	if len(files) != 2 || files[0].Short != "BENCHY~1.GCO" || files[0].Bytes != 8 || files[1].Short != "CUBE.GCO" || files[1].Long != "CUBE.GCO" {
		t.Fatalf("files = %+v", files)
	}
	if tm := d.Temps(); tm.HotendActual != 210 || tm.BedTarget != 60 {
		t.Fatalf("temps from interleaved autoreport = %+v", tm)
	}
}

func TestHaltedEvent(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	fm := newFakeMarlin(serverConn)
	fm.start("start\n")
	d := New(clientConn, 16)
	got := make(chan Event, 4)
	d.OnEvent(func(ev Event, line string) { got <- ev })
	if err := d.Handshake(2 * time.Second); err != nil {
		t.Fatal(err)
	}
	d.Start()
	defer func() { d.Close(); fm.stop() }()
	fm.send("Done printing file\nopen failed, File: X.GCO.\nError:Printer halted. kill() called!\n")
	for _, want := range []Event{EventDonePrinting, EventOpenFailed, EventHalted} {
		select {
		case ev := <-got:
			if ev != want {
				t.Fatalf("event %v, want %v", ev, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("event %v not delivered", want)
		}
	}
}

func TestLineReaderCapsLength(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	d := New(a, 16)
	go func() {
		b.Write([]byte(strings.Repeat("x", 3*maxLineLen) + "\nok N1\n"))
		b.Close()
	}()
	line, err := d.lr.readLine()
	if err != nil || line != "ok N1\n" {
		t.Fatalf("line=%q err=%v, want the overlong line discarded", line, err)
	}
}

func TestStripComment(t *testing.T) {
	cases := map[string]string{
		"G1 X1 ; move":            "G1 X1",
		"; only comment":          "",
		"M117 Layer (1/50); ok":   "M117 Layer (1/50)", // R-01: Marlin cuts at ; too
		"m118 E1 hi;there":        "m118 E1 hi",
		"M117 a\x5cb\x08c":        "M117 abc", // Marlin drops the escape; backspace removed
		"G1 X1 (paren kept)":      "G1 X1 (paren kept)",
		"G1 X1\rY1\r":             "G1 X1Y1",
		"  G28  ":                 "G28",
		"G1 X\x001":               "G1 X1",
		";M117 commented message": "",
	}
	for in, want := range cases {
		if got := stripComment(in); got != want {
			t.Errorf("stripComment(%q) = %q, want %q", in, got, want)
		}
	}
}

// R-01: a console command with a ';' comment reaches Marlin without it
// (Marlin would drop the checksum with the comment and loop on resends);
// too-long, escaped and M110 commands are refused.
func TestSendCommentAndLengthLimits(t *testing.T) {
	d, fm := startDriver(t, nil)
	if err := d.Send("M104 S0 ; heater off"); err != nil {
		t.Fatalf("Send with comment: %v", err)
	}
	waitUntil(t, 2*time.Second, "M104 accepted", func() bool { return fm.has("M104 S0") })
	long := "M117 " + strings.Repeat("x", 90)
	for _, c := range []string{long, "M117 a\x5cb", "M110 N5", "; only a comment"} {
		if err := d.Send(c); !errors.Is(err, ErrInvalidCommand) {
			t.Errorf("Send(%q) = %v, want ErrInvalidCommand", c, err)
		}
	}
	if err := d.Send("M117 " + strings.Repeat("y", 70)); err != nil {
		t.Fatalf("Send of a line that fits: %v", err)
	}
	waitUntil(t, 2*time.Second, "window drained", func() bool { return d.window.Outstanding() == 0 })
	fm.mu.Lock()
	resends := fm.resends
	fm.mu.Unlock()
	if resends != 0 {
		t.Fatalf("resends = %d", resends)
	}
}

// R-01: a file line too long for Marlin fails the upload cleanly: the save
// is closed (M29), the partial file deleted (M30), the name forgotten, and
// the error names the line.
func TestUploadRejectsTooLongLine(t *testing.T) {
	d, fm := startDriver(t, nil)
	names := NewNameMap(t.TempDir() + "/names.json")
	path := t.TempDir() + "/long.gcode"
	content := "G28\nG1 X1 ; fine\nM117 " + strings.Repeat("z", 100) + "\nG1 X2\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, _, err := d.UploadToSD(context.Background(), path, "long.gcode", names, nil)
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("upload = %v, want an error naming line 3", err)
	}
	waitUntil(t, 2*time.Second, "save closed and partial file deleted", func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		_, exists := fm.card["LONG.GCO"]
		return !fm.saving && fm.sawM29 && !exists
	})
	fm.mu.Lock()
	resends := fm.resends
	fm.mu.Unlock()
	if resends != 0 {
		t.Fatalf("resends = %d", resends)
	}
	if l := names.LongFor("LONG.GCO"); l != "LONG.GCO" {
		t.Fatalf("name mapping kept: %q", l)
	}
	if d.Uploading() {
		t.Fatal("still uploading")
	}
}

// R-02: the M29's "ok N" is lost. The drain probe's M105 reply must finish
// the upload instead of leaving the M105 itself outstanding forever.
func TestUploadLostM29OKCompletes(t *testing.T) {
	old := drainProbeAfter
	drainProbeAfter = 100 * time.Millisecond
	defer func() { drainProbeAfter = old }()
	d, fm := startDriver(t, func(fm *fakeMarlin) { fm.dropM29OK = true })
	names := NewNameMap(t.TempDir() + "/names.json")
	path := writeGcode(t, 30)
	done := make(chan error, 1)
	go func() {
		_, _, err := d.UploadToSD(context.Background(), path, "lost.gcode", names, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upload never finished after a lost M29 ok")
	}
	if _, ok := fm.card["LOST.GCO"]; !ok {
		t.Fatal("file missing")
	}
	if err := d.Send("G28"); err != nil {
		t.Fatalf("Send after upload: %v", err)
	}
}

// R-03: a line lost on the wire with nothing behind it (so Marlin never
// asks for it) is recovered by the ack-stall replay.
func TestAckStallReplays(t *testing.T) {
	old := stallAfter
	stallAfter = 100 * time.Millisecond
	defer func() { stallAfter = old }()
	d, fm := startDriver(t, nil)
	fm.mu.Lock()
	fm.swallow[fm.lastN+1] = 1
	fm.mu.Unlock()
	if err := d.Send("G1 X42"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "lost line replayed", func() bool {
		return fm.has("G1 X42") && d.window.Outstanding() == 0
	})
}

// R-03: the replay after a resend is lost too and nothing asks again;
// the stall replay recovers.
func TestAckStallAfterLostReplay(t *testing.T) {
	old := stallAfter
	stallAfter = 100 * time.Millisecond
	defer func() { stallAfter = old }()
	d, fm := startDriver(t, nil)
	fm.mu.Lock()
	next := fm.lastN + 1
	fm.reject[next] = 1  // first copy rejected -> Resend ...
	fm.loseReplay = true // ... and the replay of it is lost on the wire
	fm.mu.Unlock()
	for i := 0; i < 4; i++ {
		if err := d.Send(fmt.Sprintf("G1 X%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	waitUntil(t, 3*time.Second, "all acked", func() bool {
		return fm.has("G1 X3") && d.window.Outstanding() == 0
	})
	got := fm.acceptedCmds()
	if strings.Join(got[len(got)-4:], "|") != "G1 X0|G1 X1|G1 X2|G1 X3" {
		t.Fatalf("accepted tail = %q", got[len(got)-4:])
	}
}
