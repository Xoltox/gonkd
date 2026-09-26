package printer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"forge/internal/gcode"
)

// fakeMarlin emulates just enough of Marlin 2.1's serial host protocol to
// exercise Driver end to end: line-number and checksum checks with
// "Error:..." + "Resend: <n>" + bare "ok" (queue.cpp gcode_line_error /
// flush_and_request_resend), ADVANCED_OK acks ("ok N<n> P<p> B<b>"), M110
// renumbering, M28/M29 SD saving (every line but M29 goes into the file),
// M20 L listings with unquoted long names, M105's own "ok T:..." reply, and
// "start" on reboot.
//
// It runs over an in-process net.Pipe. Replies go through a buffered
// writer goroutine because net.Pipe has no buffering at all, unlike a real
// tty: a fake that blocked on its own writes would deadlock tests in ways
// the hardware cannot.
type fakeMarlin struct {
	conn net.Conn
	out  chan string
	done chan struct{}

	mu         sync.Mutex
	lastN      int64
	seen       []string // every raw line received
	accepted   []string // commands accepted in order ("raw:" prefix = unnumbered)
	resends    int
	reject     map[int64]int // line -> times to answer with a checksum error
	swallow    map[int64]int // line -> times to lose it on the wire (no reply at all)
	dropM29OK  bool          // lose the "ok N" of the M29 that ends a save
	loseReplay bool          // after a reject, also lose the next copy of that line
	saving     bool
	saveName   string
	card       map[string]string // SD files: short name -> content
	listExtra  []string          // lines interleaved into the M20 listing
	failOpen   bool
	holdAt     int           // block reading after this many saved lines ...
	hold       chan struct{} // ... until hold is closed
	saved      int
	m29Hold    chan struct{} // delays the M29 reply until closed
	sawM29     bool
	m29Acked   bool
}

func newFakeMarlin(conn net.Conn) *fakeMarlin {
	return &fakeMarlin{
		conn:    conn,
		out:     make(chan string, 1<<16),
		done:    make(chan struct{}),
		reject:  map[int64]int{},
		swallow: map[int64]int{},
		card:    map[string]string{},
	}
}

// start runs the fake; greeting is sent first (e.g. "start\n" after the
// DTR reset on open).
func (f *fakeMarlin) start(greeting string) {
	go f.writer()
	if greeting != "" {
		f.send(greeting)
	}
	go f.reader()
}

func (f *fakeMarlin) send(s string) {
	select {
	case f.out <- s:
	case <-f.done:
	}
}

func (f *fakeMarlin) writer() {
	for {
		select {
		case s := <-f.out:
			if _, err := io.WriteString(f.conn, s); err != nil {
				return
			}
		case <-f.done:
			return
		}
	}
}

func (f *fakeMarlin) stop() {
	f.mu.Lock()
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	f.mu.Unlock()
	f.conn.Close()
}

// reboot simulates a printer reset: line counter back to 0, save aborted,
// "start" banner.
func (f *fakeMarlin) reboot() {
	f.mu.Lock()
	f.lastN = 0
	f.saving = false
	f.mu.Unlock()
	f.send("start\necho:Marlin 2.1.2.7\n")
}

func (f *fakeMarlin) okN(n int64) string { return fmt.Sprintf("ok N%d P63 B15\n", n) }

func (f *fakeMarlin) reader() {
	r := bufio.NewReader(f.conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			f.stop()
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		f.mu.Lock()
		f.seen = append(f.seen, line)
		hold := f.hold
		blocked := f.saving && hold != nil && f.saved == f.holdAt
		f.mu.Unlock()
		if blocked {
			<-hold
		}
		f.handle(marlinStore(line))
	}
}

// marlinStore is what queue.cpp process_stream_char keeps of a received
// line in this build (no PAREN_COMMENTS, no GCODE_QUOTED_STRINGS): a
// backslash escapes the next byte and is dropped, ';' ends the line (for
// every command, M117/M118 included), 0x08 erases the previous byte, and
// from the 95th stored byte on the rest is discarded (MAX_CMD_SIZE 96).
// The checksum is then verified over what was kept.
func marlinStore(raw string) string {
	var b []byte
	esc := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case esc:
			esc = false
		case c == '\\':
			esc = true
			continue
		case c == ';':
			return string(b)
		}
		if c == 0x08 {
			if len(b) > 0 {
				b = b[:len(b)-1]
			}
			continue
		}
		b = append(b, c)
		if len(b) >= gcode.MaxFramedLen {
			return string(b)
		}
	}
	return string(b)
}

func (f *fakeMarlin) handle(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(line, "N") {
		f.accepted = append(f.accepted, "raw:"+line)
		return
	}
	star := strings.LastIndexByte(line, '*')
	sp := strings.IndexByte(line, ' ')
	if star < 0 || sp < 0 || sp > star {
		f.lineError("No Checksum with line number")
		return
	}
	n, err := strconv.ParseInt(line[1:sp], 10, 64)
	if err == nil && f.swallow[n] > 0 {
		f.swallow[n]--
		return
	}
	cs, err2 := strconv.Atoi(line[star+1:])
	if err != nil || err2 != nil || byte(cs) != gcode.Checksum(line[:star]) {
		f.lineError("checksum mismatch")
		return
	}
	cmd := line[sp+1 : star]
	if strings.HasPrefix(cmd, "M110") {
		// M110 is exempt from the sequence check; its N argument wins.
		k := n
		if i := strings.Index(cmd, "N"); i >= 0 {
			k, _ = strconv.ParseInt(strings.Fields(cmd[i+1:])[0], 10, 64)
		}
		f.lastN = k
		f.accepted = append(f.accepted, cmd)
		f.send(f.okN(n))
		return
	}
	if n != f.lastN+1 {
		if n >= f.lastN-1 && n <= f.lastN {
			return // queue.cpp: a duplicate of the last two lines is dropped silently
		}
		f.lineError("Line Number is not Last Line Number+1")
		return
	}
	if f.reject[n] > 0 {
		f.reject[n]--
		if f.loseReplay {
			f.swallow[n]++ // the replay of this line is lost as well
		}
		f.lineError("checksum mismatch")
		return
	}
	f.lastN = n
	f.accepted = append(f.accepted, cmd)

	if f.saving {
		if cmd == "M29" {
			f.saving = false
			f.sawM29 = true
			reply := "Done saving file.\n" + f.okN(n)
			if f.dropM29OK {
				reply = "Done saving file.\n"
			}
			if f.m29Hold != nil {
				hold := f.m29Hold
				go func() {
					<-hold
					f.mu.Lock()
					f.m29Acked = true
					f.mu.Unlock()
					f.send(reply)
				}()
				return
			}
			f.m29Acked = true
			f.send(reply)
			return
		}
		f.card[f.saveName] += cmd + "\n"
		f.saved++
		f.send(f.okN(n))
		return
	}

	word := strings.Fields(cmd)[0]
	switch word {
	case "M28":
		name := strings.TrimSpace(strings.TrimPrefix(cmd, "M28"))
		if f.failOpen {
			f.send("open failed, File: " + name + ".\n" + f.okN(n))
			return
		}
		f.saving = true
		f.saveName = name
		f.card[name] = ""
		f.send("Writing to file: " + name + "\n" + f.okN(n))
	case "M29":
		f.send(f.okN(n))
	case "M30":
		delete(f.card, strings.TrimSpace(strings.TrimPrefix(cmd, "M30")))
		f.send(f.okN(n))
	case "M20":
		var b strings.Builder
		b.WriteString("Begin file list\n")
		names := make([]string, 0, len(f.card))
		for k := range f.card {
			names = append(names, k)
		}
		sort.Strings(names)
		for i, k := range names {
			fmt.Fprintf(&b, "%s %d %s\n", k, len(f.card[k]), k)
			if i == 0 {
				for _, x := range f.listExtra {
					b.WriteString(x + "\n")
				}
			}
		}
		b.WriteString("End file list\n")
		b.WriteString(f.okN(n))
		f.send(b.String())
	case "M105":
		f.send("ok T:20.00 /0.00 B:20.00 /0.00 @:0 B@:0\n")
	default:
		f.send(f.okN(n))
	}
}

// lineError answers like queue.cpp gcode_line_error: error text, then
// "Resend: last+1" and a bare "ok". Caller holds f.mu.
func (f *fakeMarlin) lineError(msg string) {
	f.resends++
	f.send(fmt.Sprintf("Error:%s, Last Line: %d\nResend: %d\nok\n", msg, f.lastN, f.lastN+1))
}

func (f *fakeMarlin) acceptedCmds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.accepted...)
}

func (f *fakeMarlin) has(cmd string) bool {
	for _, c := range f.acceptedCmds() {
		if c == cmd {
			return true
		}
	}
	return false
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startDriver wires a Driver to a fresh fake and runs Handshake + Start.
func startDriver(t *testing.T, setup func(*fakeMarlin)) (*Driver, *fakeMarlin) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	fm := newFakeMarlin(serverConn)
	if setup != nil {
		setup(fm)
	}
	fm.start("start\necho:Marlin 2.1.2.7\nCap:AUTOREPORT_TEMP:1\n")
	d := New(clientConn, 16)
	if err := d.Handshake(2 * time.Second); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	d.Start()
	t.Cleanup(func() {
		d.Close()
		fm.stop()
	})
	waitUntil(t, 2*time.Second, "init commands acked", func() bool {
		return fm.has("M20 L") && d.window.Outstanding() == 0
	})
	return d, fm
}

func TestDriverHandshakeAndSend(t *testing.T) {
	d, fm := startDriver(t, nil)

	if err := d.Send("G28"); err != nil {
		t.Fatal(err)
	}
	if err := d.Send("G1 X10"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 2*time.Second, "G1 accepted", func() bool { return fm.has("G1 X10") })
	waitUntil(t, 2*time.Second, "window drained", func() bool { return d.window.Outstanding() == 0 })

	fm.mu.Lock()
	seen := append([]string(nil), fm.seen...)
	resends := fm.resends
	fm.mu.Unlock()
	// SEC-28: the handshake M110 is N0, so its "ok N0" cannot ack N1.
	if seen[0] != "N0 M110 N0*125" || !strings.HasPrefix(seen[1], "N1 M29*") || !strings.HasPrefix(seen[2], "N2 M115*") {
		t.Fatalf("wire start = %q", seen[:3])
	}
	if resends != 0 {
		t.Fatalf("unexpected resends: %d", resends)
	}
	// G13: banner lines after "start" survive into readLoop.
	if !d.Capabilities()["AUTOREPORT_TEMP"] {
		t.Fatal("Cap: line after start banner was lost")
	}
	if d.LastRX().IsZero() {
		t.Fatal("LastRX not set")
	}
}

func TestDriverResendRecovery(t *testing.T) {
	d, fm := startDriver(t, nil)
	fm.mu.Lock()
	next := fm.lastN + 1
	fm.reject[next+1] = 1 // second of the three lines is rejected once
	fm.mu.Unlock()

	for _, c := range []string{"G1 X10", "G1 X20", "G1 X30"} {
		if err := d.Send(c); err != nil {
			t.Fatal(err)
		}
	}
	waitUntil(t, 2*time.Second, "all acked", func() bool {
		return fm.has("G1 X30") && d.window.Outstanding() == 0
	})
	got := fm.acceptedCmds()
	got = got[len(got)-3:]
	if strings.Join(got, "|") != "G1 X10|G1 X20|G1 X30" {
		t.Fatalf("accepted tail = %q", got)
	}
}

func TestDriverASCIIUploadCapturesBytes(t *testing.T) {
	dir := t.TempDir()
	names := NewNameMap(dir + "/names.json")
	d, fm := startDriver(t, func(fm *fakeMarlin) {
		fm.card["TEST.GCO"] = "G28\n" // not forge's: must not be overwritten
	})

	path := dir + "/test.gcode"
	content := "G28 ; home\r\n" +
		"G1 X1 Y1\n" +
		"; a comment\n" +
		"M117 Layer (1/50); still text\n" +
		"G1 X2\rY2\n" + // interior CR is stripped
		"M29\n" + // never forwarded: would end the save early
		"G1 X3 (not a comment)\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	var lastSent, lastTotal int64
	short, err := d.UploadToSD(context.Background(), path, "test.gcode", names, func(s, tot int64) {
		lastSent, lastTotal = s, tot
	})
	if err != nil {
		t.Fatalf("UploadToSD: %v", err)
	}
	if short != "TEST~1.GCO" {
		t.Fatalf("short = %q, want TEST~1.GCO (TEST.GCO is on the card)", short)
	}
	if names.LongFor("TEST.GCO") != "TEST.GCO" {
		t.Fatal("rejected candidate TEST.GCO left in the name map")
	}
	if lastSent != lastTotal || lastTotal != int64(len(content)) {
		t.Fatalf("final progress %d/%d", lastSent, lastTotal)
	}
	fm.mu.Lock()
	got, orig := fm.card[short], fm.card["TEST.GCO"]
	acked := fm.m29Acked
	fm.mu.Unlock()
	want := "G28\nG1 X1 Y1\nM117 Layer (1/50)\nG1 X2Y2\nG1 X3 (not a comment)\n"
	if got != want {
		t.Fatalf("uploaded content = %q, want %q", got, want)
	}
	if orig != "G28\n" {
		t.Fatalf("pre-existing TEST.GCO changed: %q", orig)
	}
	if !acked {
		t.Fatal("UploadToSD returned before the M29 ack")
	}
	if d.Uploading() {
		t.Fatal("still uploading after return")
	}
}
