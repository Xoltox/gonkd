package printer

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMarlin emulates just enough of Marlin's serial host protocol to
// exercise Driver end to end: ADVANCED_OK acks with P/B counts, occasional
// "busy" and "Resend" injection, and M28/M29 ASCII SD-upload capture.
//
// NOTE: this runs over an in-process net.Pipe rather than a real pty --
// the behavior under test (line framing, resend recovery, ADVANCED_OK
// backpressure, ASCII upload capture) doesn't depend on tty semantics, and
// net.Pipe gives fully deterministic, race-free tests. A real pty (via
// socat or a Go pty library) and a qemu-mipsel-static smoke run of the
// cross-compiled binary were both out of scope for this pass; see the
// README's "Known gaps" section.
type fakeMarlin struct {
	conn        net.Conn
	mu          sync.Mutex
	seenLines   []string
	nextExpectN int64
	forceResend map[int64]bool // line numbers to Resend exactly once
	uploading   bool
	upload      strings.Builder
	uploadName  string
}

func newFakeMarlin(conn net.Conn) *fakeMarlin {
	return &fakeMarlin{conn: conn, nextExpectN: 1, forceResend: map[int64]bool{}}
}

func (f *fakeMarlin) run(t *testing.T) {
	t.Helper()
	r := bufio.NewReader(f.conn)
	// Greet like a freshly (re)booted Marlin.
	fmt.Fprint(f.conn, "start\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		f.mu.Lock()
		f.seenLines = append(f.seenLines, line)
		f.mu.Unlock()

		body, n, hasN := stripLineNumber(line)
		cmdText, _ := stripChecksum(body)

		f.mu.Lock()
		uploading := f.uploading
		f.mu.Unlock()
		if uploading {
			if strings.HasPrefix(strings.TrimSpace(cmdText), "M29") {
				f.mu.Lock()
				f.uploading = false
				f.mu.Unlock()
				fmt.Fprintf(f.conn, "Done saving file.\nok\n")
				continue
			}
			f.mu.Lock()
			f.upload.WriteString(cmdText)
			f.upload.WriteByte('\n')
			f.mu.Unlock()
			fmt.Fprintf(f.conn, "ok P30 B15\n")
			continue
		}

		if hasN {
			if f.forceResend[n] {
				delete(f.forceResend, n)
				fmt.Fprintf(f.conn, "Resend: %d\n", n)
				continue
			}
		}

		trimmedCmd := strings.TrimSpace(cmdText)
		switch {
		case strings.HasPrefix(trimmedCmd, "M110"):
			fmt.Fprintf(f.conn, "ok\n")
		case strings.HasPrefix(trimmedCmd, "M28 "):
			f.mu.Lock()
			f.uploading = true
			f.uploadName = strings.TrimSpace(strings.TrimPrefix(trimmedCmd, "M28 "))
			name := f.uploadName
			f.mu.Unlock()
			fmt.Fprintf(f.conn, "Writing to file: %s\nok\n", name)
		default:
			fmt.Fprintf(f.conn, "ok N%d P30 B15\n", n)
		}
	}
}

func stripLineNumber(line string) (rest string, n int64, ok bool) {
	if !strings.HasPrefix(line, "N") {
		return line, 0, false
	}
	sp := strings.IndexByte(line, ' ')
	if sp < 0 {
		return line, 0, false
	}
	num, err := strconv.ParseInt(line[1:sp], 10, 64)
	if err != nil {
		return line, 0, false
	}
	return line[sp+1:], num, true
}

func stripChecksum(s string) (string, bool) {
	if i := strings.LastIndexByte(s, '*'); i >= 0 {
		return s[:i], true
	}
	return s, false
}

func TestDriverHandshakeAndSend(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	fm := newFakeMarlin(serverConn)
	go fm.run(t)

	d := New(clientConn, 16)
	if err := d.Handshake(500 * time.Millisecond); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	d.Start()
	defer d.Close()

	d.Send("G28")
	d.Send("G1 X10")

	deadline := time.Now().Add(2 * time.Second)
	for d.Outstanding() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d.Outstanding() != 0 {
		t.Fatalf("expected all lines acked, outstanding=%d", d.Outstanding())
	}
}

func TestDriverResendRecovery(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	fm := newFakeMarlin(serverConn)
	fm.forceResend[2] = true // force Marlin to ask for line 2 (G1 X20) once
	go fm.run(t)

	d := New(clientConn, 16)
	if err := d.Handshake(500 * time.Millisecond); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	d.Start()
	defer d.Close()

	d.Send("G1 X10") // N1
	d.Send("G1 X20") // N2 -- will be Resend'd once
	d.Send("G1 X30") // N3

	deadline := time.Now().Add(2 * time.Second)
	for d.Outstanding() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d.Outstanding() != 0 {
		t.Fatalf("expected recovery to ack everything, outstanding=%d", d.Outstanding())
	}
}

func TestDriverASCIIUploadCapturesBytes(t *testing.T) {
	dir := t.TempDir()
	names := NewNameMap(dir + "/names.json")

	serverConn, clientConn := net.Pipe()
	fm := newFakeMarlin(serverConn)
	go fm.run(t)

	d := New(clientConn, 16)
	if err := d.Handshake(500 * time.Millisecond); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	d.Start()
	defer d.Close()

	// Write a small gcode file for uploadASCII to read.
	path := dir + "/test.gcode"
	content := "G28\nG1 X1 Y1\n; a comment\nG1 X2 Y2\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if err := d.uploadASCII(context.Background(), path, names.Assign("test.gcode"), int64(len(content)), nil); err != nil {
		t.Fatalf("uploadASCII: %v", err)
	}

	// Wait for the fake Marlin to have actually seen the M29 that ends the
	// upload (not just for Driver's queue to look momentarily empty, which
	// can race with the send-pump goroutine before it's even scheduled).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fm.mu.Lock()
		done := fm.uploadName != "" && !fm.uploading
		fm.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	fm.mu.Lock()
	got := fm.upload.String()
	fm.mu.Unlock()
	want := "G28\nG1 X1 Y1\nG1 X2 Y2\n" // comment-only line dropped, per stripComment
	if got != want {
		t.Fatalf("uploaded content = %q, want %q", got, want)
	}
}
