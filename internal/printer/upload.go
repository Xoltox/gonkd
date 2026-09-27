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
	"time"

	"github.com/Xoltox/gonkd/internal/binprotocol"
	"github.com/Xoltox/gonkd/internal/gcode"
	"github.com/Xoltox/gonkd/internal/heatshrink"
)

// UploadProtocol selects how UploadToSD moves a file to the card.
type UploadProtocol string

const (
	ProtoASCII  UploadProtocol = "ascii"  // M28 <name> ... M29, numbered and checksummed lines
	ProtoBinary UploadProtocol = "binary" // Marlin BINARY_FILE_TRANSFER ("M28 B1")
	ProtoAuto   UploadProtocol = "auto"   // binary, ASCII if no binary session starts
)

// UploadProto is the protocol UploadToSD uses; set once at startup
// (-upload-proto). ASCII until binary is verified on the printer.
var UploadProto = ProtoASCII

// ParseUploadProtocol validates an -upload-proto value.
func ParseUploadProtocol(s string) (UploadProtocol, error) {
	switch p := UploadProtocol(strings.ToLower(strings.TrimSpace(s))); p {
	case ProtoASCII, ProtoBinary, ProtoAuto:
		return p, nil
	}
	return "", fmt.Errorf("unknown upload protocol %q (want ascii, binary or auto)", s)
}

// ProgressFunc is called periodically during upload/stream with 0..100.
type ProgressFunc func(sentBytes, totalBytes int64)

const (
	uploadMaxLine  = 1 << 20          // longest source line accepted (slicer config comments can be long)
	m28Timeout     = 30 * time.Second // M28 must answer "Writing to file" or "open failed" by then
	listingTimeout = 10 * time.Second // fresh M20 listing before picking a short name
	closeTimeout   = 15 * time.Second // M29 after a cancel or failure
)

// drainProbeAfter: quiet this long after M29, probe with M105. A variable
// so tests can shorten it.
var drainProbeAfter = 10 * time.Second

// UploadToSD transfers localPath to the printer's SD card under an 8.3 name
// derived from longName and returns that short name. It holds the port
// exclusively: every other Send and RefreshFiles gets ErrBusy until it
// returns (emergency commands still go through). It returns only after
// every file line and the closing M29 are acked, i.e. the file is closed on
// the card. On ctx cancel it stops, sends M29 (and M30 to delete the
// partial file) and returns ctx.Err(). On any failure the name mapping is
// forgotten.
//
// The caller is responsible for deleting localPath afterward and for
// issuing M23/M24 if the file should print immediately.
func (d *Driver) UploadToSD(ctx context.Context, localPath, longName string, names *NameMap, progress ProgressFunc) (string, error) {
	if names == nil {
		return "", errors.New("upload: no name map")
	}
	info, err := os.Stat(localPath)
	if err != nil {
		return "", err
	}
	size := info.Size()

	gen, err := d.beginUpload()
	if err != nil {
		return "", err
	}
	defer d.endUpload()

	// Pick the short name against a fresh listing so a file already on the
	// card (not created by gonkd) is never overwritten by M28.
	if err := d.refreshListing(ctx, gen); err != nil {
		if ctx.Err() != nil || errors.Is(err, errPrinterReset) || errors.Is(err, ErrClosed) {
			return "", err
		}
		log.Printf("gonkd: upload: SD listing not refreshed (%v), using the last one", err)
	}
	short := d.assignShort(names, longName)

	start := time.Now()
	used, detail := ProtoASCII, ""
	if UploadProto == ProtoBinary || UploadProto == ProtoAuto {
		used = ProtoBinary
		var st binStats
		st, err = d.uploadBinary(ctx, gen, localPath, short, progress)
		detail = fmt.Sprintf(", %d bytes on the wire, %d resends", st.Wire, st.Resends)
		if st.Compressed {
			detail += ", heatshrink"
		}
		if err != nil && UploadProto == ProtoAuto && st.CanFallBack && ctx.Err() == nil && d.gen.Load() == gen {
			log.Printf("gonkd: upload %s: binary transfer did not start (%v), using ASCII", short, err)
			used, detail = ProtoASCII, ""
			start = time.Now()
			err = d.uploadASCII(ctx, gen, localPath, short, size, progress)
		}
	} else {
		err = d.uploadASCII(ctx, gen, localPath, short, size, progress)
	}
	if err != nil {
		log.Printf("gonkd: upload %s via %s failed after %.1fs: %v", short, used, time.Since(start).Seconds(), err)
		names.Forget(short)
		return "", err
	}
	dur := time.Since(start)
	log.Printf("gonkd: upload %s via %s: %d bytes in %.1fs, %.1f KB/s%s",
		short, used, size, dur.Seconds(), float64(size)/1000/max(dur.Seconds(), 0.001), detail)
	return short, nil
}

func (d *Driver) beginUpload() (uint64, error) {
	d.qmu.Lock()
	defer d.qmu.Unlock()
	select {
	case <-d.dead:
		return 0, ErrClosed
	default:
	}
	if d.uploading.Load() {
		return 0, ErrBusy
	}
	d.uploading.Store(true)
	d.writeErr.Store(false)
	return d.gen.Load(), nil
}

func (d *Driver) endUpload() {
	d.qmu.Lock()
	d.uploading.Store(false)
	d.m28.Store(m28Idle)
	d.qmu.Unlock()
}

// sendOwned queues one of the upload's own lines.
func (d *Driver) sendOwned(ctx context.Context, gen uint64, cmd string) error {
	return d.enqueue(ctx, cmd, true, gen, 0)
}

// waitFor blocks until cond is true, re-checking on every driver state
// change (ack, dequeue, listing, reset). It fails on printer reset, driver
// death, ctx, or after timeout (0 = none) with ErrTimeout.
func (d *Driver) waitFor(ctx context.Context, gen uint64, timeout time.Duration, cond func() bool) error {
	var expired <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		expired = t.C
	}
	for {
		wake := d.changed.wait()
		if d.gen.Load() != gen {
			return errPrinterReset
		}
		if cond() {
			return nil
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

// drained reports that every queued line has been sent and acked.
func (d *Driver) drained() bool {
	return d.queued.Load() == 0 && d.window.Outstanding() == 0
}

func (d *Driver) refreshListing(ctx context.Context, gen uint64) error {
	seq := d.listSeq.Load()
	if err := d.sendOwned(ctx, gen, "M20 L"); err != nil {
		return err
	}
	return d.waitFor(ctx, gen, listingTimeout, func() bool { return d.listSeq.Load() != seq })
}

// assignShort picks the 8.3 name, avoiding both names.json entries and the
// files in the current SD listing.
func (d *Driver) assignShort(names *NameMap, longName string) string {
	onCard := map[string]bool{}
	for _, f := range d.SDFiles() {
		onCard[strings.ToUpper(f.Short)] = true
	}
	return names.AssignAvoiding(longName, func(s string) bool { return onCard[strings.ToUpper(s)] })
}

// uploadASCII streams the file with M28 <name> / M29 through the normal
// windowed queue, so Marlin's acks pace it exactly as they do a live print.
// Marlin writes every line it receives into the open file, which is why
// the upload must own the port.
func (d *Driver) uploadASCII(ctx context.Context, gen uint64, localPath, short string, size int64, progress ProgressFunc) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// Open the file and make sure Marlin really did: after a failed M28
	// every following line would run as live G-code.
	d.m28.Store(m28Waiting)
	if err := d.sendOwned(ctx, gen, "M28 "+short); err != nil {
		return err
	}
	if err := d.waitFor(ctx, gen, m28Timeout, func() bool { return d.m28.Load() != m28Waiting }); err != nil {
		d.abortUpload(gen, short)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("M28 %s: %w", short, err)
	}
	if d.m28.Load() != m28Opened {
		return fmt.Errorf("M28 %s: printer could not open the file", short)
	}

	var sent, reported int64
	lineNo := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), uploadMaxLine)
	for scanner.Scan() {
		line := scanner.Text()
		lineNo++
		sent += int64(len(line)) + 1
		if d.metaFeed != nil {
			d.metaFeed(line)
		}
		cmd := stripComment(line)
		if cmd == "" || isSaveControl(cmd) || hasM110(cmd) {
			continue
		}
		if !d.fits(cmd) {
			// Marlin would truncate it, checksum included, and ask for it
			// again forever with the file open.
			d.abortUpload(gen, short)
			return fmt.Errorf("G-code line %d is too long for the printer: %d bytes after comment removal, framed line would exceed Marlin's %d-byte limit",
				lineNo, len(cmd), gcode.MaxFramedLen)
		}
		if err := d.sendOwned(ctx, gen, cmd); err != nil {
			if ctx.Err() != nil {
				d.abortUpload(gen, short)
				return ctx.Err()
			}
			return err
		}
		if progress != nil && sent-reported >= 4096 {
			reported = sent
			progress(sent, size)
		}
	}
	if err := scanner.Err(); err != nil {
		d.abortUpload(gen, short)
		return fmt.Errorf("read %s: %w", localPath, err)
	}

	if err := d.sendOwned(ctx, gen, "M29"); err != nil {
		if ctx.Err() != nil {
			d.abortUpload(gen, short)
			return ctx.Err()
		}
		return err
	}
	if err := d.waitDrained(ctx, gen); err != nil {
		if ctx.Err() != nil {
			d.abortUpload(gen, short) // M29 is already queued; this deletes the file
			return ctx.Err()
		}
		return err
	}
	if d.writeErr.Load() {
		return fmt.Errorf("printer reported an SD write error for %s", short)
	}
	if progress != nil {
		progress(size, size)
	}
	return nil
}

// waitDrained waits until the M29 (and everything before it) is acked. A
// lost final "ok" would otherwise hang here forever, because Marlin only
// asks for a resend when a later line arrives: after drainProbeAfter of
// silence an M105 is sent. Its "ok T:..." acks that M105 and everything
// before it (gcode.Window.Ack), and if the M29 itself was lost, its later
// line number makes Marlin request the M29 again. (The file is closed by
// then, so M105 never lands in it.)
func (d *Driver) waitDrained(ctx context.Context, gen uint64) error {
	for {
		err := d.waitFor(ctx, gen, drainProbeAfter, d.drained)
		if err != ErrTimeout {
			return err
		}
		log.Printf("gonkd: upload: no ack for the final lines, probing with M105")
		if err := d.sendOwned(ctx, gen, "M105"); err != nil {
			return err
		}
	}
}

// WaitDrained waits until every line queued so far has been sent and acked
// (the send queue is empty and nothing is outstanding in the window),
// probing with M105 if nothing acks for a while so a lost final "ok" cannot
// hang it forever. It returns once ctx is done, the driver dies, or a
// printer reset changes the generation. The stream feeder uses it to know
// every line has truly been printed, not just handed to Send, before
// marking the job complete.
func (d *Driver) WaitDrained(ctx context.Context) error {
	return d.waitDrained(ctx, d.gen.Load())
}

// abortUpload closes a partial upload: M29 so Marlin leaves saving mode,
// then M30 to delete the partial file. Best effort and time-limited; after
// a printer reset (gen changed) there is nothing to close.
func (d *Driver) abortUpload(gen uint64, short string) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	if d.sendOwned(ctx, gen, "M29") != nil {
		return
	}
	if d.waitFor(ctx, gen, 0, d.drained) != nil {
		return
	}
	_ = d.sendOwned(ctx, gen, "M30 "+short)
}

// isSaveControl reports M28/M29 lines inside a G-code file: sent during an
// upload they would end the save early (and the rest of the file would run
// live) or nest it, so they are never forwarded.
func isSaveControl(cmd string) bool {
	w := strings.ToUpper(firstField(cmd))
	return w == "M28" || w == "M29"
}

func firstField(s string) string {
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// Binary transfer tuning; variables so tests can shorten them.
var (
	binTimeout     = time.Second      // reply timeout before a resend (reference client: 1 s)
	binOpenTimeout = 10 * time.Second // OPEN/CLOSE answer: Marlin mounts the card, creates or flushes the file
	binSwitchWait  = 5 * time.Second  // ok for "M28 B1" after "Switching to Binary Protocol"
	binWindow      = 4                // WRITE packets in flight: 4 x 106 bytes, far below RX_BUFFER_SIZE 2048
	binCompress    = true             // heatshrink when Marlin offers it
)

// binStats describes a binary upload for the summary log line.
type binStats struct {
	Wire        int64
	Resends     int
	Compressed  bool
	CanFallBack bool // failed before any file was created, printer back in ASCII mode
}

// uploadBinary sends the file with Marlin's BINARY_FILE_TRANSFER protocol
// (internal/binprotocol). The card gets the same bytes as with ASCII:
// every line comment-stripped and filtered exactly like uploadASCII, each
// ended by "\n", and metaFeed still sees every raw line.
//
// Sequence: all ASCII acked; "M28 B1" and its ok; readLoop hands replies
// to the session and the pump stays silent (enterBinary); SYNC, QUERY,
// OPEN, WRITE..., CLOSE, CONTROL CLOSE; a bare newline and the pump
// resumes (leaveBinary). Any failure after OPEN sends ABORT (the partial
// file is deleted) and CONTROL CLOSE, so Marlin is left in ASCII mode with
// no file open; if even that gets no answer, Marlin's own 10 s transfer
// timeout closes and deletes the file.
func (d *Driver) uploadBinary(ctx context.Context, gen uint64, localPath, short string, progress ProgressFunc) (st binStats, err error) {
	st.CanFallBack = true
	if !d.Capabilities()["BINARY_FILE_TRANSFER"] {
		return st, errors.New("printer does not report Cap:BINARY_FILE_TRANSFER")
	}
	f, err := os.Open(localPath)
	if err != nil {
		return st, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return st, err
	}
	size := info.Size()

	// Nothing ASCII may be in flight: after M28 B1 Marlin reads packets.
	if err := d.waitFor(ctx, gen, m28Timeout, d.drained); err != nil {
		return st, err
	}
	d.binSwitched.Store(false)
	d.m28.Store(m28Waiting)
	err = d.sendOwned(ctx, gen, "M28 B1")
	if err == nil {
		err = d.waitFor(ctx, gen, m28Timeout, func() bool {
			return d.binSwitched.Load() || d.m28.Load() != m28Waiting || d.drained()
		})
	}
	opened := d.m28.Swap(m28Idle) == m28Opened
	switch {
	case err != nil && !d.binSwitched.Load():
		return st, fmt.Errorf("M28 B1: %w", err)
	case opened:
		// A firmware without binary transfer takes "B1" as a file name
		// and is now saving; close and delete it.
		d.abortUpload(gen, "B1")
		return st, errors.New("printer opened a file for M28 B1 instead of switching to binary mode")
	case !d.binSwitched.Load():
		return st, errors.New("printer did not switch to binary mode")
	}
	// "Switching" comes before the ok; let the ok arrive so the ASCII
	// window is empty. If it was lost, the stall replay settles it once
	// the port is back in ASCII mode (Marlin has the line already).
	_ = d.waitFor(ctx, gen, binSwitchWait, d.drained)

	// From here on Marlin reads packets: every exit path must get it back
	// to ASCII (Exit) before leaveBinary lets the pump write again.
	st.CanFallBack = false
	if err := d.enterBinary(gen); err != nil {
		return st, err
	}
	defer d.leaveBinary(gen)
	sctx, cancel := d.linkCtx(ctx, 0)
	defer cancel()

	var sess *binprotocol.Session
	exit := func() {
		if sess == nil || d.gen.Load() != gen {
			return
		}
		ectx, cancel := d.linkCtx(context.Background(), closeTimeout)
		defer cancel()
		if err := sess.Exit(ectx); err != nil {
			log.Printf("gonkd: binary transfer: leaving binary mode: %v (the printer may still be in binary mode)", err)
		}
	}
	defer func() {
		if sess != nil {
			s := sess.Stats()
			st.Wire, st.Resends = s.Wire, s.Resends
		}
	}()

	// Not cancelled by ctx: once Marlin has switched, only a synced session
	// can bring it back to ASCII (Start is bounded by its own timeouts).
	startCtx, startCancel := d.linkCtx(context.Background(), 0)
	defer startCancel()
	sess, err = binprotocol.Start(startCtx, binprotocol.Config{
		Write:       func(p []byte) error { return d.writeBinary(gen, p) },
		Lines:       d.binLines,
		Timeout:     binTimeout,
		OpenTimeout: binOpenTimeout,
		Window:      binWindow,
	})
	if err != nil {
		// Without an "ss" the stream sync is unknown and no CONTROL
		// CLOSE can be addressed: no fallback, the printer may still be
		// in binary mode.
		if sess != nil {
			exit()
			st.CanFallBack = d.gen.Load() == gen
		}
		return st, fmt.Errorf("binary session: %w", err)
	}

	var enc *heatshrink.Encoder
	pw := &packetWriter{ctx: sctx, s: sess}
	var sink io.Writer = pw
	if w, l, ok := sess.Heatshrink(); ok && binCompress {
		if e, err := heatshrink.NewEncoder(pw, heatshrink.Config{WindowBits: uint(w), LookaheadBits: uint(l)}); err == nil {
			enc, sink = e, e
			st.Compressed = true
		} else {
			log.Printf("gonkd: binary transfer: %v, sending uncompressed", err)
		}
	}
	if ctx.Err() != nil {
		exit()
		return st, ctx.Err()
	}
	if err := sess.Open(sctx, short, enc != nil); err != nil {
		// If OPEN was processed but its answer lost, the file is open and
		// Marlin is saving: after CONTROL CLOSE every ASCII line would be
		// written into it (queue.cpp, card.flag.saving). CLOSE ends that and
		// is harmless otherwise ("PFT:invalid"); ABORT is not (it deletes
		// card.filename, which may be another file).
		if d.gen.Load() == gen {
			cctx, cancel := d.linkCtx(context.Background(), closeTimeout)
			_ = sess.Close(cctx)
			cancel()
		}
		exit()
		st.CanFallBack = d.gen.Load() == gen
		return st, err
	}

	fail := func(err error) (binStats, error) {
		if d.gen.Load() == gen {
			actx, cancel := d.linkCtx(context.Background(), closeTimeout)
			if aerr := sess.Abort(actx); aerr != nil {
				log.Printf("gonkd: binary transfer: abort: %v", aerr)
			}
			cancel()
			exit()
		}
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
		return st, err
	}

	var sent, reported int64
	lineNo := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4096), uploadMaxLine)
	for scanner.Scan() {
		if err := sctx.Err(); err != nil {
			return fail(err)
		}
		line := scanner.Text()
		lineNo++
		sent += int64(len(line)) + 1
		if d.metaFeed != nil {
			d.metaFeed(line)
		}
		cmd := stripComment(line)
		if cmd == "" || isSaveControl(cmd) || hasM110(cmd) {
			continue
		}
		if len(cmd) > gcode.MaxFramedLen {
			// Marlin would cut it short when printing from the card.
			return fail(fmt.Errorf("G-code line %d is too long for the printer: %d bytes after comment removal, Marlin keeps %d",
				lineNo, len(cmd), gcode.MaxFramedLen))
		}
		if _, err := io.WriteString(sink, cmd+"\n"); err != nil {
			return fail(err)
		}
		if progress != nil && sent-reported >= 4096 {
			reported = sent
			progress(sent, size)
		}
	}
	if err := scanner.Err(); err != nil {
		return fail(fmt.Errorf("read %s: %w", localPath, err))
	}
	if enc != nil {
		if err := enc.Close(); err != nil {
			return fail(err)
		}
	}
	if err := pw.flush(); err != nil {
		return fail(err)
	}
	if err := sess.Close(sctx); err != nil {
		return fail(err)
	}
	exit()
	if progress != nil {
		progress(size, size)
	}
	return st, nil
}

// packetWriter cuts a byte stream into WRITE packets of the session's
// maximum payload.
type packetWriter struct {
	ctx context.Context
	s   *binprotocol.Session
	buf []byte
}

func (p *packetWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	max := p.s.MaxPayload()
	if k := len(p.buf) / max * max; k > 0 {
		if err := p.s.Write(p.ctx, p.buf[:k]); err != nil {
			return 0, err
		}
		p.buf = append(p.buf[:0], p.buf[k:]...)
	}
	return len(b), nil
}

func (p *packetWriter) flush() error {
	if len(p.buf) == 0 {
		return nil
	}
	err := p.s.Write(p.ctx, p.buf)
	p.buf = p.buf[:0]
	return err
}

// stripComment prepares one source line for the wire so that Marlin stores
// exactly the bytes gonkd checksums: a ';' comment is cut off for every
// command, M117/M118 included (Marlin's process_stream_char drops it at
// receive time, and the checksum with it: this build has no
// GCODE_QUOTED_STRINGS), then backslashes (Marlin's escape character,
// dropped on receipt) and control bytes other than tab are removed, and
// whitespace is trimmed. Parentheses are left alone (no PAREN_COMMENTS).
// Note the SD copy of an uploaded file is therefore comment-stripped.
func stripComment(line string) string {
	line = gcode.CutComment(line)
	if !gcode.WireSafe(line) {
		// Byte-wise, so invalid UTF-8 in the file passes through unchanged.
		b := make([]byte, 0, len(line))
		for i := 0; i < len(line); i++ {
			if c := line[i]; c != '\\' && (c >= 0x20 || c == '\t') {
				b = append(b, c)
			}
		}
		line = string(b)
	}
	return strings.TrimSpace(line)
}
