package printer

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"forge/internal/gcode"
)

// Conn is what Driver needs from a transport: the printer package doesn't
// care whether it's a real serial.Port or a test double (pty, net.Pipe).
type Conn interface {
	io.ReadWriteCloser
}

// Driver owns one serial connection to Marlin: it frames/sends G-code
// through a windowed queue (keeping ADVANCED_OK's buffer full), routes
// emergency commands around the queue, and parses temperature/SD-status
// autoreports and resend/error chatter out of the incoming stream.
//
// Low goroutine count by design: exactly two goroutines per connection
// (one blocking read loop, one send-pump loop driven by a channel), as
// required for the target box's tight RAM/CPU budget.
type Driver struct {
	conn   Conn
	window *gcode.Window

	mu        sync.Mutex
	temps     Temps
	sdStatus  SDStatus
	caps      map[string]bool
	lastLines []string // small ring buffer for the UI console
	onLine    func(string)

	sdFiles    []SDFile
	inFileList bool
	names      *NameMap // optional: resolves short->long for listing display

	sendCh   chan sendReq
	closed   chan struct{}
	closeErr error
}

type sendReq struct {
	cmd  string
	done chan error // optional: signaled once queued (not once acked)
}

const consoleBacklog = 100

// New wraps conn with framing/queueing. bufsize should be Marlin's
// BUFSIZE (16 for this firmware build) as the outstanding-line budget.
func New(conn Conn, bufsize int) *Driver {
	d := &Driver{
		conn:   conn,
		window: gcode.NewWindow(bufsize),
		caps:   map[string]bool{},
		sendCh: make(chan sendReq, 256),
		closed: make(chan struct{}),
	}
	return d
}

// OnLine registers a callback invoked for every raw line received, for the
// UI's live console. It must not block.
func (d *Driver) OnLine(f func(string)) { d.onLine = f }

// SetNames wires in the long/short name map so SD listings can show
// original long filenames instead of just 8.3 short names.
func (d *Driver) SetNames(n *NameMap) { d.names = n }

// SDFiles returns the most recent M20 L listing.
func (d *Driver) SDFiles() []SDFile {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]SDFile, len(d.sdFiles))
	copy(out, d.sdFiles)
	return out
}

// Start launches the read and send-pump goroutines. Call Handshake first.
func (d *Driver) Start() {
	go d.readLoop()
	go d.pumpLoop()
}

// Close stops the driver and closes the underlying connection.
func (d *Driver) Close() error {
	select {
	case <-d.closed:
	default:
		close(d.closed)
	}
	return d.conn.Close()
}

// Handshake waits (briefly) for Marlin's "start" banner after a board reset
// (CH340 DTR toggling commonly resets the MCU on open), falling back to
// assuming the board was already running if nothing arrives, then sends
// M110 N0 to (re)synchronize line numbering.
func (d *Driver) Handshake(timeout time.Duration) error {
	r := bufio.NewReader(d.conn)
	deadline := time.Now().Add(timeout)
	sawStart := false
	for time.Now().Before(deadline) {
		if dl, ok := d.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
			_ = dl.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		}
		line, err := r.ReadString('\n')
		if line != "" {
			resp := gcode.ParseLine(line)
			if resp.Kind == gcode.KindStart {
				sawStart = true
				break
			}
		}
		if err != nil {
			continue
		}
	}
	_ = sawStart // informational only; we sync either way
	d.window.Reset(1)
	if _, err := d.conn.Write([]byte("N1 M110 N0*")); err != nil {
		// best effort; fall through to the checksummed path below instead
	}
	// Send a properly framed M110 N0 (resets Marlin's expected line number
	// to 0 so the very next command is N1).
	frame := gcode.FrameLine(1, "M110 N0")
	_, err := d.conn.Write([]byte(frame))
	return err
}

// SendEmergency writes an EMERGENCY_PARSER command (M112/M108/M410/M876)
// directly to the wire, bypassing line numbering and the send window
// entirely, matching how Marlin's interrupt-driven parser expects them.
func (d *Driver) SendEmergency(cmd string) error {
	_, err := d.conn.Write([]byte(strings.TrimSpace(cmd) + "\n"))
	return err
}

// Send queues a normal command for windowed, checksummed delivery. It
// returns once the command has been accepted into the queue, not once
// Marlin has acked it.
func (d *Driver) Send(cmd string) {
	if gcode.IsEmergency(cmd) {
		if err := d.SendEmergency(cmd); err != nil {
			log.Printf("forge: emergency send error: %v", err)
		}
		return
	}
	select {
	case d.sendCh <- sendReq{cmd: cmd}:
	case <-d.closed:
	}
}

func (d *Driver) pumpLoop() {
	var pending []string
	for {
		select {
		case <-d.closed:
			return
		case req := <-d.sendCh:
			pending = append(pending, req.cmd)
		case <-time.After(10 * time.Millisecond):
			// periodic drain tick; avoids a busy loop while still keeping
			// the window full promptly after each ack (readLoop also pokes
			// via retryResends below through the same channel mechanism)
		}
		for len(pending) > 0 && d.window.CanSend() {
			cmd := pending[0]
			pending = pending[1:]
			_, framed := d.window.Prepare(cmd)
			if _, err := d.conn.Write([]byte(framed)); err != nil {
				log.Printf("forge: write error: %v", err)
			}
		}
	}
}

func (d *Driver) readLoop() {
	r := bufio.NewReader(d.conn)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			d.handleLine(line)
		}
		if err != nil {
			select {
			case <-d.closed:
				return
			default:
			}
			if err == io.EOF {
				return
			}
			// Transient read errors (deadline, etc.) - keep trying.
			time.Sleep(10 * time.Millisecond)
			continue
		}
	}
}

func (d *Driver) handleLine(raw string) {
	d.recordConsole(raw)
	if d.onLine != nil {
		d.onLine(raw)
	}
	resp := gcode.ParseLine(raw)
	switch resp.Kind {
	case gcode.KindOK:
		d.window.Ack(resp)
	case gcode.KindResend:
		for _, s := range d.window.Resend(resp.Resend) {
			frame := gcode.FrameLine(s.N, s.Cmd)
			if _, err := d.conn.Write([]byte(frame)); err != nil {
				log.Printf("forge: resend write error: %v", err)
			}
		}
	case gcode.KindError:
		log.Printf("forge: printer error: %s", resp.Message)
	}

	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "Begin file list") {
		d.mu.Lock()
		d.inFileList = true
		d.sdFiles = d.sdFiles[:0]
		d.mu.Unlock()
	} else if strings.HasPrefix(trimmed, "End file list") {
		d.mu.Lock()
		d.inFileList = false
		d.mu.Unlock()
	} else {
		d.mu.Lock()
		inList := d.inFileList
		d.mu.Unlock()
		if inList {
			if f, ok := ParseFileListLine(raw); ok {
				if f.Long == "" && d.names != nil {
					f.Long = d.names.LongFor(f.Short)
				}
				d.mu.Lock()
				d.sdFiles = append(d.sdFiles, f)
				d.mu.Unlock()
			}
		}
	}

	// Autoreports and other chatter aren't part of the ok/resend protocol
	// and can appear on any line, so they're parsed independent of Kind.
	if t, ok := ParseTemps(raw); ok {
		d.mu.Lock()
		t.UpdatedAt = time.Now()
		d.temps = t
		d.mu.Unlock()
	}
	if s, ok := ParseSDStatus(raw); ok {
		d.mu.Lock()
		d.sdStatus = s
		d.mu.Unlock()
	}
	if name, enabled, ok := ParseCapability(raw); ok {
		d.mu.Lock()
		d.caps[name] = enabled
		d.mu.Unlock()
	}
}

func (d *Driver) recordConsole(line string) {
	line = strings.TrimRight(line, "\r\n")
	d.mu.Lock()
	d.lastLines = append(d.lastLines, line)
	if len(d.lastLines) > consoleBacklog {
		d.lastLines = d.lastLines[len(d.lastLines)-consoleBacklog:]
	}
	d.mu.Unlock()
}

// Console returns the last ~100 received lines for the UI.
func (d *Driver) Console() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.lastLines))
	copy(out, d.lastLines)
	return out
}

// Temps returns the last known temperature reading.
func (d *Driver) Temps() Temps {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.temps
}

// SDStatus returns the last known M27 status.
func (d *Driver) SDStatus() SDStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sdStatus
}

// Capabilities returns the M115 Cap: flags seen so far.
func (d *Driver) Capabilities() map[string]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]bool, len(d.caps))
	for k, v := range d.caps {
		out[k] = v
	}
	return out
}

// Outstanding reports how many sent lines are awaiting "ok", useful for
// tests and diagnostics.
func (d *Driver) Outstanding() int { return d.window.Outstanding() }

// WriteRaw is an escape hatch for protocols (binary transfer) that need to
// take over the wire directly; callers must ensure no G-code is in flight.
func (d *Driver) WriteRaw() io.ReadWriter { return d.conn }

var _ = fmt.Sprintf // keep fmt import if unused branches trimmed later
