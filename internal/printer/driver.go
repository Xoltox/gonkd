package printer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"forge/internal/gcode"
)

var (
	// ErrInvalidCommand: empty (after cutting a ';' comment), contains a
	// byte Marlin would not store as sent (CR, LF, NUL, other control
	// bytes, backslash), an M110 (the Driver owns line numbering), or too
	// long to frame within Marlin's MAX_CMD_SIZE.
	ErrInvalidCommand = errors.New("invalid command")
	ErrBusy           = errors.New("printer busy: SD upload in progress")
	ErrClosed         = errors.New("printer connection closed")
	ErrTimeout        = errors.New("printer not accepting commands")

	errPrinterReset = errors.New("printer reset during upload")
)

// Event is something the firmware reported that the job layer must react
// to. Delivered through the OnEvent callback.
type Event int

const (
	// EventPrinterReset: "start" seen after Handshake. The Driver has
	// already reset line numbers (M110 N0), dropped every queued line and
	// re-sent the init commands.
	EventPrinterReset Event = iota + 1
	EventDonePrinting       // "Done printing file"
	EventOpenFailed         // "open failed" (M23 could not open the file)
	EventHalted             // "Error:Printer halted" or "Printer stopped due to errors"
)

// Conn is what Driver needs from a transport: the printer package doesn't
// care whether it's a real serial.Port or a test double (net.Pipe).
type Conn interface {
	io.ReadWriteCloser
}

const (
	consoleBacklog = 100
	maxLineLen     = 4096            // longer received lines are discarded
	queueLen       = 32              // bounded send queue in front of the window
	sendTimeout    = 5 * time.Second // Send gives up waiting for queue room

	stallMax = 60 * time.Second // ack-stall replay backoff cap
)

// stallAfter: lines outstanding and no ack (nor "busy:") for this long are
// replayed; repeated stalls without any ack back off up to stallMax. A
// variable so tests can shorten it.
var stallAfter = 5 * time.Second

// resendQuiet is how long no further "Resend:" must arrive before a resend
// request is answered (see gcode.Window.NoteResend). Marlin drains its RX
// buffer on every line error (queue.cpp gcode_line_error), so bytes still
// in transit (CH341 hands them over in small USB packets) turn into
// headless fragments that draw one more error and drain each; a replay
// written into that burst would be eaten by it. 250000 baud moves a full
// 16-line window in well under this.
var resendQuiet = 100 * time.Millisecond

// initCommands are sent by Start and again after a printer reset:
// capabilities, 2 s temperature and SD-status autoreports, SD listing.
var initCommands = []string{"M115", "M155 S2", "M27 S2", "M20 L"}

// Driver owns one serial connection to Marlin: it frames/sends G-code
// through a windowed queue (at most BUFSIZE unacked lines, as ADVANCED_OK
// expects), routes emergency commands around the queue, and parses
// temperature/SD-status autoreports and resend/error chatter out of the
// incoming stream.
//
// Exactly two goroutines per connection: one blocking read loop and one
// send pump (which also runs the ack-stall check). The pump only takes a line off the bounded queue when the
// window has room, so a caller feeding a file is back-pressured by Marlin
// and memory stays at queueLen+BUFSIZE lines no matter the file size.
type Driver struct {
	conn   Conn
	lr     lineReader // the only reader of conn, shared by Handshake and readLoop
	window *gcode.Window

	mu         sync.Mutex // guards the parsed state below
	temps      Temps
	sdStatus   SDStatus
	caps       map[string]bool
	lastLines  []string // small ring buffer for the UI console
	sdFiles    []SDFile
	listing    []SDFile // M20 listing being received
	inFileList bool
	listSeq    atomic.Uint64 // bumped at every "End file list"

	names   *NameMap // optional: resolves short->long for listing display
	onEvent func(Event, string)

	// wmu orders writes: a line is numbered (window.Prepare) and written
	// under it, and resend replays/resyncs hold it, so the wire always sees
	// line numbers in window order.
	wmu sync.Mutex

	// qmu guards admission into sendCh against upload ownership and resets.
	qmu       sync.Mutex
	gen       atomic.Uint64 // bumped on printer reset; older queued lines are dropped
	uploading atomic.Bool   // changed under qmu
	sendCh    chan queuedCmd
	queued    atomic.Int64  // lines in sendCh or taken but not yet numbered
	notify    chan struct{} // cap 1: window room may have appeared
	changed   broadcast     // queue room, acks, listing, reset, failure

	m28      atomic.Int32 // upload open state, see m28* constants
	writeErr atomic.Bool  // "error writing to file" seen during upload

	lastRX atomic.Int64 // unix nanos of the last complete line

	acks        atomic.Uint64 // ok lines received, for the stall backoff
	stallAfter  time.Duration // set before Start; tests shorten it
	stallCount  atomic.Uint64 // ack-stall replays done, for tests
	resendQuiet time.Duration // set before Start

	dead      chan struct{}
	failOnce  sync.Once
	err       error
	closeOnce sync.Once
	closeErr  error
}

type queuedCmd struct {
	cmd string
	gen uint64
}

const (
	m28Idle int32 = iota
	m28Waiting
	m28Opened
	m28Failed
)

// New wraps conn with framing/queueing. bufsize should be Marlin's
// BUFSIZE (16 for this firmware build) as the outstanding-line budget.
func New(conn Conn, bufsize int) *Driver {
	return &Driver{
		conn:   conn,
		lr:     lineReader{r: bufio.NewReaderSize(conn, maxLineLen)},
		window: gcode.NewWindow(bufsize),
		caps:   map[string]bool{},
		sendCh: make(chan queuedCmd, queueLen),
		notify: make(chan struct{}, 1),
		dead:   make(chan struct{}),

		stallAfter:  stallAfter,
		resendQuiet: resendQuiet,
	}
}

// SetNames wires in the long/short name map so SD listings can show
// original long filenames instead of just 8.3 short names.
func (d *Driver) SetNames(n *NameMap) { d.names = n }

// OnEvent registers the event callback. Set it before Start. It runs on
// the read goroutine: it must not block or call Send synchronously.
func (d *Driver) OnEvent(f func(ev Event, line string)) { d.onEvent = f }

// Start launches the read and send-pump goroutines, then queues the init
// commands. Call Handshake first.
func (d *Driver) Start() {
	go d.readLoop()
	go d.pumpLoop()
	for _, c := range initCommands {
		if err := d.Send(c); err != nil {
			log.Printf("forge: init %s: %v", c, err)
			return
		}
	}
}

// Close stops the driver and closes the underlying connection.
func (d *Driver) Close() error {
	d.fail(ErrClosed)
	return d.closeConn()
}

// Done is closed on a fatal read/write error (EOF, EIO) or Close.
func (d *Driver) Done() <-chan struct{} { return d.dead }

// Err returns why Done was closed, or nil while the driver is running.
func (d *Driver) Err() error {
	select {
	case <-d.dead:
		return d.err
	default:
		return nil
	}
}

// LastRX returns when the last complete line was received.
func (d *Driver) LastRX() time.Time {
	n := d.lastRX.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Uploading reports whether UploadToSD currently holds the port.
func (d *Driver) Uploading() bool { return d.uploading.Load() }

func (d *Driver) fail(err error) {
	d.failOnce.Do(func() {
		d.err = err
		close(d.dead)
		d.changed.fire()
		// Unblock a readLoop parked in Read (and a pump parked in Write).
		_ = d.closeConn()
	})
}

func (d *Driver) closeConn() error {
	d.closeOnce.Do(func() { d.closeErr = d.conn.Close() })
	return d.closeErr
}

// write puts one framed line on the wire; a failure is fatal.
func (d *Driver) write(s string) error {
	if _, err := io.WriteString(d.conn, s); err != nil {
		d.fail(fmt.Errorf("write: %w", err))
		return err
	}
	return nil
}

// Handshake waits (briefly) for Marlin's "start" banner after a board reset
// (CH340 DTR toggling commonly resets the MCU on open), falling back to
// assuming the board was already running if nothing arrives. It then sends
// "N0 M110 N0" so the next line is N1 (numbered N0 so its "ok N0" cannot
// ack a real line), and queues M29 in case a previous session left Marlin
// in M28 saving mode (harmless otherwise). The M110 is a window barrier:
// the M29 goes out after its ok (see gcode.Window).
func (d *Driver) Handshake(timeout time.Duration) error {
	dl, hasDeadline := d.conn.(interface{ SetReadDeadline(time.Time) error })
	deadline := time.Now().Add(timeout)
	for hasDeadline && time.Now().Before(deadline) {
		_ = dl.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		line, err := d.lr.readLine()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			_ = dl.SetReadDeadline(time.Time{})
			return err
		}
		d.lastRX.Store(time.Now().UnixNano())
		d.recordConsole(line)
		if gcode.ParseLine(line).Kind == gcode.KindStart {
			break
		}
		d.parseInfo(line)
	}
	// Clear the per-read deadline; left expired, every read in readLoop
	// would fail immediately and nothing would ever be received.
	if hasDeadline {
		_ = dl.SetReadDeadline(time.Time{})
	}
	d.wmu.Lock()
	d.window.Reset(0)
	_, framed := d.window.PrepareBarrier("M110 N0")
	err := d.write(framed)
	d.wmu.Unlock()
	if err != nil {
		return err
	}
	d.qmu.Lock()
	d.tryPushLocked("M29")
	d.qmu.Unlock()
	return nil
}

// cleanCommand validates one command line for Send. A ';' comment is cut
// off, as Marlin itself would at receive time (taking the checksum with
// it). Anything Marlin would not store byte for byte is rejected.
func cleanCommand(cmd string) (string, error) {
	if strings.ContainsAny(cmd, "\r\n\x00") {
		return "", ErrInvalidCommand
	}
	cmd = strings.TrimSpace(gcode.CutComment(cmd))
	if cmd == "" || !gcode.WireSafe(cmd) {
		return "", ErrInvalidCommand
	}
	return cmd, nil
}

// hasM110 reports a line Marlin would treat as M110: queue.cpp looks for
// "M110" anywhere in the line and then skips the sequence check.
func hasM110(cmd string) bool { return strings.Contains(strings.ToUpper(cmd), "M110") }

// fits reports whether cmd, framed with any line number it can still get
// (the next one plus everything queued ahead of it), stays within
// Marlin's MAX_CMD_SIZE. A longer line is truncated by Marlin, checksum
// included, and requested again forever.
func (d *Driver) fits(cmd string) bool {
	maxN := d.window.NextN() + queueLen + 2
	return gcode.FramedLenMax(maxN, cmd) <= gcode.MaxFramedLen
}

// SendEmergency writes an EMERGENCY_PARSER command (M112/M108/M410/M876)
// directly to the wire, bypassing line numbering, the send window and
// upload ownership, matching how Marlin's out-of-band parser expects them.
func (d *Driver) SendEmergency(cmd string) error {
	cmd, err := cleanCommand(cmd)
	if err != nil {
		return err
	}
	select {
	case <-d.dead:
		return ErrClosed
	default:
	}
	// One Write call per line: os.File and net.Pipe serialize Write calls,
	// so this cannot land in the middle of a framed line.
	if _, err := io.WriteString(d.conn, cmd+"\n"); err != nil {
		d.fail(fmt.Errorf("write: %w", err))
		return err
	}
	return nil
}

// Send queues one command for windowed, checksummed delivery. It returns
// once the command is queued (not acked), waiting at most sendTimeout for
// queue room (ErrTimeout). ErrBusy while an SD upload holds the port,
// ErrClosed after Done, ErrInvalidCommand on bad input. Emergency commands
// bypass the queue.
func (d *Driver) Send(cmd string) error {
	return d.send(context.Background(), cmd, sendTimeout)
}

// SendCtx is Send without the timeout: it blocks until the command is
// queued, ctx is done or the driver dies. The stream feeder uses it; the
// bounded queue back-pressures it at Marlin's pace.
func (d *Driver) SendCtx(ctx context.Context, cmd string) error {
	return d.send(ctx, cmd, 0)
}

// RefreshFiles requests a new SD listing (M20 L).
func (d *Driver) RefreshFiles() error { return d.Send("M20 L") }

func (d *Driver) send(ctx context.Context, cmd string, timeout time.Duration) error {
	cmd, err := cleanCommand(cmd)
	if err != nil {
		return err
	}
	if gcode.IsEmergency(cmd) {
		return d.SendEmergency(cmd)
	}
	if hasM110(cmd) || !d.fits(cmd) {
		return ErrInvalidCommand
	}
	return d.enqueue(ctx, cmd, false, 0, timeout)
}

// enqueue puts cmd on the bounded queue. owner marks the upload's own
// lines, which are admitted only while the upload of generation gen holds
// the port; everyone else gets ErrBusy during an upload.
func (d *Driver) enqueue(ctx context.Context, cmd string, owner bool, gen uint64, timeout time.Duration) error {
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		d.qmu.Lock()
		if err := d.admitLocked(owner, gen); err != nil {
			d.qmu.Unlock()
			return err
		}
		if d.tryPushLocked(cmd) {
			d.qmu.Unlock()
			return nil
		}
		// Register for a wakeup, then retry once so a dequeue between the
		// failed push and the registration is not missed.
		wake := d.changed.wait()
		if d.tryPushLocked(cmd) {
			d.qmu.Unlock()
			return nil
		}
		d.qmu.Unlock()

		var expired <-chan time.Time
		if timeout > 0 {
			if timer == nil {
				timer = time.NewTimer(timeout)
			}
			expired = timer.C
		}
		select {
		case <-wake:
		case <-d.dead:
			return ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		case <-expired:
			return ErrTimeout
		}
	}
}

func (d *Driver) admitLocked(owner bool, gen uint64) error {
	select {
	case <-d.dead:
		return ErrClosed
	default:
	}
	if owner {
		if !d.uploading.Load() || d.gen.Load() != gen {
			return errPrinterReset
		}
	} else if d.uploading.Load() {
		return ErrBusy
	}
	return nil
}

func (d *Driver) tryPushLocked(cmd string) bool {
	d.queued.Add(1)
	select {
	case d.sendCh <- queuedCmd{cmd: cmd, gen: d.gen.Load()}:
		return true
	default:
		d.queued.Add(-1)
		return false
	}
}

func (d *Driver) poke() {
	select {
	case d.notify <- struct{}{}:
	default:
	}
}

// pumpLoop moves lines from the queue into the window, only ever taking a
// line when the window has room. It sleeps on the notify channel (poked by
// acks and resets) while the window is full, and checks for an ack stall
// on every tick.
func (d *Driver) pumpLoop() {
	tick := d.stallAfter / 5
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	st := stallState{wait: d.stallAfter}

	var held *queuedCmd // taken off the queue, not yet numbered
	for {
		// Answer a pending resend request once Marlin has gone quiet;
		// until then resendC wakes the loop when it may be due.
		resendC := d.serviceResend()
		if held == nil && d.window.CanSend() {
			select {
			case <-d.dead:
				return
			case <-d.notify:
			case <-resendC:
			case <-ticker.C:
				d.checkStall(&st)
			case q := <-d.sendCh:
				d.changed.fire() // queue room for blocked senders
				held = &q
			}
			continue
		}
		if held != nil {
			// Re-checked under wmu: a reset or an M110 barrier may have
			// happened since the line was taken.
			d.wmu.Lock()
			var err error
			done := false
			if held.gen != d.gen.Load() {
				d.queued.Add(-1) // queued before a printer reset: drop
				done = true
			} else if d.window.CanSend() {
				_, framed := d.window.Prepare(held.cmd)
				d.queued.Add(-1)
				err = d.write(framed)
				done = true
			}
			d.wmu.Unlock()
			if err != nil {
				return
			}
			if done {
				held = nil
				continue
			}
		}
		select {
		case <-d.dead:
			return
		case <-d.notify:
		case <-resendC:
		case <-ticker.C:
			d.checkStall(&st)
		}
	}
}

// serviceResend writes the replay for a pending resend request once no
// further request arrived for resendQuiet (gcode.Window.TakeResend). It
// returns a channel that fires when a request still pending may be due,
// or nil. The replay starts with a bare newline: it ends any fragment
// left in Marlin's line buffer by a lost byte, so that fragment cannot
// swallow the first replayed line (an empty line is ignored).
func (d *Driver) serviceResend() <-chan time.Time {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	lines, resync, k, wait := d.window.TakeResend(d.resendQuiet)
	if wait > 0 {
		return time.After(wait)
	}
	if resync {
		log.Printf("forge: resend is stale, resyncing with M110 N%d", k)
	}
	if len(lines) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteByte('\n')
	for _, s := range lines {
		b.WriteString(gcode.FrameLine(s.N, s.Cmd))
	}
	_ = d.write(b.String())
	return nil
}

// stallState is the pump's ack-stall backoff.
type stallState struct {
	wait time.Duration // current threshold
	acks uint64        // ack count at the last replay
}

// checkStall is the ack-stall recovery: lines are outstanding and nothing
// was acked for st.wait although the printer was not reporting "busy:". A
// lost replay, a lost "Resend:" or an eaten line leaves Marlin waiting
// for a line and forge waiting for an ok;
// replaying every outstanding line breaks that (Marlin drops or
// re-requests what it already has). Repeated replays without any ack in
// between back off, so a genuinely slow command costs little.
func (d *Driver) checkStall(st *stallState) {
	if d.acks.Load() != st.acks {
		st.wait = d.stallAfter
	}
	if !d.window.Stalled(st.wait) {
		return
	}
	d.wmu.Lock()
	lines := d.window.Replay()
	if len(lines) > 0 {
		d.stallCount.Add(1)
		log.Printf("forge: no ack for %v, replaying %d outstanding line(s) from N%d", st.wait, len(lines), lines[0].N)
	}
	for _, s := range lines {
		if d.write(gcode.FrameLine(s.N, s.Cmd)) != nil {
			break
		}
	}
	d.wmu.Unlock()
	st.acks = d.acks.Load()
	if st.wait *= 2; st.wait > stallMax {
		st.wait = stallMax
	}
}

func (d *Driver) readLoop() {
	for {
		line, err := d.lr.readLine()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				// Nothing sets a deadline after Handshake; clear it rather
				// than spin if one is somehow left over.
				if dl, ok := d.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
					_ = dl.SetReadDeadline(time.Time{})
				}
				continue
			}
			d.fail(fmt.Errorf("read: %w", err))
			return
		}
		d.handleLine(line)
	}
}

func (d *Driver) handleLine(raw string) {
	d.lastRX.Store(time.Now().UnixNano())
	d.recordConsole(raw)
	resp := gcode.ParseLine(raw)
	switch resp.Kind {
	case gcode.KindOK:
		d.acks.Add(1)
		d.window.Ack(resp)
		d.writeReplay()
		d.poke()
		d.changed.fire()
	case gcode.KindResend:
		d.handleResend(resp.Resend)
	case gcode.KindStart:
		log.Printf("forge: printer reset detected, resyncing")
		d.resync()
		d.emit(EventPrinterReset, resp.Line)
		return
	case gcode.KindError:
		log.Printf("forge: printer error: %s", resp.Message)
	case gcode.KindOther:
		if strings.Contains(raw, "busy:") {
			// HOST_KEEPALIVE: a long command is executing, not a stall.
			d.window.Touch()
		}
	}
	d.parseInfo(raw)
}

// writeReplay writes the lines an acked resync M110 released (see
// gcode.Window.TakeReplay). CanSend stays false until they are taken, so
// no new line can overtake them.
func (d *Driver) writeReplay() {
	d.wmu.Lock()
	defer d.wmu.Unlock()
	for _, s := range d.window.TakeReplay() {
		if d.write(gcode.FrameLine(s.N, s.Cmd)) != nil {
			return
		}
	}
}

// handleResend records what Marlin asked for; the pump replays it once
// the error burst is over (serviceResend). A request for a line the window
// no longer holds (printer reset, stale numbering) is answered with an
// M110 that moves Marlin's counter to the oldest line still held; those
// lines follow once the M110 is acked (writeReplay), instead of an endless
// resend loop.
func (d *Driver) handleResend(n int64) {
	d.window.NoteResend(n)
	d.poke()
}

// resync handles a printer reset after Handshake: every queued line is
// dropped (a reset ends any upload or stream, and an upload line sent now
// would run as live G-code), Marlin's line counter is reset with
// "N0 M110 N0" and the init commands are queued again behind it (they go
// out once the M110 is acked).
func (d *Driver) resync() {
	// wmu first: the pump takes wmu (never qmu) to number a line, so no
	// line can be written between the drain and the M110.
	d.wmu.Lock()
	d.qmu.Lock()
	d.gen.Add(1)
	for drained := false; !drained; {
		select {
		case <-d.sendCh:
			d.queued.Add(-1)
		default:
			drained = true
		}
	}
	d.window.Reset(0)
	_, framed := d.window.PrepareBarrier("M110 N0")
	for _, c := range initCommands {
		d.tryPushLocked(c) // the queue was just drained: always room
	}
	d.qmu.Unlock()

	d.mu.Lock()
	d.inFileList = false
	d.listing = nil
	d.mu.Unlock()

	_ = d.write(framed)
	d.wmu.Unlock()
	d.poke()
	d.changed.fire()
}

func (d *Driver) emit(ev Event, line string) {
	if d.onEvent != nil {
		d.onEvent(ev, strings.TrimSpace(line))
	}
}

// parseInfo extracts everything that is not part of the ok/resend
// protocol: SD listings, autoreports, capabilities and firmware events.
func (d *Driver) parseInfo(raw string) {
	trimmed := strings.TrimSpace(raw)
	switch {
	case trimmed == "Begin file list":
		d.mu.Lock()
		d.inFileList = true
		d.listing = nil
		d.mu.Unlock()
		return
	case trimmed == "End file list":
		d.mu.Lock()
		if d.inFileList {
			d.sdFiles = d.listing
		}
		d.inFileList = false
		d.listing = nil
		d.mu.Unlock()
		d.listSeq.Add(1)
		d.changed.fire()
		return
	}
	d.mu.Lock()
	inList := d.inFileList
	d.mu.Unlock()
	if inList {
		if f, ok := ParseFileListLine(trimmed); ok {
			if d.names != nil {
				if l := d.names.LongFor(f.Short); l != f.Short {
					f.Long = l
				}
			}
			if f.Long == "" {
				f.Long = f.Short
			}
			d.mu.Lock()
			d.listing = append(d.listing, f)
			d.mu.Unlock()
			return
		}
	}

	if t, ok := ParseTemps(trimmed); ok {
		d.mu.Lock()
		t.UpdatedAt = time.Now()
		d.temps = t
		d.mu.Unlock()
		return
	}
	if s, ok := ParseSDStatus(trimmed); ok {
		d.mu.Lock()
		d.sdStatus = s
		d.mu.Unlock()
		return
	}
	if name, enabled, ok := ParseCapability(trimmed); ok {
		d.mu.Lock()
		d.caps[name] = enabled
		d.mu.Unlock()
		return
	}

	switch {
	case strings.HasPrefix(trimmed, "Writing to file:"):
		d.m28.CompareAndSwap(m28Waiting, m28Opened)
		d.changed.fire()
	case strings.Contains(trimmed, "open failed"):
		// During an upload's M28 this is the upload's failure, not M23's.
		if d.m28.CompareAndSwap(m28Waiting, m28Failed) {
			d.changed.fire()
		} else {
			d.emit(EventOpenFailed, trimmed)
		}
	case strings.Contains(trimmed, "error writing to file"):
		d.writeErr.Store(true)
	case strings.Contains(trimmed, "Done printing file"):
		d.emit(EventDonePrinting, trimmed)
	case strings.Contains(trimmed, "Printer halted"),
		strings.Contains(trimmed, "Printer stopped due to errors"):
		d.emit(EventHalted, trimmed)
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

// SDFiles returns the most recent complete M20 L listing.
func (d *Driver) SDFiles() []SDFile {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]SDFile, len(d.sdFiles))
	copy(out, d.sdFiles)
	return out
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

// lineReader reads '\n'-terminated lines of at most maxLineLen bytes. A
// longer line is discarded whole. Bytes read before a deadline error are
// kept for the next call rather than returned as a (partial) line.
type lineReader struct {
	r       *bufio.Reader // size maxLineLen
	partial []byte
	discard bool
}

func (l *lineReader) readLine() (string, error) {
	for {
		b, err := l.r.ReadSlice('\n')
		if err == nil {
			if l.discard {
				l.discard = false
				continue
			}
			if len(l.partial) == 0 {
				return string(b), nil
			}
			if len(l.partial)+len(b) > maxLineLen {
				l.partial = l.partial[:0]
				continue
			}
			s := string(append(l.partial, b...))
			l.partial = l.partial[:0]
			return s, nil
		}
		if !l.discard {
			l.partial = append(l.partial, b...)
			if len(l.partial) >= maxLineLen {
				l.partial = l.partial[:0]
				l.discard = true
			}
		}
		if err != bufio.ErrBufferFull {
			return "", err
		}
	}
}

// broadcast wakes every goroutine waiting on the channel from wait() when
// fire() is called. It allocates only while someone is waiting.
type broadcast struct {
	mu sync.Mutex
	ch chan struct{}
}

func (b *broadcast) wait() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ch == nil {
		b.ch = make(chan struct{})
	}
	return b.ch
}

func (b *broadcast) fire() {
	b.mu.Lock()
	if b.ch != nil {
		close(b.ch)
		b.ch = nil
	}
	b.mu.Unlock()
}
