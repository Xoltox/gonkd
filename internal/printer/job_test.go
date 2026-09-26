package printer

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// jtFake is a small Marlin stand-in for Manager and Link tests. Replies go
// through a buffered writer goroutine so the fake never blocks on the pipe
// while the Driver is itself writing (net.Pipe is unbuffered).
type jtFake struct {
	conn net.Conn
	out  chan string
	done chan struct{} // closed when run returns

	mu        sync.Mutex
	seen      []string // command text without N and checksum
	saving    bool
	saved     strings.Builder
	saveDelay time.Duration // per line while saving (M28)
	ackDelay  time.Duration // per line otherwise
	openFail  bool
	mute      bool // read but never answer
	holdAbort bool // M524 does not produce "Not SD printing" on its own
}

func newJTFake(conn net.Conn) *jtFake {
	f := &jtFake{conn: conn, out: make(chan string, 4096), done: make(chan struct{})}
	go func() {
		for {
			select {
			case s := <-f.out:
				if _, err := conn.Write([]byte(s)); err != nil {
					return
				}
			case <-f.done:
				return
			}
		}
	}()
	return f
}

func (f *jtFake) emit(s string) {
	select {
	case f.out <- s:
	case <-f.done:
	}
}

func (f *jtFake) run() {
	defer close(f.done)
	f.emit("start\n")
	r := bufio.NewReader(f.conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		body, n, hasN := jtSplit(line)
		f.mu.Lock()
		f.seen = append(f.seen, body)
		saving, mute, openFail, holdAbort := f.saving, f.mute, f.openFail, f.holdAbort
		saveDelay, ackDelay := f.saveDelay, f.ackDelay
		f.mu.Unlock()
		if mute {
			continue
		}
		ok := "ok\n"
		if hasN {
			ok = fmt.Sprintf("ok N%d P30 B15\n", n)
		}
		if saving {
			if strings.HasPrefix(body, "M29") {
				f.mu.Lock()
				f.saving = false
				f.mu.Unlock()
				f.emit("Done saving file.\n" + ok)
				continue
			}
			f.mu.Lock()
			f.saved.WriteString(body + "\n")
			f.mu.Unlock()
			time.Sleep(saveDelay)
			f.emit(ok)
			continue
		}
		time.Sleep(ackDelay)
		switch {
		case body == "M112":
			// emergency parser: no reply
		case strings.HasPrefix(body, "M28 "):
			f.mu.Lock()
			f.saving = true
			f.mu.Unlock()
			f.emit("Writing to file: " + strings.TrimPrefix(body, "M28 ") + "\n" + ok)
		case body == "M524":
			// Like the M27 autoreport after Marlin's abort ran.
			if holdAbort {
				f.emit(ok)
			} else {
				f.emit(ok + "Not SD printing\n")
			}
		case strings.HasPrefix(body, "M20"):
			f.emit("Begin file list\nEnd file list\n" + ok)
		case strings.HasPrefix(body, "M23 "):
			name := strings.TrimPrefix(body, "M23 ")
			if openFail {
				f.emit("open failed, File: " + name + ".\n" + ok)
			} else {
				f.emit("File opened: " + name + " Size: 100\nFile selected\n" + ok)
			}
		default:
			f.emit(ok)
		}
	}
}

func jtSplit(line string) (body string, n int64, hasN bool) {
	body = line
	if i := strings.LastIndexByte(body, '*'); i >= 0 {
		body = body[:i]
	}
	if strings.HasPrefix(body, "N") {
		if sp := strings.IndexByte(body, ' '); sp > 0 {
			if v, err := strconv.ParseInt(body[1:sp], 10, 64); err == nil {
				n, hasN = v, true
				body = body[sp+1:]
			}
		}
	}
	return strings.TrimSpace(body), n, hasN
}

func (f *jtFake) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := 0
	for _, s := range f.seen {
		if strings.HasPrefix(s, prefix) {
			c++
		}
	}
	return c
}

func (f *jtFake) isSaving() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saving
}

func (f *jtFake) set(fn func(f *jtFake)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

func jtWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// jtConnect builds a started Driver talking to a fresh fake and attaches it
// to mgr, the way Link does.
func jtConnect(t *testing.T, mgr *Manager, setup func(f *jtFake)) (*Driver, *jtFake) {
	t.Helper()
	server, client := net.Pipe()
	f := newJTFake(server)
	if setup != nil {
		setup(f)
	}
	go f.run()
	d := New(client, 16)
	d.SetNames(mgr.Names)
	if err := d.Handshake(500 * time.Millisecond); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	mgr.Attach(d)
	d.Start()
	t.Cleanup(func() {
		d.Close()
		server.Close()
	})
	return d, f
}

func jtRig(t *testing.T, setup func(f *jtFake)) (*Manager, *jtFake) {
	t.Helper()
	mgr := NewManager(NewNameMap(filepath.Join(t.TempDir(), "names.json")))
	_, f := jtConnect(t, mgr, setup)
	return mgr, f
}

func jtGcode(t *testing.T, lines int) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "G1 X%d Y%d ; move\n", i%200, i%150)
	}
	path := filepath.Join(t.TempDir(), "job.gcode")
	if err := os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func jtGone(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

// jtIdleWorker waits until no upload/stream goroutine is left.
func jtIdleWorker(t *testing.T, mgr *Manager) {
	t.Helper()
	jtWait(t, "worker exit", func() bool {
		mgr.mu.Lock()
		defer mgr.mu.Unlock()
		return mgr.worker == nil
	})
}

func TestUploadIsAsyncAndPrintFollowsM27(t *testing.T) {
	mgr, f := jtRig(t, func(f *jtFake) { f.saveDelay = 2 * time.Millisecond })
	path := jtGcode(t, 200)

	start := time.Now()
	if err := mgr.Upload(path, "benchy.gcode", ModeSDUpload, true); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("Upload blocked for %v; it must return before the transfer", d)
	}
	if s := mgr.Snapshot(); s.State != StateUploading || s.Job == nil {
		t.Fatalf("right after Upload: state=%s job=%v, want uploading with a job", s.State, s.Job)
	}
	second := jtGcode(t, 1)
	if err := mgr.Upload(second, "second.gcode", ModeSDUpload, false); !errors.Is(err, ErrJobActive) {
		t.Fatalf("second Upload err = %v, want ErrJobActive", err)
	}

	jtWait(t, "M24", func() bool { return f.count("M24") == 1 })
	jtWait(t, "staged file removed", func() bool { return jtGone(path) })
	if s := mgr.Snapshot(); s.State != StatePrinting || s.Job == nil || s.Job.Filename != "benchy.gcode" {
		t.Fatalf("after upload: %+v", s)
	}

	f.emit("SD printing byte 50/100\n")
	jtWait(t, "M27 progress", func() bool {
		s := mgr.Snapshot()
		return s.Job != nil && s.Job.Progress == 50 && s.Job.TotalBytes == 100
	})
	f.emit("Done printing file\n")
	jtWait(t, "idle after Done printing", func() bool {
		s := mgr.Snapshot()
		return s.State == StateIdle && s.Job == nil
	})
	// The stale "SD printing" left in the driver must not look like an
	// LCD-started print.
	if s := mgr.Snapshot(); s.State != StateIdle {
		t.Fatalf("stale M27 revived a job: %+v", s)
	}
}

func TestUploadWithoutPrintEndsIdle(t *testing.T) {
	mgr, f := jtRig(t, nil)
	path := jtGcode(t, 20)
	if err := mgr.Upload(path, "cube.gcode", ModeSDUpload, false); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "idle", func() bool { s := mgr.Snapshot(); return s.State == StateIdle && s.Job == nil })
	jtIdleWorker(t, mgr)
	if f.count("M23") != 0 || f.count("M24") != 0 {
		t.Fatal("print started although startPrint=false")
	}
	if !jtGone(path) {
		t.Fatal("staged file not removed")
	}
}

func TestNotSDPrintingAfterPrintingGoesIdle(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if err := mgr.StartSDPrint("cube.gco"); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "M24", func() bool { return f.count("M24") == 1 })
	if f.count("M23 CUBE.GCO") != 1 {
		t.Fatal("M23 CUBE.GCO not sent")
	}
	f.emit("Not SD printing\n") // stale-looking report before the print began
	time.Sleep(50 * time.Millisecond)
	if s := mgr.Snapshot(); s.State != StatePrinting {
		t.Fatalf("Not SD printing before any progress ended the job: %s", s.State)
	}
	f.emit("SD printing byte 10/100\n")
	jtWait(t, "progress 10", func() bool { s := mgr.Snapshot(); return s.Job != nil && s.Job.Progress == 10 })
	f.emit("Not SD printing\n")
	jtWait(t, "idle", func() bool { return mgr.Snapshot().State == StateIdle })
}

func TestCancelDuringSDUploadSendsNoM524(t *testing.T) {
	mgr, f := jtRig(t, func(f *jtFake) { f.saveDelay = 5 * time.Millisecond })
	path := jtGcode(t, 2000)
	if err := mgr.Upload(path, "long.gcode", ModeSDUpload, true); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "upload progress", func() bool {
		s := mgr.Snapshot()
		return s.Job != nil && s.Job.SentBytes > 0
	})
	if err := mgr.Cancel(); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if s := mgr.Snapshot(); s.State != StateIdle || s.Job != nil {
		t.Fatalf("after Cancel: %+v", s)
	}
	jtIdleWorker(t, mgr)
	jtWait(t, "M29 closes the file", func() bool { return !f.isSaving() })
	for _, c := range []string{"M524", "M104", "M140", "G1 Z10", "M23", "M24"} {
		if n := f.count(c); n != 0 {
			t.Errorf("%s sent %d times after cancelling an upload", c, n)
		}
	}
	if !jtGone(path) {
		t.Fatal("staged file not removed")
	}
}

func TestPauseDispatchesOnJobMode(t *testing.T) {
	t.Run("sd", func(t *testing.T) {
		mgr, f := jtRig(t, nil)
		if err := mgr.Pause(); !errors.Is(err, ErrNoJob) {
			t.Fatalf("Pause with no job = %v, want ErrNoJob", err)
		}
		if err := mgr.StartSDPrint("CUBE.GCO"); err != nil {
			t.Fatal(err)
		}
		if err := mgr.Pause(); err != nil {
			t.Fatal(err)
		}
		jtWait(t, "M25", func() bool { return f.count("M25") == 1 })
		if s := mgr.State(); s != StatePaused {
			t.Fatalf("state %s, want paused", s)
		}
		if err := mgr.Resume(); err != nil {
			t.Fatal(err)
		}
		jtWait(t, "second M24", func() bool { return f.count("M24") == 2 })
		f.emit("SD printing byte 10/100\n")
		jtWait(t, "SD progress", func() bool { s := mgr.Snapshot(); return s.Job != nil && s.Job.Progress == 10 })
		if err := mgr.Cancel(); err != nil {
			t.Fatal(err)
		}
		jtWait(t, "M524 and cooldown", func() bool { return f.count("M524") == 1 && f.count("M104 S0") == 1 })
	})
	t.Run("stream", func(t *testing.T) {
		mgr, f := jtRig(t, func(f *jtFake) { f.ackDelay = 3 * time.Millisecond })
		path := jtGcode(t, 3000)
		if err := mgr.Upload(path, "stream.gcode", ModeStream, true); err != nil {
			t.Fatal(err)
		}
		jtWait(t, "streaming", func() bool { return f.count("G1 X") > 5 })
		if err := mgr.Pause(); err != nil {
			t.Fatal(err)
		}
		if s := mgr.State(); s != StatePaused {
			t.Fatalf("state %s, want paused", s)
		}
		// Lines already queued in the driver still drain; wait until the
		// count settles, then make sure it stays put.
		before := -1
		jtWait(t, "queue drained", func() bool {
			n := f.count("G1 X")
			settled := n == before
			before = n
			time.Sleep(100 * time.Millisecond)
			return settled
		})
		time.Sleep(150 * time.Millisecond)
		if after := f.count("G1 X"); after != before {
			t.Fatalf("feeder kept sending while paused: %d -> %d", before, after)
		}
		if err := mgr.Resume(); err != nil {
			t.Fatal(err)
		}
		jtWait(t, "stream resumes", func() bool { return f.count("G1 X") > before })
		if err := mgr.Cancel(); err != nil {
			t.Fatal(err)
		}
		jtIdleWorker(t, mgr)
		jtWait(t, "cooldown", func() bool { return f.count("M104 S0") == 1 })
		if f.count("M25") != 0 || f.count("M524") != 0 {
			t.Fatal("SD commands sent for a stream job")
		}
		if !jtGone(path) {
			t.Fatal("staged file not removed")
		}
	})
}

func TestStreamRunsToCompletion(t *testing.T) {
	mgr, f := jtRig(t, nil)
	path := jtGcode(t, 100)
	if err := mgr.Upload(path, "s.gcode", ModeStream, true); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "idle", func() bool { return mgr.Snapshot().State == StateIdle })
	jtIdleWorker(t, mgr)
	if n := f.count("G1 X"); n != 100 {
		t.Fatalf("fake saw %d moves, want 100", n)
	}
	if f.count("M73 P100") != 1 {
		t.Fatal("final M73 P100 missing")
	}
}

func TestDetachDuringUploadIsAnError(t *testing.T) {
	mgr, _ := jtRig(t, func(f *jtFake) { f.saveDelay = 5 * time.Millisecond })
	path := jtGcode(t, 2000)
	if err := mgr.Upload(path, "long.gcode", ModeSDUpload, true); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "upload progress", func() bool {
		s := mgr.Snapshot()
		return s.Job != nil && s.Job.SentBytes > 0
	})
	mgr.Detach("test unplug")
	s := mgr.Snapshot()
	if s.State != StateError || s.Job != nil || s.Connected {
		t.Fatalf("after Detach: %+v", s)
	}
	if !strings.Contains(s.LastError, "connection lost during upload") {
		t.Fatalf("LastError = %q", s.LastError)
	}
	jtIdleWorker(t, mgr)
	if !jtGone(path) {
		t.Fatal("staged file not removed")
	}
}

func TestDetachKeepsSDPrintAndReconciles(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if err := mgr.StartSDPrint("CUBE.GCO"); err != nil {
		t.Fatal(err)
	}
	f.emit("SD printing byte 10/100\n")
	jtWait(t, "progress", func() bool { s := mgr.Snapshot(); return s.Job != nil && s.Job.Progress == 10 })

	mgr.Detach("test unplug")
	s := mgr.Snapshot()
	if s.State != StateDisconnected || s.Job == nil || s.Job.Note == "" {
		t.Fatalf("after Detach: %+v", s)
	}
	if err := mgr.Send("M105"); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Send while detached = %v", err)
	}

	// Reconnect, printer still printing: the job continues.
	_, f2 := jtConnect(t, mgr, nil)
	if s := mgr.Snapshot(); s.State != StatePrinting || s.Job == nil {
		t.Fatalf("after re-Attach: %+v", s)
	}
	f2.emit("SD printing byte 40/100\n")
	jtWait(t, "reconciled", func() bool {
		s := mgr.Snapshot()
		return s.Job != nil && s.Job.Progress == 40 && s.Job.Note == ""
	})

	// Lose it again; this time the printer comes back without the print.
	mgr.Detach("test unplug 2")
	_, f3 := jtConnect(t, mgr, nil)
	f3.emit("Not SD printing\n")
	jtWait(t, "print gone", func() bool {
		s := mgr.Snapshot()
		return s.State == StateIdle && s.Job == nil && strings.Contains(s.LastError, "no longer running")
	})
}

func TestPrintStartedOnPrinterBecomesJob(t *testing.T) {
	mgr, f := jtRig(t, nil)
	f.emit("SD printing byte 5/100\n")
	jtWait(t, "LCD job", func() bool {
		s := mgr.Snapshot()
		return s.State == StatePrinting && s.Job != nil && s.Job.Filename == lcdJobName && s.Job.Progress == 5
	})
	if err := mgr.RefreshFiles(); !errors.Is(err, ErrJobActive) {
		t.Fatalf("RefreshFiles during a print = %v, want ErrJobActive", err)
	}
	f.emit("Done printing file\n")
	jtWait(t, "idle", func() bool { return mgr.Snapshot().State == StateIdle })
}

func TestOpenFailedIsAnError(t *testing.T) {
	mgr, _ := jtRig(t, func(f *jtFake) { f.openFail = true })
	if err := mgr.StartSDPrint("MISSING.GCO"); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "error state", func() bool {
		s := mgr.Snapshot()
		return s.State == StateError && s.Job == nil && strings.Contains(s.LastError, "could not open")
	})
}

func TestEmergencySendsM112(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if err := mgr.StartSDPrint("CUBE.GCO"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Emergency(); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "M112", func() bool { return f.count("M112") == 1 })
	if s := mgr.Snapshot(); s.State != StateError || s.Job != nil {
		t.Fatalf("after Emergency: %+v", s)
	}
}

func TestBabystep(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if err := mgr.Babystep(1.5); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("Babystep(1.5) = %v, want ErrInvalidCommand", err)
	}
	if err := mgr.Babystep(-0.01); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Babystep(0.05); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SaveSettings(); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "M290/M500", func() bool {
		return f.count("M290 Z-0.01") == 1 && f.count("M290 Z0.05") == 1 && f.count("M500") == 1
	})
	for in, want := range map[float64]string{0.05: "0.05", -0.01: "-0.01", 1: "1", 0.1 + 0.2: "0.3", -0.0001: "0", 0.125: "0.125"} {
		if got := formatMM(in); got != want {
			t.Errorf("formatMM(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestSendSequenceKeepsOrder(t *testing.T) {
	mgr, f := jtRig(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := mgr.SendSequence([]string{"G91", fmt.Sprintf("G1 X%d", i+1), "G90"}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	jtWait(t, "all jogs", func() bool { return f.count("G90") == 5 })
	f.mu.Lock()
	defer f.mu.Unlock()
	var jog []string
	for _, s := range f.seen {
		if s == "G91" || s == "G90" || strings.HasPrefix(s, "G1 X") {
			jog = append(jog, s)
		}
	}
	for i := 0; i+2 < len(jog); i += 3 {
		if jog[i] != "G91" || !strings.HasPrefix(jog[i+1], "G1 X") || jog[i+2] != "G90" {
			t.Fatalf("interleaved jog sequence: %v", jog)
		}
	}
}

func TestManagerWithoutPrinter(t *testing.T) {
	mgr := NewManager(NewNameMap(filepath.Join(t.TempDir(), "names.json")))
	if s := mgr.Snapshot(); s.Connected || s.State != StateDisconnected {
		t.Fatalf("fresh manager: %+v", s)
	}
	if err := mgr.Send("M105"); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Send = %v", err)
	}
	path := jtGcode(t, 5)
	if err := mgr.Upload(path, "x.gcode", ModeSDUpload, true); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Upload = %v", err)
	}
	if !jtGone(path) {
		t.Fatal("Upload must remove the staged file even when it refuses")
	}
	if got := mgr.Console(); got == nil || len(got) != 0 {
		t.Fatalf("Console = %v, want empty slice", got)
	}
	if err := mgr.Emergency(); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("Emergency = %v", err)
	}
}

// R-04: nothing may follow M524 until Marlin has confirmed the abort, and
// the park is sent as one self-contained G91/G1/G90 group.
func TestCancelSDParksOnlyAfterAbortConfirmed(t *testing.T) {
	mgr, f := jtRig(t, func(f *jtFake) { f.holdAbort = true })
	if err := mgr.StartSDPrint("CUBE.GCO"); err != nil {
		t.Fatal(err)
	}
	f.emit("SD printing byte 10/100\n")
	jtWait(t, "SD progress", func() bool { s := mgr.Snapshot(); return s.Job != nil && s.Job.Progress == 10 })
	done := make(chan error, 1)
	go func() { done <- mgr.Cancel() }()
	jtWait(t, "M524", func() bool { return f.count("M524") == 1 })
	time.Sleep(300 * time.Millisecond)
	for _, c := range []string{"M104 S0", "G91", "G1 Z10"} {
		if n := f.count(c); n != 0 {
			t.Fatalf("%s sent before the abort was confirmed", c)
		}
	}
	f.emit("Not SD printing\n")
	if err := <-done; err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	jtWait(t, "park", func() bool { return f.count("G90") == 1 })
	f.mu.Lock()
	defer f.mu.Unlock()
	var tail []string
	for i, s := range f.seen {
		if s == "M524" {
			tail = f.seen[i+1:]
		}
	}
	want := []string{"M104 S0", "M140 S0", "M107", "G91", "G1 Z10 F600", "G90"}
	if len(tail) < len(want) {
		t.Fatalf("after M524: %v", tail)
	}
	for i, w := range want {
		if tail[i] != w {
			t.Fatalf("after M524: %v, want %v", tail, want)
		}
	}
}

// Without an abort confirmation Cancel still cools down but never parks.
func TestCancelSDWithoutConfirmationSkipsPark(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the cancel timeout")
	}
	mgr, f := jtRig(t, func(f *jtFake) { f.holdAbort = true })
	if err := mgr.StartSDPrint("CUBE.GCO"); err != nil {
		t.Fatal(err)
	}
	f.emit("SD printing byte 10/100\n")
	jtWait(t, "SD progress", func() bool { s := mgr.Snapshot(); return s.Job != nil && s.Job.Progress == 10 })
	if err := mgr.Cancel(); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "cooldown", func() bool { return f.count("M107") == 1 })
	if f.count("G91") != 0 || f.count("G1 Z10") != 0 {
		t.Fatal("parked without an abort confirmation")
	}
	if s := mgr.Snapshot(); !strings.Contains(s.LastError, "did not confirm") {
		t.Fatalf("lastError = %q", s.LastError)
	}
}

// A printer reset mid-stream stops the feeder, fails the job with a clear
// error and switches the heaters off once the feeder is gone.
func TestPrinterResetStopsStream(t *testing.T) {
	mgr, f := jtRig(t, func(f *jtFake) { f.ackDelay = 3 * time.Millisecond })
	path := jtGcode(t, 3000)
	if err := mgr.Upload(path, "stream.gcode", ModeStream, true); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "streaming", func() bool { return f.count("G1 X") > 5 })
	f.emit("start\n")
	jtWait(t, "error state", func() bool {
		s := mgr.Snapshot()
		return s.State == StateError && s.Job == nil && strings.Contains(s.LastError, "reset")
	})
	jtIdleWorker(t, mgr)
	jtWait(t, "cooldown", func() bool { return f.count("M104 S0") == 1 && f.count("M107") == 1 })
	before := f.count("G1 X")
	time.Sleep(200 * time.Millisecond)
	if after := f.count("G1 X"); after != before {
		t.Fatalf("feeder kept sending after the reset: %d -> %d", before, after)
	}
	if before >= 3000 {
		t.Fatal("stream ran to completion despite the reset")
	}
	if !jtGone(path) {
		t.Fatal("staged file not removed")
	}
}

// R-05: motion and EEPROM commands are refused while a job runs; babystep
// and plain queries still go through.
func TestMotionRefusedDuringJob(t *testing.T) {
	mgr, f := jtRig(t, nil)
	if err := mgr.StartSDPrint("CUBE.GCO"); err != nil {
		t.Fatal(err)
	}
	for _, seq := range [][]string{{"G28"}, {"G91", "G1 X10", "G90"}, {"g0 z5"}, {"M500"}} {
		if err := mgr.SendSequence(seq); !errors.Is(err, ErrJobActive) {
			t.Fatalf("SendSequence(%v) during a job = %v, want ErrJobActive", seq, err)
		}
	}
	if err := mgr.SaveSettings(); !errors.Is(err, ErrJobActive) {
		t.Fatalf("SaveSettings during a job = %v", err)
	}
	if err := mgr.Send("M105"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Babystep(0.02); err != nil {
		t.Fatal(err)
	}
	jtWait(t, "M290", func() bool { return f.count("M290 Z0.02") == 1 })
	if f.count("G28") != 0 || f.count("G91") != 0 || f.count("M500") != 0 {
		t.Fatal("refused command reached the printer")
	}
}

// R-09: an SD print reported while gonkd shows an error is adopted, so the
// next upload is refused instead of aborting it with M28.
func TestPrintAdoptedInErrorState(t *testing.T) {
	mgr, f := jtRig(t, nil)
	mgr.mu.Lock()
	mgr.state, mgr.lastErr = StateError, "old failure"
	mgr.mu.Unlock()
	f.emit("SD printing byte 5/100\n")
	jtWait(t, "adopted", func() bool { s := mgr.Snapshot(); return s.State == StatePrinting && s.Job != nil })
	path := jtGcode(t, 5)
	if err := mgr.Upload(path, "x.gcode", ModeSDUpload, true); !errors.Is(err, ErrJobActive) {
		t.Fatalf("Upload during an adopted print = %v, want ErrJobActive", err)
	}
}

func TestUploadRejectsUnknownMode(t *testing.T) {
	mgr, _ := jtRig(t, nil)
	path := jtGcode(t, 5)
	if err := mgr.Upload(path, "x.gcode", UploadMode("bogus"), true); !errors.Is(err, ErrInvalidMode) {
		t.Fatalf("Upload(bogus) = %v, want ErrInvalidMode", err)
	}
	if !jtGone(path) {
		t.Fatal("staged file not removed")
	}
}
